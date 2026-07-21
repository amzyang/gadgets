package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	cardMid, err := cli.SendCardToUser(self.OpenID, card)
	if err != nil {
		return fmt.Errorf("send card failed: %w", err)
	}
	if cardMid != "" {
		// 回填卡片自身 message_id：alerter 路径改卡完成态的凭证（回填失败/
		// 响应缺字段只降级为不改卡，卡片回调路径有 token、不依赖它）。
		if err := s.PendingSetCardMid(mid, cardMid); err != nil {
			logf("card_mid save failed for %s: %v", mid, err)
		}
	}
	logf("draft card sent for %s", mid)
	// 草稿已就绪：认领并展示同会话被延迟的系统通知。查无延迟条目
	// （已超时弹出 / 通知已关闭 / 补课路径）则静默跳过，不会重复弹。
	// 等草稿期间本人已亲自回复的也不再弹（卡片照发——草稿仍可能有参考
	// 价值，只是不该再催）；chat_state 经 SQLite WAL 跨进程读 daemon 落盘值。
	if msgs, ok := s.NotifyDeferClaimChat(mid); ok {
		script, enabled := LoadNotifyScript(paths.ConfigDir)
		evlog.Info("notify.claim", "mid", mid, "n", len(msgs), "script", script != "", "enabled", enabled)
		if msgs = dropReplied(s, msgs); len(msgs) > 0 && enabled {
			// 候选①与 pending 键随通知下发：弹窗「复制」给待发话术、「发送」直接回复。
			StartNotify(context.Background(), paths.ConfigDir, script, msgs, drafts[0], mid)
		}
	} else {
		evlog.Debug("notify.claim", "mid", mid, "n", 0) // 常态（已超时弹出/补课路径），降 debug
	}
	return nil
}

// 通知横幅/弹窗回调的子命令与 flag 名：main dispatch 与 notify.go 脚本模板
// 共用（脚本里字面拼命令行，编译期无约束，常量即两侧契约）。
const (
	CmdSendDraft = "send-draft"
	CmdSendText  = "send-text"
	CmdReact     = "react"
	FlagMid      = "mid"
	FlagText     = "text"
	FlagEmoji    = "emoji"
)

// markCardDone 把 pending 对应的草稿卡片改为完成态（alerter 路径的改卡：不经
// 回调、无 token，按发卡时回填的卡片 message_id PATCH）。全程 best-effort——
// card_mid 未回填（存量 pending/响应缺字段）跳过，渲染或 PATCH 失败仅记日志，
// 不影响发送主流程。须在 PendingDelete 之前调用（删后读不到卡片原稿）。
func markCardDone(s *Store, cli LarkCLI, mid string, st doneState, keepIdx int) {
	card, cardMid, ok := s.PendingCard(mid)
	if !ok || cardMid == "" {
		return
	}
	newCard, err := RenderDoneCard(card, st, keepIdx)
	if err != nil {
		logf("card render failed for %s: %v", mid, err)
		return
	}
	if err := cli.PatchCard(cardMid, newCard); err != nil {
		logf("card patch failed for %s: %v", mid, err)
	}
}

// RunSendDraft 是 send-draft 子命令入口（通知弹窗「发送」按钮的回调）：
// 按 pending 里的候选直接以用户身份回复，语义与卡片「发送」一致（幂等键 =
// 源消息 mid，弹窗/卡片双端点击也不会双发）。成功后按 card_mid 改卡
// 「✅ 已发送」（只留所发候选）并删除 pending。失败保留 pending 并弹错误提示
// （弹窗场景无终端可看，静默失败会让用户误以为已发出；提示弹窗 best-effort）。
func RunSendDraft(ctx context.Context, s *Store, cli LarkCLI, paths Paths, mid string, idx int) error {
	drafts, format, _, ok := s.PendingGet(mid)
	if !ok {
		sendDraftAlertFn(ctx, paths.ConfigDir, "草稿已失效", "草稿不存在或已处理（可能已在卡片端发送/忽略）")
		return fmt.Errorf("no pending draft for %s", mid)
	}
	if idx < 0 || idx >= len(drafts) {
		return fmt.Errorf("draft idx %d out of range for %s (%d drafts)", idx, mid, len(drafts))
	}
	if err := cli.ReplyAsUser(mid, drafts[idx], format, mid); err != nil {
		evlog.Info("popup.send", "mid", mid, "idx", idx, "ok", false)
		sendDraftAlertFn(ctx, paths.ConfigDir, "回复发送失败", "草稿发送失败，请回终端或卡片处理")
		return err
	}
	markCardDone(s, cli, mid, doneSent, idx)
	s.PendingDelete(mid)
	evlog.Info("popup.send", "mid", mid, "idx", idx, "ok", true)
	logf("sent reply for %s (candidate %d, via popup)", mid, idx)
	return nil
}

// quickIdemKey 是常用语快捷回复的幂等键：与卡片/弹窗「发送」的键（= mid）
// 分离——共键会让服务端把后发的正式回复当重复吞掉；带文本哈希则同一条
// 常用语连点仍防双发、不同常用语互不干扰。
func quickIdemKey(mid, text string) string {
	sum := sha256.Sum256([]byte(text))
	return mid + "-q-" + hex.EncodeToString(sum[:4])
}

// RunSendText 是 send-text 子命令入口（通知横幅常用语动作的回调）：
// 以固定常用语纯文本回复源消息。成功后改卡「已快捷回复」（发出的是常用语而
// 非草稿，不标「已发送」；候选正文全保留）并删除 pending（事已处理，草稿候选
// 随之失效，与卡片「发送」语义一致；无 pending——即时/兜底通知场景——是
// no-op）；失败保留 pending 并弹提示。
func RunSendText(ctx context.Context, s *Store, cli LarkCLI, paths Paths, mid, text string) error {
	if err := cli.ReplyAsUser(mid, text, "text", quickIdemKey(mid, text)); err != nil {
		evlog.Info("popup.qreply", "mid", mid, "ok", false)
		sendDraftAlertFn(ctx, paths.ConfigDir, "快捷回复失败", "常用语发送失败，请回终端或飞书处理")
		return err
	}
	markCardDone(s, cli, mid, doneQuick, -1)
	s.PendingDelete(mid)
	evlog.Info("popup.qreply", "mid", mid, "ok", true)
	logf("sent quick reply for %s", mid)
	return nil
}

// RunReact 是 react 子命令入口（通知横幅表情动作的回调）：给源消息加表情
// 回应。不动 pending——点赞不等于已回复，草稿仍可后续发送。
func RunReact(ctx context.Context, cli LarkCLI, paths Paths, mid, emoji string) error {
	if !emojiTypeRe.MatchString(emoji) {
		return fmt.Errorf("invalid emoji type %q", emoji)
	}
	if err := cli.ReactAsUser(mid, emoji); err != nil {
		evlog.Info("popup.react", "mid", mid, "emoji", emoji, "ok", false)
		sendDraftAlertFn(ctx, paths.ConfigDir, "表情回应失败", "表情回应发送失败，请回飞书处理")
		return err
	}
	evlog.Info("popup.react", "mid", mid, "emoji", emoji, "ok", true)
	logf("reacted %s on %s", emoji, mid)
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
