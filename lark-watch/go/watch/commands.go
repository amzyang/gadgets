package watch

import (
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

	result := CatchupGroup(rules.ClassifyAll(msgs), cursorMinutes, FmtMinute(floorEpoch), peek, hasMore)
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
// 用户本人。draftPaths 每项为草稿文件路径或 "-"（stdin，至多一项——stdin 只可读
// 一次）；format（text|markdown）决定回复消息类型与卡片渲染，应用于全部候选。
func RunSendCard(s *Store, cli LarkCLI, mid string, draftPaths []string, original, from, scene, t, format string) error {
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
	card := RenderDraftCard(mid, scene, from, t, original, drafts, format)
	if err := s.PendingPut(mid, drafts, format, card, time.Now().Unix()); err != nil {
		return err
	}
	if err := cli.SendCardToUser(self.OpenID, card); err != nil {
		return fmt.Errorf("send card failed: %w", err)
	}
	logf("draft card sent for %s", mid)
	return nil
}

// RunStatus 输出健康 JSON（含 auth 状态，心跳检查只看这一份输出）。
func RunStatus(s *Store, cli LarkCLI) error {
	os.Stdout.Write(EncodeLine(buildStatus(s, cli, time.Now())))
	return nil
}

func buildStatus(s *Store, cli LarkCLI, now time.Time) Status {
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
