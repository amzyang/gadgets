package watch

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
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

// notifyGraceSecs 是 P0 系统通知的延迟窗口：需要起草回复的 P0 先压住通知，
// 等草稿卡片（send-card）发出后再展示；窗口内未发卡（模型判定无需回复、起草
// 超时）则照常弹出兜底。<=0 恢复全部即时通知。
func notifyGraceSecs() int64 { return int64(envInt("LW_NOTIFY_GRACE", 180)) }

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

	// 上次运行遗留的延迟通知：新鲜的留给首轮 flush 照常释放，停机太久的
	// 直接丢弃——重启后弹陈旧消息只会误导（对齐游标夹紧哲学）。
	if n := s.NotifyDeferPurge(p.now() - MaxGap()); n > 0 {
		logf("dropped %d stale deferred notification(s)", n)
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
		// 到期兜底先于 tick：API 故障退避期间延迟通知也能按时释放
		p.flushDueNotify(ctx, nowEpoch)
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

	// p0 = 本 tick 全部 P0（通知批次）；p0Buf = 待聚合发射的 P0（不含音视频会议）；
	// selfLast = 本批增量里本人发言的每会话最新时间（replied 注记信号源）
	var p0, p1, p0Buf []Message
	selfLast := map[string]string{}
	// 任何返回路径（含中途 auth 失败）前先清算缓冲：collect 已把 mid 写入
	// seen，若不发射即返回，重启后被当重复过滤，消息永久丢失。
	defer func() {
		// 同会话聚合 + replied 注记后统一发射（音视频会议已在 collect 内即时单发）
		for _, ev := range MarkReplied(GroupP0(p0Buf), selfLast) {
			p.emit(ev)
		}
		if len(p1) > 0 {
			s.DigestAppend(p1)
		}
		// 通知命令：每 tick 的 P0 批次聚合为一次调用（避免弹窗轰炸），异步不阻塞轮询；
		// 本人已回复的不打扰（音视频会议豁免）。需要起草的 P0 延迟到草稿就绪后展示
		// （见 dispatchNotify）。
		if batch := notifyBatch(p0, selfLast); len(batch) > 0 {
			if script := ReadNotifyScript(filepath.Join(p.Paths.ConfigDir, "notify")); script != "" {
				p.dispatchNotify(ctx, script, batch, nowEpoch)
			}
		}
	}()
	collect := func(msgs []Message) error {
		mergeSelfLast(selfLast, SelfLastTimes(msgs, rules.Self))
		fresh, err := s.FilterNewMessages(rules.ClassifyAll(msgs), nowEpoch, SeenMax())
		if err != nil {
			return err
		}
		for _, m := range fresh {
			switch {
			case m.P != "P0":
				p1 = append(p1, m)
				continue
			case vcTypes[m.Type]:
				p.emit(m) // 音视频会议实时性最强：跳过聚合与 replied，拉到即发
			default:
				p0Buf = append(p0Buf, m)
			}
			p0 = append(p0, m)
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
	return nil
}

// GroupP0 把一个 tick 内的 P0 按会话聚合：同 cid 多条合并为一个事件
// （Msgs 按时间升序，顶层字段取最后一条作代表——send-card 的回复目标与
// 幂等键天然指向最新消息），单条保持原形状（N/Msgs 缺省，输出字节不变）。
// 事件顺序按会话首见顺序稳定输出。音视频会议不进此函数（即时单发）。
func GroupP0(msgs []Message) []Message {
	var order []string
	byCid := map[string][]Message{}
	for _, m := range msgs {
		if _, ok := byCid[m.Cid]; !ok {
			order = append(order, m.Cid)
		}
		byCid[m.Cid] = append(byCid[m.Cid], m)
	}
	out := make([]Message, 0, len(order))
	for _, cid := range order {
		group := byCid[cid]
		// 同会话可能同时来自逐会话拉取与 search 兜底，先按时间归位
		sort.SliceStable(group, func(i, j int) bool { return group[i].T < group[j].T })
		if len(group) == 1 {
			out = append(out, group[0])
			continue
		}
		rep := group[len(group)-1]
		rep.N = len(group)
		rep.Msgs = make([]P0Item, 0, len(group))
		for _, m := range group {
			rep.Msgs = append(rep.Msgs, P0Item{
				Text: m.Text, From: m.From, T: m.T, Type: m.Type, Mid: m.Mid, Fid: m.Fid,
			})
		}
		out = append(out, rep)
	}
	return out
}

// selfRepliedAfter 是「本人已回复」的唯一判据：本人同会话最新发言严格晚于
// 该消息（分钟精度下同分钟不算——宁可多提醒，不可误标漏消息）。
// MarkReplied（事件注记）与 notifyBatch（通知抑制）共用，两侧行为不分叉。
func selfRepliedAfter(selfLast map[string]string, cid, t string) bool {
	last := selfLast[cid]
	return last != "" && last > t
}

// MarkReplied 给本人已回复的事件置 replied——人已亲自处理，模型据此安静
// 跳过。聚合组以最后一条（顶层 T）为准。
func MarkReplied(events []Message, selfLast map[string]string) []Message {
	for i := range events {
		if selfRepliedAfter(selfLast, events[i].Cid, events[i].T) {
			events[i].Replied = true
		}
	}
	return events
}

// notifyBatch 从通知批次剔除本人已回复的 P0；音视频会议豁免——
// 加入会议的实时提醒不因本人发言而抑制。
func notifyBatch(p0 []Message, selfLast map[string]string) []Message {
	out := make([]Message, 0, len(p0))
	for _, m := range p0 {
		if !vcTypes[m.Type] && selfRepliedAfter(selfLast, m.Cid, m.T) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// dispatchNotify 分流通知批次：音视频会议即时弹（跳过起草，无草稿可等）；
// 其余 P0 入延迟队列，由 send-card 认领（草稿就绪即通知）或 notifyGraceSecs
// 超时兜底（flushDueNotify）。延迟入库失败退回即时通知（宁可早弹不可漏弹）。
func (p *Poller) dispatchNotify(ctx context.Context, script string, batch []Message, now int64) {
	var immediate, deferred []Message
	for _, m := range batch {
		if vcTypes[m.Type] {
			immediate = append(immediate, m)
		} else {
			deferred = append(deferred, m)
		}
	}
	if notifyGraceSecs() <= 0 {
		immediate, deferred = batch, nil
	}
	if len(deferred) > 0 {
		if err := p.Store.NotifyDeferPut(deferred, now+notifyGraceSecs()); err != nil {
			logf("notify defer failed, notifying now: %v", err)
			immediate = append(immediate, deferred...)
		}
	}
	if len(immediate) > 0 {
		go RunNotify(ctx, script, immediate)
	}
}

// flushDueNotify 释放到期未被 send-card 认领的延迟通知，内容与即时通知
// 完全一致，只是晚到。脚本已被删除（通知被关闭）时到期条目直接丢弃。
func (p *Poller) flushDueNotify(ctx context.Context, now int64) {
	batch, err := p.Store.NotifyDeferTakeDue(now)
	if err != nil {
		logf("notify defer take failed: %v", err)
		return
	}
	if len(batch) == 0 {
		return
	}
	if script := ReadNotifyScript(filepath.Join(p.Paths.ConfigDir, "notify")); script != "" {
		go RunNotify(ctx, script, batch)
	}
}

// mergeSelfLast 把单批提取结果并入 tick 级累积（保留每会话最大时间）。
func mergeSelfLast(acc, batch map[string]string) {
	for cid, t := range batch {
		if t > acc[cid] {
			acc[cid] = t
		}
	}
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
