package watch

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// minuteLayout 是消息 create_time 的分钟精度格式（FmtMinute/parseMinute 成对使用）。
const minuteLayout = "2006-01-02 15:04"

func fmtTS(epoch int64) string {
	return time.Unix(epoch, 0).Format("2006-01-02T15:04:05-07:00")
}

// FmtMinute 与消息 create_time 同格式（分钟精度，可字符串比较）。
func FmtMinute(epoch int64) string {
	return time.Unix(epoch, 0).Format(minuteLayout)
}

// parseMinute 把分钟精度时间串解析为 epoch 秒；失败返回 0。
func parseMinute(t string) int64 {
	tm, err := time.ParseInLocation(minuteLayout, t, time.Local)
	if err != nil {
		return 0
	}
	return tm.Unix()
}

// listLookback 是逐会话拉取的回看秒数：覆盖分钟精度游标边界与
// 懒初始化会话的首条消息（须 > 轮询间隔），重复消息靠 seen 去重。
const listLookback = 90

// searchLookback 是 search 兜底的回看秒数：覆盖游标精度缺口，重复靠 seen 去重。
const searchLookback = 120

func earlyStopK() int   { return envInt("LW_EARLY_STOP", 8) }
func searchEveryN() int { return envInt("LW_SEARCH_EVERY", 10) }

// restrictedReprobe 是防泄密群标记的重探间隔秒数（群可能事后关闭防泄密模式）。
func restrictedReprobe() int64 { return int64(envInt("LW_RESTRICTED_REPROBE", 86400)) }

// Poller 是实时监控循环。Out 为事件行输出（run 模式下由单写者串行化）。
// 拉取走「chat-list 活跃降序 + 逐会话增量」（不依赖搜索索引，不漏、低延迟）；
// messages-search 降为每 searchEveryN 个 tick 一次的兜底对账。
type Poller struct {
	Store        *Store
	CLI          LarkCLI
	Paths        Paths
	Interval     time.Duration
	DigestWindow int64
	DigestMax    int
	Out          func(line []byte)

	Now func() int64 // 测试注入；默认 time.Now

	tickN int
}

func (p *Poller) now() int64 {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().Unix()
}

func (p *Poller) emit(v any) { p.Out(EncodeLine(v)) }

// Run 阻塞运行直到 ctx 取消；取消时 flush 摘要缓冲。
// 返回 error 仅在不可恢复（auth 失效）时。
func (p *Poller) Run(ctx context.Context, self string) error {
	s := p.Store

	// baseline：首跑从当下开始，不涌历史（fetched 空表由懒初始化处理）
	if _, ok := s.MetaGetInt("cursor"); !ok {
		s.MetaSetInt("cursor", p.now())
	}
	if _, ok := s.MetaGetInt("last_flush"); !ok {
		s.MetaSetInt("last_flush", p.now())
	}

	// 启动夹紧：上次心跳落后过久（停机）时全部游标置为当下，
	// 积压交给 catchup，不洪泛实时 P0
	if hb, ok := s.MetaGetInt("heartbeat"); ok {
		if gap := p.now() - hb; gap > MaxGap() {
			s.ClampFetchCursors(p.now())
			s.MetaSetInt("cursor", p.now())
			p.emit(Backlog{P: "backlog", OfflineSecs: gap})
			logf("cursors clamped (offline %ds); 积压可用 catchup 拉取", gap)
		}
	}

	logf("poller ready self=%s interval=%s digest=%ds/%dmsgs early-stop=%d search-every=%d",
		self, p.Interval, p.DigestWindow, p.DigestMax, earlyStopK(), searchEveryN())

	defer logf("poller stopped")
	defer p.flushDigest()

	fails := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		nowEpoch := p.now()
		if err := p.tick(ctx, nowEpoch, self); err != nil {
			fails++
			if IsAuthError(err) {
				p.emit(NewAlert("auth", authAlertMsg(err)))
				return err
			}
			wait := int64(60) << (fails - 1)
			if wait > 600 {
				wait = 600
			}
			logf("tick failed (#%d), backoff %ds: %v", fails, wait, err)
			if fails == 10 {
				p.emit(NewAlert("api", "连续 10 次调用失败，仍在退避重试；详见 stderr"))
			}
			if sleepCtx(ctx, time.Duration(wait)*time.Second) != nil {
				return nil
			}
			continue
		}
		fails = 0
		s.MetaSetInt("heartbeat", nowEpoch)

		lastFlush, _ := s.MetaGetInt("last_flush")
		if ShouldFlush(s.DigestCount(), p.DigestMax, lastFlush, nowEpoch, p.DigestWindow) {
			p.flushDigest()
			s.MetaSetInt("last_flush", nowEpoch)
		}

		if sleepCtx(ctx, p.Interval) != nil {
			return nil
		}
	}
}

