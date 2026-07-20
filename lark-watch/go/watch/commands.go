package watch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseDuration 解析 "24h"/"3d"/"90m"/纯数字秒。
func ParseDuration(s string) (int64, error) {
	mul := int64(1)
	num := s
	switch {
	case strings.HasSuffix(s, "h"):
		mul, num = 3600, strings.TrimSuffix(s, "h")
	case strings.HasSuffix(s, "d"):
		mul, num = 86400, strings.TrimSuffix(s, "d")
	case strings.HasSuffix(s, "m"):
		mul, num = 60, strings.TrimSuffix(s, "m")
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return n * mul, nil
}

// RunCatchup 补课：拉积压消息按会话分组输出单行 JSON。
func RunCatchup(s *Store, cli LarkCLI, paths Paths, since string, peek int) error {
	self, err := cli.AuthSelf()
	if err != nil {
		return err
	}
	sinceSecs, err := ParseDuration(since)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	floorEpoch := now - sinceSecs
	startEpoch := floorEpoch

	cursors, err := s.ProcessedCursors()
	if err != nil {
		return err
	}
	for _, at := range cursors {
		if at < startEpoch {
			startEpoch = at
		}
	}
	if cap := now - searchMaxLookbackDays*86400; startEpoch < cap {
		startEpoch = cap // search 回溯硬上限
	}

	raw, err := cli.Search(fmtTS(startEpoch), fmtTS(now))
	if err != nil {
		return fmt.Errorf("catchup 拉取失败: %w", err)
	}
	msgs, hasMore, err := Trim(raw)
	if err != nil {
		return err
	}
	if hasMore {
		logf("结果被截断（search 单次上限 %d 页×%d 条），仅覆盖最近约 %d 条",
			searchMaxPages, searchPageSize, searchMaxPages*searchPageSize)
	}

	rules := LoadRulesDir(self.OpenID, paths.ConfigDir)
	cursorMinutes := make(map[string]string, len(cursors))
	for cid, at := range cursors {
		cursorMinutes[cid] = FmtMinute(at)
	}

	kept, _ := rules.ClassifyAll(msgs)
	result := CatchupGroup(kept, cursorMinutes, FmtMinute(floorEpoch), peek, hasMore)
	os.Stdout.Write(EncodeLine(result))

	cids := make([]string, 0, len(result.Chats))
	for _, c := range result.Chats {
		cids = append(cids, c.Cid)
	}
	return s.CatchupLastSet(cids)
}

// RunMark 标记会话已处理游标。
func RunMark(s *Store, cids []string, all bool, at int64) error {
	if all {
		last, err := s.CatchupLastGet()
		if err != nil {
			return err
		}
		if len(last) == 0 {
			return fmt.Errorf("mark --all 需要先跑过 catchup")
		}
		cids = append(cids, last...)
	}
	if err := s.MarkProcessed(cids, at); err != nil {
		return err
	}
	logf("marked %d chat(s) at %s", len(cids), FmtMinute(at))
	return nil
}

// RunIgnoreAdd 追加用户级噪音模式（正则校验通过才落盘）。
func RunIgnoreAdd(paths Paths, pattern string) error {
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("invalid regex, not added: %s (%v)", pattern, err)
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(paths.ConfigDir, ignoreFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, pattern); err != nil {
		return err
	}
	logf("ignore pattern added: %s", pattern)
	return nil
}

// RunSendCard 起草卡片：候选草稿 pending 入库 + 渲染 Card 2.0 模板 + bot 私发给
// 用户本人，随后释放该会话被延迟的 P0 系统通知（通知在草稿生成之后展示）。
// draftPaths 每项为草稿文件路径或 "-"（stdin，至多一项——stdin 只可读
// 一次）；format（text|markdown）决定回复消息类型与卡片渲染，应用于全部候选；
// note 非空时卡片展示「依据」状态行（表态门禁场景标注求证结论）。
func RunSendCard(s *Store, cli LarkCLI, paths Paths, mid string, draftPaths []string, original, from, scene, t, format, note string) error {
	drafts := make([]string, len(draftPaths))
	for i, path := range draftPaths {
		var draftBytes []byte
		var err error
		if path == "-" {
			draftBytes, err = io.ReadAll(os.Stdin)
		} else {
			draftBytes, err = os.ReadFile(path)
		}
		if err != nil {
			return err
		}
		drafts[i] = strings.TrimRight(string(draftBytes), "\n")
		if drafts[i] == "" {
			return fmt.Errorf("draft %d is empty", i+1)
		}
	}

	self, err := cli.AuthSelf()
	if err != nil {
		return err
	}
	card := RenderDraftCard(mid, scene, from, t, original, drafts, format, note)
	if err := s.PendingPut(mid, drafts, format, card, time.Now().Unix()); err != nil {
		return err
	}
	if err := cli.SendCardToUser(self.OpenID, card); err != nil {
		return fmt.Errorf("send card failed: %w", err)
	}
	logf("draft card sent for %s", mid)
	// 草稿已就绪：认领并展示同会话被延迟的系统通知。查无延迟条目
	// （已超时弹出 / 未配置通知 / 补课路径）则静默跳过，不会重复弹。
	// 等草稿期间本人已亲自回复的也不再弹（卡片照发——草稿仍可能有参考
	// 价值，只是不该再催）；chat_state 经 SQLite WAL 跨进程读 daemon 落盘值。
	if msgs, ok := s.NotifyDeferClaimChat(mid); ok {
		script := ReadNotifyScript(filepath.Join(paths.ConfigDir, "notify"))
		evlog.Info("notify.claim", "mid", mid, "n", len(msgs), "script", script != "")
		if msgs = dropReplied(s, msgs); len(msgs) > 0 && script != "" {
			StartNotify(context.Background(), script, msgs)
		}
	} else {
		evlog.Debug("notify.claim", "mid", mid, "n", 0) // 常态（已超时弹出/补课路径），降 debug
	}
	return nil
}

// RunStatus 输出健康 JSON（含 auth 状态，心跳检查只看这一份输出）。
func RunStatus(s *Store, cli LarkCLI, paths Paths) error {
	os.Stdout.Write(EncodeLine(buildStatus(s, cli, paths, time.Now())))
	return nil
}

func buildStatus(s *Store, cli LarkCLI, paths Paths, now time.Time) Status {
	heartbeat, _ := s.MetaGetInt("heartbeat")
	cursor, _ := s.MetaGetInt("cursor")
	lastFlush, _ := s.MetaGetInt("last_flush")
	consumer, _ := s.MetaGet("consumer_state")
	if consumer == "" {
		consumer = "unknown"
	}
	st := Status{
		Cursor: cursor, Heartbeat: heartbeat, HeartbeatAge: now.Unix() - heartbeat,
		ConsumerState: consumer, Pending: s.PendingCount(),
		DigestBuffered: s.DigestCount(), LastFlush: lastFlush,
	}
	st.RestrictedChats, _ = s.RestrictedList()
	if eventLogEnabled() {
		st.EventLog = eventLogPath(paths.StateDir)
	}
	auth, err := cli.AuthSelf()
	if err != nil {
		st.AuthWarning = authAlertMsg(err)
		return st
	}
	st.AuthOK = true
	if !auth.RefreshExpiresAt.IsZero() {
		st.AuthRefreshExpiresIn = int64(auth.RefreshExpiresAt.Sub(now).Seconds())
	}
	st.AuthWarning = authExpiringMsg(auth.RefreshExpiresAt, now)
	return st
}