// tick 执行一轮拉取：chat-list 活跃降序遍历 + 逐会话增量，外加低频 search 兜底。
// 返回 error 仅当 chat-list 失败（单会话失败只记日志、游标不动，下 tick 重试）。
func (p *Poller) tick(ctx context.Context, nowEpoch int64, self string) error {
	s := p.Store
	chats, err := p.CLI.ChatList()
	if err != nil {
		return err
	}
	rules := LoadRulesDir(self, p.Paths.ConfigDir)

	var p0, p1 []Message
	collect := func(msgs []Message) error {
		fresh, err := s.FilterNewMessages(rules.ClassifyAll(msgs), nowEpoch, SeenMax())
		if err != nil {
			return err
		}
		for _, m := range fresh {
			if m.P == "P0" {
				p.emit(m)
				p0 = append(p0, m)
			} else {
				p1 = append(p1, m)
			}
		}
		return nil
	}

	streak := 0
	for _, ch := range chats {
		if streak >= earlyStopK() {
			break
		}
		restrictedAt, wasRestricted := s.RestrictedGet(ch.Cid)
		if wasRestricted && nowEpoch-restrictedAt < restrictedReprobe() {
			continue // 防泄密群拉取必失败，标记过期后才重探
		}
		cursor, ok := s.FetchCursor(ch.Cid)
		if !ok {
			// 懒初始化：新会话（含首启全量）从当下开始，历史归 catchup。
			// 该会话此刻的消息由下 tick 的 listLookback 回看覆盖，不漏。
			s.SetFetchCursor(ch.Cid, nowEpoch)
			continue
		}
		raw, err := p.CLI.ChatMessages(ch.Cid, fmtTS(cursor-listLookback))
		if err != nil {
			if IsAuthError(err) {
				return err
			}
			if IsRestrictedModeError(err) {
				p.markRestricted(ch, wasRestricted, nowEpoch)
				continue
			}
			logf("chat %s fetch failed (cursor kept): %v", ch.Cid, err)
			continue
		}
		if wasRestricted {
			// 重探成功：防泄密已关闭。游标夹到当下（积压不涌实时链路，对齐停机夹紧哲学）
			s.RestrictedClear(ch.Cid)
			s.SetFetchCursor(ch.Cid, nowEpoch)
			logf("chat %s (%s) restricted mode lifted, resuming", ch.Cid, ch.Name)
			continue
		}
		msgs, hasMore, err := TrimChatMessages(raw, ch.Name, ch.Mode)
		if err != nil {
			logf("chat %s parse failed: %v", ch.Cid, err)
			continue
		}
		before := len(p0) + len(p1)
		if err := collect(msgs); err != nil {
			logf("dedup failed: %v", err)
			continue
		}
		if len(p0)+len(p1) == before {
			streak++ // 去重后无新消息才算「空」，回看窗口的重复不阻碍 early-stop
		} else {
			streak = 0
		}
		// has_more：单 tick 未拉完（刷屏），游标只推进到本批最后一条，下 tick 续拉
		next := nowEpoch
		if hasMore && len(msgs) > 0 {
			if t := parseMinute(msgs[len(msgs)-1].T); t > 0 {
				next = t
			}
		}
		s.SetFetchCursor(ch.Cid, next)
	}

	// search 兜底对账：捞回 early-stop/active_time 排序理论上可能漏的，mid 去重天然合并
	if p.tickN%searchEveryN() == 0 {
		cursor, _ := s.MetaGetInt("cursor")
		raw, err := p.CLI.Search(fmtTS(cursor-searchLookback), fmtTS(nowEpoch))
		if err != nil {
			if IsAuthError(err) {
				return err
			}
			logf("search fallback failed: %v", err)
		} else if msgs, _, err := Trim(raw); err != nil {
			logf("parse search response failed: %v", err)
		} else {
			if err := collect(msgs); err != nil {
				logf("dedup failed: %v", err)
			}
			s.MetaSetInt("cursor", nowEpoch)
		}
	}
	p.tickN++

	if len(p1) > 0 {
		s.DigestAppend(p1)
	}
	// 通知命令：每 tick 的 P0 批次聚合为一次调用（避免弹窗轰炸），异步不阻塞轮询
	if len(p0) > 0 {
		if script := ReadNotifyScript(filepath.Join(p.Paths.ConfigDir, "notify")); script != "" {
			go RunNotify(ctx, script, p0)
		}
	}
	return nil
}

// markRestricted 持久标记防泄密群；仅首次检测告警（重探失败只刷新时间戳）。
func (p *Poller) markRestricted(ch ChatMeta, known bool, now int64) {
	p.Store.RestrictedSet(ch.Cid, ch.Name, now)
	logf("chat %s (%s) restricted mode, skipped (reprobe in %ds)", ch.Cid, ch.Name, restrictedReprobe())
	if !known {
		p.emit(NewAlert("restricted",
			fmt.Sprintf("群「%s」开启防泄密模式，API 无法读取消息（search 亦被屏蔽），已跳过监控", ch.Name)))
	}
}

func (p *Poller) flushDigest() {
	msgs, err := p.Store.DigestTake()
	if err != nil || len(msgs) == 0 {
		return
	}
	p.emit(BuildDigest(msgs))
}

// sleepCtx 可取消睡眠；被取消返回 ctx.Err()。
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
