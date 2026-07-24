package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const emptyMessagesResp = `{"ok":true,"data":{"messages":[],"has_more":false}}`

// fakeCLI 记录调用并可注入失败（替代 bash 测试的 PATH shim）。
type fakeCLI struct {
	calls         []string
	failReply     bool
	failUpdate    bool
	failPatch     bool
	failAvatar    bool
	failDownload  bool
	onReply       func() // ReplyAsUser 后置钩子：在命令中途注错（如关库）用
	chatAvatarURL string
	userAvatarURL string
	docsFetch     func(ctx context.Context, ref string) ([]byte, error) // DocsFetch 注入；nil = 默认成功响应
	downloadName  string                                                // ResourceDownload 写入 destDir 的文件名；空 = "res.png"
}

func (f *fakeCLI) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}
func (f *fakeCLI) AuthSelf() (AuthInfo, error) { return AuthInfo{OpenID: "ou_SELF"}, nil }
func (f *fakeCLI) Search(start, end string) ([]byte, error) {
	f.record("search %s %s", start, end)
	return []byte(emptyMessagesResp), nil
}
func (f *fakeCLI) ChatList() ([]ChatMeta, error) {
	f.record("chat-list")
	return nil, nil
}
func (f *fakeCLI) ChatMessages(cid, start string) ([]byte, error) {
	f.record("chat-messages %s %s", cid, start)
	return []byte(emptyMessagesResp), nil
}
func (f *fakeCLI) EventConsumeCmd(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, "false") // 测试不跑 consume 子进程
}
func (f *fakeCLI) ReplyAsUser(mid, draft, format, idemKey string) error {
	f.record("reply %s %s format=%s key=%s", mid, draft, format, idemKey)
	if f.onReply != nil {
		f.onReply()
	}
	if f.failReply {
		return fmt.Errorf("api error")
	}
	return nil
}
func (f *fakeCLI) ReactAsUser(mid, emojiType string) error {
	f.record("react %s %s", mid, emojiType)
	if f.failReply {
		return fmt.Errorf("api error")
	}
	return nil
}
func (f *fakeCLI) ChatAvatar(cid string) (string, error) {
	f.record("chat-avatar %s", cid)
	if f.failAvatar {
		return "", fmt.Errorf("api error")
	}
	return f.chatAvatarURL, nil
}
func (f *fakeCLI) UserAvatar(openID, name string) (string, error) {
	f.record("user-avatar %s %s", openID, name)
	if f.failAvatar {
		return "", fmt.Errorf("api error")
	}
	return f.userAvatarURL, nil
}
func (f *fakeCLI) SendTextAsBot(userID, text string) error {
	f.record("send-text %s %s", userID, text)
	return nil
}
func (f *fakeCLI) SendCardToUser(userID, cardJSON string) (string, error) {
	f.record("send-card %s %s", userID, cardJSON)
	return "om_card_1", nil
}
func (f *fakeCLI) UpdateCard(token, cardJSON string) error {
	f.record("update-card %s %s", token, cardJSON)
	if f.failUpdate {
		return fmt.Errorf("token exhausted")
	}
	return nil
}
func (f *fakeCLI) PatchCard(cardMid, cardJSON string) error {
	f.record("patch-card %s %s", cardMid, cardJSON)
	if f.failPatch {
		return fmt.Errorf("api error")
	}
	return nil
}

func (f *fakeCLI) DocsFetch(ctx context.Context, ref string) ([]byte, error) {
	f.record("docs-fetch %s", ref)
	if f.docsFetch != nil {
		return f.docsFetch(ctx, ref)
	}
	return []byte(`{"ok":true,"data":{"document":{"document_id":"dox1","content":"# 测试文档\n\n正文内容"}}}`), nil
}

func (f *fakeCLI) ResourceDownload(ctx context.Context, mid, key, rtype, destDir string) (string, error) {
	f.record("download %s %s %s", mid, key, rtype)
	if f.failDownload {
		return "", fmt.Errorf("api error")
	}
	name := f.downloadName
	if name == "" {
		name = "res.png"
	}
	if err := os.WriteFile(filepath.Join(destDir, name), []byte("data"), 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func (f *fakeCLI) hasCall(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

const testCardContent = `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"原文"},{"tag":"button"},{"tag":"button"}]}}`

// 卡片链路 PendingDelete 失败不再静默（与 send-draft 同一盲区，行为仍 best-effort）。
func TestHandleDraftPendingDeleteFailureLogged(t *testing.T) {
	logs := captureEvlog(t)
	s := openTestStore(t)
	h := &CardHandler{Store: s, CLI: &fakeCLI{}, Self: "ou_SELF"}
	s.PendingPut("om_cd", []string{"候选"}, "text", testCardContent, 1)
	s.Close()

	h.handleDraft(CardEvent{EventID: "ev_cd"}, cardAction{Action: "ignore", Mid: "om_cd"})

	if !logsContain(logs(), "pending delete failed") {
		t.Error("pending delete failure should be logged")
	}
}

func cardEvent(eventID, token, action, mid string) []byte {
	return []byte(fmt.Sprintf(
		`{"event_id":%q,"action_tag":"button","token":%q,"action_value":"{\"action\":\"%s\",\"mid\":\"%s\"}","card_content":"{\"schema\":\"2.0\",\"body\":{\"elements\":[{\"tag\":\"markdown\",\"content\":\"原文\"},{\"tag\":\"button\"}]}}"}`,
		eventID, token, action, mid))
}

// cardEventIdx 构造带候选索引的回调事件（多候选卡片的发送按钮）。
func cardEventIdx(eventID, token, action, mid string, idx int) []byte {
	return []byte(fmt.Sprintf(
		`{"event_id":%q,"action_tag":"button","token":%q,"action_value":"{\"action\":\"%s\",\"mid\":\"%s\",\"idx\":%d}"}`,
		eventID, token, action, mid, idx))
}

// cardEventIdxH 构造带候选索引与内容指纹的回调事件（新版卡片按钮）。
func cardEventIdxH(eventID, token, action, mid string, idx int, h string) []byte {
	return []byte(fmt.Sprintf(
		`{"event_id":%q,"action_tag":"button","token":%q,"action_value":"{\"action\":\"%s\",\"mid\":\"%s\",\"idx\":%d,\"h\":\"%s\"}"}`,
		eventID, token, action, mid, idx, h))
}

// cardEventMsg 构造带卡片自身 message_id 的回调事件（PATCH 兜底路径）。
func cardEventMsg(eventID, token, msgID, action, mid string) []byte {
	return []byte(fmt.Sprintf(
		`{"event_id":%q,"action_tag":"button","token":%q,"message_id":%q,"action_value":"{\"action\":\"%s\",\"mid\":\"%s\"}"}`,
		eventID, token, msgID, action, mid))
}

// cardEventOp 构造带点击者 open_id 的回调事件（operator 过滤路径）。
func cardEventOp(eventID, operator, action, mid string) []byte {
	return []byte(fmt.Sprintf(
		`{"event_id":%q,"action_tag":"button","token":"tok","operator_id":%q,"action_value":"{\"action\":\"%s\",\"mid\":\"%s\"}"}`,
		eventID, operator, action, mid))
}

// token 版改卡失败（30 分钟/2 次用尽）：按事件自带的卡片 message_id PATCH 兜底。
func TestCardUpdateFallsBackToPatch(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{failUpdate: true}
	s.PendingPut("om_fb1", []string{"草稿"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEventMsg("efb1", "tokfb1", "om_card_fb1", "send", "om_fb1"), 100)

	if !cli.hasCall("update-card tokfb1") {
		t.Errorf("token update should be tried first: %v", cli.calls)
	}
	if !cli.hasCall("patch-card om_card_fb1") || !cli.hasCall("已发送") {
		t.Errorf("patch fallback missing: %v", cli.calls)
	}
}

// token 缺失但事件带 message_id：直接 PATCH，不尝试 token 版。
func TestCardUpdateNoTokenUsesPatch(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_fb2", []string{"草稿"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEventMsg("efb2", "", "om_card_fb2", "ignore", "om_fb2"), 100)

	if cli.hasCall("update-card") {
		t.Errorf("no token, should not call update-card: %v", cli.calls)
	}
	if !cli.hasCall("patch-card om_card_fb2") || !cli.hasCall("已忽略") {
		t.Errorf("patch fallback missing: %v", cli.calls)
	}
}

// token 与 message_id 均缺失：跳过改卡（发送本身不受影响）。
func TestCardUpdateNoTokenNoMsgIDSkips(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_fb3", []string{"草稿"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEvent("efb3", "", "send", "om_fb3"), 100)

	if cli.hasCall("update-card") || cli.hasCall("patch-card") {
		t.Errorf("no token/message_id, should skip card update: %v", cli.calls)
	}
	if !cli.hasCall("reply om_fb3") {
		t.Errorf("reply should still happen: %v", cli.calls)
	}
}

func TestCardSend(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t1", []string{"测试草稿"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEvent("e1", "tok1", "send", "om_t1"), 100)

	if !cli.hasCall("reply om_t1 测试草稿 format=text key=" + draftIdemKey("om_t1", "测试草稿")) {
		t.Errorf("reply args wrong: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_t1"); ok {
		t.Error("pending should be deleted after send")
	}
	if !cli.hasCall("update-card tok1") || !cli.hasCall("已发送") {
		t.Errorf("card update missing: %v", cli.calls)
	}
	// 改卡用本地原稿（含「原文」且按钮被剥除）
	for _, c := range cli.calls {
		if strings.HasPrefix(c, "update-card") && strings.Contains(c, "button") {
			t.Errorf("buttons not stripped: %s", c)
		}
	}
	// 去重：同一 event_id 重放不产生新调用
	n := len(cli.calls)
	handleCard(s, cli, "ou_SELF", cardEvent("e1", "tok1", "send", "om_t1"), 101)
	if len(cli.calls) != n {
		t.Errorf("duplicate event produced calls: %v", cli.calls[n:])
	}
}

func TestCardSendUsesLocalCardSource(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	local := `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"LOCAL_CARD_MARKER"},{"tag":"button"}]}}`
	s.PendingPut("om_t6", []string{"草稿6"}, "text", local, 1)

	handleCard(s, cli, "ou_SELF", cardEvent("e6", "tok6", "send", "om_t6"), 100)

	if !cli.hasCall("LOCAL_CARD_MARKER") {
		t.Errorf("update should use local card source: %v", cli.calls)
	}
}

// markdown 草稿：落盘 format 原样透传给回复发送。
func TestCardSendMarkdownFormat(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t7", []string{"```go\nx\n```"}, "markdown", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEvent("e7", "tok7", "send", "om_t7"), 100)

	if !cli.hasCall("format=markdown") {
		t.Errorf("format not passed through: %v", cli.calls)
	}
}

// 多候选：按 idx 发送对应候选，done 卡只保留所选候选块。
func TestCardSendCandidate(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	card := RenderDraftCard("om_c", "私聊", "张三", "", "原消息", []string{"候选A", "候选B"}, "text", "")
	s.PendingPut("om_c", []string{"候选A", "候选B"}, "text", card, 1)

	handleCard(s, cli, "ou_SELF", cardEventIdx("e10", "tok10", "send", "om_c", 1), 100)

	if !cli.hasCall("reply om_c 候选B format=text") {
		t.Errorf("should reply candidate B: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_c"); ok {
		t.Error("pending should be deleted after send")
	}
	for _, c := range cli.calls {
		if strings.HasPrefix(c, "update-card") && (strings.Contains(c, "候选A") || !strings.Contains(c, "候选B")) {
			t.Errorf("done card should keep only chosen candidate: %s", c)
		}
	}
}

// idx 越界（同 mid 重发覆盖 pending 后旧卡点了消失的候选）：不发送、改卡已失效、
// pending 保留（与 stale 语义一致，用户回终端处理）。
func TestCardSendIdxOutOfRange(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_o", []string{"唯一候选"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEventIdx("e11", "tok11", "send", "om_o", 5), 100)

	if cli.hasCall("reply") {
		t.Errorf("should not reply out-of-range candidate: %v", cli.calls)
	}
	if !cli.hasCall("已失效") {
		t.Errorf("should update stale status: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_o"); !ok {
		t.Error("pending should be kept")
	}
}

// 多候选复制：bot 逐条回发全部候选（每条可单独长按复制），pending 保留、不改卡。
func TestCardCopyMulti(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_cm", []string{"候选甲", "候选乙"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEvent("e12", "tok12", "copy", "om_cm"), 100)

	if !cli.hasCall("send-text ou_SELF 候选甲") || !cli.hasCall("send-text ou_SELF 候选乙") {
		t.Errorf("copy should send every candidate: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_cm"); !ok {
		t.Error("pending should be kept after copy")
	}
	if cli.hasCall("update-card") {
		t.Errorf("copy should not update card: %v", cli.calls)
	}
}

func TestCardIgnore(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t2", []string{"草稿2"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEvent("e2", "tok2", "ignore", "om_t2"), 100)

	if _, _, _, ok := s.PendingGet("om_t2"); ok {
		t.Error("pending should be deleted")
	}
	if !cli.hasCall("已忽略") || cli.hasCall("reply") {
		t.Errorf("calls: %v", cli.calls)
	}
}

func TestCardCopy(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t3", []string{"草稿3"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEvent("e3", "tok3", "copy", "om_t3"), 100)

	if !cli.hasCall("send-text ou_SELF 草稿3") {
		t.Errorf("copy should send draft text: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_t3"); !ok {
		t.Error("pending should be kept after copy")
	}
	if cli.hasCall("update-card") {
		t.Errorf("copy should not update card: %v", cli.calls)
	}
}

func TestCardSendMissingPending(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}

	handleCard(s, cli, "ou_SELF", cardEvent("e4", "tok4", "send", "om_none"), 100)

	if cli.hasCall("reply") {
		t.Errorf("should not reply: %v", cli.calls)
	}
	if !cli.hasCall("已失效") {
		t.Errorf("should update stale status (card_content fallback): %v", cli.calls)
	}
}

func TestCardSendFailureKeepsPending(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{failReply: true}
	s.PendingPut("om_t5", []string{"草稿5"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEvent("e5", "tok5", "send", "om_t5"), 100)

	if _, _, _, ok := s.PendingGet("om_t5"); !ok {
		t.Error("pending should be kept on failure")
	}
	if !cli.hasCall("发送失败") {
		t.Errorf("should update failure status: %v", cli.calls)
	}
}

// 卡片回调动作留痕（action/mid/idx/event_id 可与 msg.keep、send-card 对上）。
func TestHandleCardEventLogsAction(t *testing.T) {
	logs := captureEvlog(t)
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_log", []string{"候选A", "候选B"}, "text", testCardContent, 1)

	handleCard(s, cli, "ou_SELF", cardEventIdx("e20", "tok20", "send", "om_log", 1), 100)

	recs := findLogs(logs(), "card.action")
	if len(recs) != 1 {
		t.Fatalf("want 1 card.action, got %v", recs)
	}
	r := recs[0]
	if r["action"] != "send" || r["mid"] != "om_log" || r["idx"] != float64(1) || r["event_id"] != "e20" {
		t.Errorf("card.action attrs: %v", r)
	}
}

func TestRenderDraftCard(t *testing.T) {
	card := RenderDraftCard("om_x", "私聊", "张三", "12:03",
		`<at user_id="ou_1">周八</at> 帮我看下 *这个* <方案>`, []string{"好的，```稍后```回复"}, "text", "")

	for _, want := range []string{
		`"schema":"2.0"`,
		"@周八 帮我看下 &#42;这个&#42; &#60;方案&#62;", // at 转 @名字 + 特殊字符转义
		"**草稿**\\n\\n```\\n",                 // 代码围栏前空行
		"'''稍后'''",                           // 草稿内围栏降级
		fmt.Sprintf(`"action":"send","h":%q,"idx":0,"mid":"om_x"`, contentHash("好的，```稍后```回复")),
		`"action":"copy"`,
		`"action":"ignore"`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q\n%s", want, card)
		}
	}
	if strings.Contains(card, "confirm") {
		t.Error("send button must not have confirm popup")
	}
	// 单候选保持单草稿文案（不出现圈号）
	if strings.Contains(card, "草稿 ①") || strings.Contains(card, "发送 ①") {
		t.Errorf("single draft should not use circled labels: %s", card)
	}
}

// 多候选：每条候选块带圈号标题与自己的发送按钮（就近排列），底部共享复制/忽略。
func TestRenderDraftCardMulti(t *testing.T) {
	card := RenderDraftCard("om_m", "私聊", "张三", "", "原消息",
		[]string{"先答应", "先问细节", "婉拒"}, "text", "")

	// 元素顺序：草稿① < 发送① < 草稿② < 发送② < 草稿③ < 发送③ < 复制 < 忽略
	var last int
	for _, marker := range []string{
		"**草稿 ①**", fmt.Sprintf(`"action":"send","h":%q,"idx":0,"mid":"om_m"`, contentHash("先答应")),
		"**草稿 ②**", fmt.Sprintf(`"action":"send","h":%q,"idx":1,"mid":"om_m"`, contentHash("先问细节")),
		"**草稿 ③**", fmt.Sprintf(`"action":"send","h":%q,"idx":2,"mid":"om_m"`, contentHash("婉拒")),
		"复制草稿", "忽略",
	} {
		i := strings.Index(card, marker)
		if i < 0 {
			t.Fatalf("card missing %q\n%s", marker, card)
		}
		if i < last {
			t.Fatalf("element %q out of order\n%s", marker, card)
		}
		last = i
	}
	for _, want := range []string{
		`"element_id":"draft-0"`, `"element_id":"draft-1"`, `"element_id":"draft-2"`,
		"发送 ①", "发送 ②", "发送 ③",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q\n%s", want, card)
		}
	}
	if strings.Count(card, `"action":"copy"`) != 1 || strings.Count(card, `"action":"ignore"`) != 1 {
		t.Errorf("copy/ignore should appear once each: %s", card)
	}
}

// 多候选 text 格式：每条候选独立包围栏、独立降级内部围栏。
func TestRenderDraftCardMultiText(t *testing.T) {
	card := RenderDraftCard("om_mt", "", "", "", "", []string{"甲```x```", "乙"}, "text", "")
	for _, want := range []string{"'''x'''", "**草稿 ①**\\n\\n```\\n", "**草稿 ②**\\n\\n```\\n乙\\n```"} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q\n%s", want, card)
		}
	}
}

// 多候选 markdown 格式：每条候选独立走围栏补空行，不降级。
func TestRenderDraftCardMultiMarkdown(t *testing.T) {
	card := RenderDraftCard("om_mm", "", "", "", "",
		[]string{"看这段：\n```go\nx := 1\n```", "直接说结论"}, "markdown", "")
	for _, want := range []string{
		"**草稿 ①**\\n\\n看这段：\\n\\n```go\\nx := 1\\n```",
		"**草稿 ②**\\n\\n直接说结论",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q\n%s", want, card)
		}
	}
	if strings.Contains(card, "'''") {
		t.Errorf("markdown drafts should keep fences: %s", card)
	}
}

// markdown 草稿：直接按 markdown 渲染（不包围栏、不降级 ```），围栏前补空行。
func TestRenderDraftCardMarkdown(t *testing.T) {
	card := RenderDraftCard("om_md", "", "", "", "", []string{"看这段：\n```go\nx := 1\n```\n跑一下"}, "markdown", "")

	for _, want := range []string{
		"**草稿**\\n\\n看这段：\\n\\n```go\\nx := 1\\n```\\n跑一下", // 卡片方言：围栏前补空行
		fmt.Sprintf(`"action":"send","h":%q,"idx":0,"mid":"om_md"`, contentHash("看这段：\n```go\nx := 1\n```\n跑一下")),
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q\n%s", want, card)
		}
	}
	if strings.Contains(card, "'''") {
		t.Errorf("markdown draft should keep fences: %s", card)
	}
}

func TestPadCardFences(t *testing.T) {
	cases := map[string][2]string{
		"补空行":    {"文字\n```go\nx\n```", "文字\n\n```go\nx\n```"},
		"已有空行不动": {"文字\n\n```\nx\n```", "文字\n\n```\nx\n```"},
		"闭围栏不动":  {"```\nx\n```\n尾注", "```\nx\n```\n尾注"},
		"无围栏原样":  {"纯文本\n两行", "纯文本\n两行"},
	}
	for name, c := range cases {
		if got := padCardFences(c[0]); got != c[1] {
			t.Errorf("%s: want %q, got %q", name, c[1], got)
		}
	}
}

// 最小参数（仅 mid+draft）：展示片段整体省略，不渲染空标题/空引用。
func TestRenderDraftCardMinimal(t *testing.T) {
	card := RenderDraftCard("om_min", "", "", "", "", []string{"只有草稿"}, "text", "")

	for _, want := range []string{
		`"schema":"2.0"`,
		"**草稿**",
		fmt.Sprintf(`"action":"send","h":%q,"idx":0,"mid":"om_min"`, contentHash("只有草稿")),
		`"action":"copy"`,
		`"action":"ignore"`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q\n%s", want, card)
		}
	}
	for _, bad := range []string{"**：**", "> ", " · "} {
		if strings.Contains(card, bad) {
			t.Errorf("card should not contain %q\n%s", bad, card)
		}
	}
}

func TestRenderDoneCard(t *testing.T) {
	got, err := RenderDoneCard(testCardContent, doneSent, -1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "button") {
		t.Errorf("buttons not stripped: %s", got)
	}
	for _, want := range []string{"原文", "✅ 已发送"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
}

// 发送成功的多候选卡：只保留所选候选块，其余候选剔除；引用原消息等无
// element_id 的块（含旧版本卡片全部块）不受过滤影响。
func TestRenderDoneCardKeepsChosen(t *testing.T) {
	draft := RenderDraftCard("om_k", "私聊", "张三", "", "原始消息",
		[]string{"候选甲", "候选乙", "候选丙"}, "text", "")

	got, err := RenderDoneCard(draft, doneSent, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"原始消息", "候选乙", "✅ 已发送"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	for _, bad := range []string{"候选甲", "候选丙", "button"} {
		if strings.Contains(got, bad) {
			t.Errorf("should not contain %q: %s", bad, got)
		}
	}

	// keepIdx < 0（忽略/失效/失败态）：全部候选保留
	got, err = RenderDoneCard(draft, doneIgnored, -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"候选甲", "候选乙", "候选丙"} {
		if !strings.Contains(got, want) {
			t.Errorf("keepIdx=-1 missing %q: %s", want, got)
		}
	}
}

// note 非空时在全部候选之后、共享按钮之前追加灰字「依据」状态行（表态门禁），
// 空值整体省略；完成态改卡时依据行（非按钮、无 element_id）不受剔除影响。
func TestRenderDraftCardNote(t *testing.T) {
	card := RenderDraftCard("om_note", "私聊", "张三", "", "原消息",
		[]string{"我看下这块再回你"}, "text", "未验证对方建议，表态请自行判断")
	want := "<font color='grey'>依据：未验证对方建议，表态请自行判断</font>"
	if !strings.Contains(card, want) {
		t.Errorf("card missing note line %q: %s", want, card)
	}
	// 顺序：草稿块 < 依据行 < 共享按钮
	draftIdx := strings.Index(card, "我看下这块再回你")
	noteIdx := strings.Index(card, "依据：")
	copyIdx := strings.Index(card, "复制草稿")
	if draftIdx > noteIdx || noteIdx > copyIdx {
		t.Errorf("note line out of order (draft=%d note=%d copy=%d): %s", draftIdx, noteIdx, copyIdx, card)
	}

	done, err := RenderDoneCard(card, doneIgnored, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(done, "依据：") {
		t.Errorf("done card should keep note line: %s", done)
	}

	if strings.Contains(RenderDraftCard("om_non", "", "", "", "", []string{"x"}, "text", ""), "依据：") {
		t.Error("empty note should omit the note line")
	}
}

// 完成态更新头部标题（脱离「待确认」）：发卡渲染标题，改卡替换为完成态标题。
func TestRenderDoneCardUpdatesTitle(t *testing.T) {
	draft := RenderDraftCard("om_title", "私聊", "张三", "12:03", "原始消息", []string{"草稿内容"}, "text", "")
	if !strings.Contains(draft, "回复草稿待确认") {
		t.Fatalf("draft card should have pending title: %s", draft)
	}

	got, err := RenderDoneCard(draft, doneSent, -1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "回复草稿待确认") {
		t.Errorf("done card should not keep pending title: %s", got)
	}
	if !strings.Contains(got, doneSent.title) {
		t.Errorf("done card missing title %q: %s", doneSent.title, got)
	}
}

// handleCard 以默认 CardHandler（无 booker/out）处理回调，覆盖草稿卡分支。
func handleCard(s *Store, cli LarkCLI, self string, raw []byte, now int64) { //nolint:unparam // self 与调用点语义对齐
	(&CardHandler{Store: s, CLI: cli, Self: self}).Handle(raw, now)
}

// fakeBooker 记录预订调用并可注入失败（RoomBooker 测试替身）。
type fakeBooker struct {
	calls []string
	fail  error
	res   BookResult
}

func (f *fakeBooker) Book(_ context.Context, slot BookSlot, title string, participants []string) (BookResult, error) {
	f.calls = append(f.calls, fmt.Sprintf("book %s %s %s %s", slot.Date, slot.Time, title, strings.Join(participants, ",")))
	if f.fail != nil {
		return BookResult{}, f.fail
	}
	return f.res, nil
}

// bookHandler 构造带 booker 与事件收集的 CardHandler。
func bookHandler(s *Store, cli LarkCLI, b RoomBooker) (*CardHandler, *[][]byte) {
	var lines [][]byte
	h := &CardHandler{Store: s, CLI: cli, Booker: b, Self: "ou_SELF",
		Out: func(line []byte) { lines = append(lines, line) }}
	return h, &lines
}

func testBookPending() BookPending {
	return BookPending{
		Slots:        []BookSlot{{Date: "07-22", Time: "14:00-15:00"}, {Date: "07-22", Time: "16:00-17:00"}},
		Title:        "方案对齐会",
		Participants: []string{"alice@corp.com"},
		Card:         testCardContent,
	}
}

// 点「预约 ②」：按 idx 预订对应时段，pending 删除，改卡「会议已预约」，
// stdout 发 booked 事件（模型据此起草告知对方）。
func TestCardBookSuccess(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	booker := &fakeBooker{res: BookResult{Room: "A栋3F-301", Date: "2026-07-22", Start: "16:00", End: "17:00", EventID: "ev_1"}}
	h, lines := bookHandler(s, cli, booker)
	s.BookPendingPut("om_bk1", testBookPending(), 1)

	h.Handle(cardEventIdx("eb1", "tokb1", "book", "om_bk1", 1), 100)

	if len(booker.calls) != 1 || booker.calls[0] != "book 07-22 16:00-17:00 方案对齐会 alice@corp.com" {
		t.Errorf("booker calls: %v", booker.calls)
	}
	if _, ok := s.BookPendingGet("om_bk1"); ok {
		t.Error("book pending should be deleted after booking")
	}
	if !cli.hasCall("update-card tokb1") || !cli.hasCall("✅ 已预约 A栋3F-301 · 2026-07-22 16:00-17:00") {
		t.Errorf("done card missing booked status: %v", cli.calls)
	}
	if len(*lines) != 1 {
		t.Fatalf("want 1 stdout event, got %v", *lines)
	}
	var evt BookedEvent
	if err := json.Unmarshal((*lines)[0], &evt); err != nil {
		t.Fatal(err)
	}
	if evt.P != "booked" || evt.Room != "A栋3F-301" || evt.Mid != "om_bk1" || evt.EventID != "ev_1" || evt.Title != "方案对齐会" {
		t.Errorf("booked event: %+v", evt)
	}
	// 去重：同一 event_id 重放不再预订
	h.Handle(cardEventIdx("eb1", "tokb1", "book", "om_bk1", 1), 101)
	if len(booker.calls) != 1 {
		t.Errorf("duplicate event should not book again: %v", booker.calls)
	}
}

// 预订失败：pending 不 re-put（重试由模型经 book-failed 事件重发新卡），
// 改卡「预约失败」带错误信息，stdout 发 book-failed。
func TestCardBookFailure(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	booker := &fakeBooker{fail: &BookError{Type: "no_room", Message: "该时段无可用会议室", Hint: "换时段重试"}}
	h, lines := bookHandler(s, cli, booker)
	s.BookPendingPut("om_bk2", testBookPending(), 1)

	h.Handle(cardEventIdx("eb2", "tokb2", "book", "om_bk2", 0), 100)

	if _, ok := s.BookPendingGet("om_bk2"); ok {
		t.Error("book pending should not be re-put on failure")
	}
	if !cli.hasCall("预约失败") || !cli.hasCall("该时段无可用会议室") {
		t.Errorf("done card missing failure status: %v", cli.calls)
	}
	var evt BookFailedEvent
	if len(*lines) != 1 {
		t.Fatalf("want 1 stdout event, got %v", *lines)
	}
	if err := json.Unmarshal((*lines)[0], &evt); err != nil {
		t.Fatal(err)
	}
	if evt.P != "book-failed" || evt.Reason != "no_room" || evt.Mid != "om_bk2" || evt.Hint != "换时段重试" {
		t.Errorf("book-failed event: %+v", evt)
	}
}

// 双击（两个不同 event_id）：第一次订完删 pending，第二次落空改卡「已失效」，
// 只订一次。
func TestCardBookDoubleClick(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	booker := &fakeBooker{res: BookResult{Room: "R1"}}
	h, _ := bookHandler(s, cli, booker)
	s.BookPendingPut("om_bk3", testBookPending(), 1)

	h.Handle(cardEventIdx("eb3a", "tok3a", "book", "om_bk3", 0), 100)
	// 第二击：pending 已删，改卡走事件 card_content 兜底（真实回调都带）
	h.Handle([]byte(`{"event_id":"eb3b","action_tag":"button","token":"tok3b",`+
		`"action_value":"{\"action\":\"book\",\"mid\":\"om_bk3\",\"idx\":0}",`+
		`"card_content":"{\"schema\":\"2.0\",\"body\":{\"elements\":[]}}"}`), 101)

	if len(booker.calls) != 1 {
		t.Errorf("double click should book once: %v", booker.calls)
	}
	if !cli.hasCall("已失效") {
		t.Errorf("second click should mark stale: %v", cli.calls)
	}
}

// pending 缺失 / idx 越界：不预订，改卡「已失效」。
func TestCardBookMissingAndOutOfRange(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	booker := &fakeBooker{}
	h, lines := bookHandler(s, cli, booker)

	h.Handle(cardEventIdx("eb4", "tok4", "book", "om_none", 0), 100)

	s.BookPendingPut("om_bk4", testBookPending(), 1)
	h.Handle(cardEventIdx("eb5", "tok5", "book", "om_bk4", 9), 101)

	if len(booker.calls) != 0 {
		t.Errorf("should not book: %v", booker.calls)
	}
	if _, ok := s.BookPendingGet("om_bk4"); !ok {
		t.Error("out-of-range click should keep pending")
	}
	if !cli.hasCall("已失效") || len(*lines) != 0 {
		t.Errorf("want stale card and no events: %v %v", cli.calls, *lines)
	}
}

// 同 mid 重发覆盖 pending 后，旧卡按钮指纹不符：不发送（发的必须是用户在这张卡
// 上看到的文本），改卡「已失效」，pending 保留给新卡；指纹相符照常发送。
func TestCardSendStaleFingerprint(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_h1", []string{"草稿A"}, "text", testCardContent, 1)
	s.PendingPut("om_h1", []string{"草稿B"}, "text", testCardContent, 2) // 重发覆盖

	// 旧卡按钮仍携带草稿A 的指纹
	handleCard(s, cli, "ou_SELF", cardEventIdxH("eh1", "tokh1", "send", "om_h1", 0, contentHash("草稿A")), 100)

	if cli.hasCall("reply om_h1") {
		t.Errorf("stale fingerprint must not send: %v", cli.calls)
	}
	if !cli.hasCall("已失效") {
		t.Errorf("want stale card update: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_h1"); !ok {
		t.Error("pending should be kept for the fresh card")
	}

	// 新卡按钮指纹相符：照常发送
	handleCard(s, cli, "ou_SELF", cardEventIdxH("eh2", "tokh2", "send", "om_h1", 0, contentHash("草稿B")), 101)
	if !cli.hasCall("reply om_h1 草稿B format=text") {
		t.Errorf("matching fingerprint should send: %v", cli.calls)
	}
}

// 预约按钮指纹不符（同 mid 重发改了时段）：不预订、认领作废参数放回、改卡
// 「已失效」；指纹相符照常预订。
func TestCardBookStaleFingerprint(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	booker := &fakeBooker{}
	h, lines := bookHandler(s, cli, booker)
	s.BookPendingPut("om_hb", testBookPending(), 1)

	h.Handle(cardEventIdxH("ehb1", "tokhb1", "book", "om_hb", 1, contentHash("07-21 10:00-11:00")), 100)

	if len(booker.calls) != 0 {
		t.Errorf("stale fingerprint must not book: %v", booker.calls)
	}
	if _, ok := s.BookPendingGet("om_hb"); !ok {
		t.Error("pending should be re-put after stale claim")
	}
	if !cli.hasCall("已失效") || len(*lines) != 0 {
		t.Errorf("want stale card, no events: %v %v", cli.calls, *lines)
	}

	// 指纹相符：照常预订（slots[1] = 07-22 16:00-17:00）
	h.Handle(cardEventIdxH("ehb2", "tokhb2", "book", "om_hb", 1, contentHash("07-22 16:00-17:00")), 101)
	if len(booker.calls) != 1 {
		t.Errorf("matching fingerprint should book: %v", booker.calls)
	}
}

// 「忽略」：删 pending，改卡「已忽略」，不预订不发事件。
func TestCardBookIgnore(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	booker := &fakeBooker{}
	h, lines := bookHandler(s, cli, booker)
	s.BookPendingPut("om_bk5", testBookPending(), 1)

	h.Handle(cardEvent("eb6", "tok6", "book-ignore", "om_bk5"), 100)

	if _, ok := s.BookPendingGet("om_bk5"); ok {
		t.Error("pending should be deleted on ignore")
	}
	if !cli.hasCall("已忽略") || len(booker.calls) != 0 || len(*lines) != 0 {
		t.Errorf("ignore should only update card: %v %v", cli.calls, booker.calls)
	}
}

// 意向卡渲染：多时段圈号按钮 + 忽略；orange 头部；标题/参会展示行；
// element_id 沿用 draft- 前缀（复用完成态 keepIdx 过滤）；原消息转义。
func TestRenderBookCard(t *testing.T) {
	card := RenderBookCard("om_rb", "私聊", "张三", "12:03", "明天下午对齐 *方案*",
		[]BookSlot{{Date: "07-22", Time: "14:00-15:00"}, {Date: "07-22", Time: "16:00-17:00"}},
		"方案对齐会", []string{"张三", "本人"})

	var last int
	for _, marker := range []string{
		"明天下午对齐 &#42;方案&#42;",
		"时段 ①", fmt.Sprintf(`"action":"book","h":%q,"idx":0,"mid":"om_rb"`, contentHash("07-22 14:00-15:00")),
		"时段 ②", fmt.Sprintf(`"action":"book","h":%q,"idx":1,"mid":"om_rb"`, contentHash("07-22 16:00-17:00")),
		"标题：方案对齐会", `"action":"book-ignore","mid":"om_rb"`,
	} {
		i := strings.Index(card, marker)
		if i < 0 {
			t.Fatalf("card missing %q\n%s", marker, card)
		}
		if i < last {
			t.Fatalf("element %q out of order\n%s", marker, card)
		}
		last = i
	}
	for _, want := range []string{
		"会议预约待确认", `"template":"orange"`, "预约 ①", "预约 ②",
		`"element_id":"draft-0"`, `"element_id":"draft-1"`,
		"参会：张三、本人", "07-22 14:00-15:00", "07-22 16:00-17:00",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q\n%s", want, card)
		}
	}
}

// 卡片被转发后他人点击：operator ≠ self 直接丢弃（不发送、不预订、pending
// 保留、不发事件）；operator == self 或缺失（旧事件兼容）照常处理。
func TestCardOperatorFilter(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	booker := &fakeBooker{}
	h, lines := bookHandler(s, cli, booker)
	s.PendingPut("om_op1", []string{"草稿"}, "text", testCardContent, 1)
	s.BookPendingPut("om_op1", testBookPending(), 1)

	h.Handle(cardEventOp("eop1", "ou_OTHER", "send", "om_op1"), 100)
	h.Handle(cardEventOp("eop2", "ou_OTHER", "book", "om_op1"), 101)

	if len(cli.calls) != 0 || len(booker.calls) != 0 || len(*lines) != 0 {
		t.Errorf("foreign operator should be dropped: %v %v %v", cli.calls, booker.calls, *lines)
	}
	if _, _, _, ok := s.PendingGet("om_op1"); !ok {
		t.Error("pending should be kept after foreign click")
	}
	if _, ok := s.BookPendingGet("om_op1"); !ok {
		t.Error("book pending should be kept after foreign click")
	}
	// 本人点击照常处理
	h.Handle(cardEventOp("eop3", "ou_SELF", "ignore", "om_op1"), 102)
	if _, _, _, ok := s.PendingGet("om_op1"); ok {
		t.Error("self operator should be processed")
	}
}

// 确认卡按钮有真实副作用（以用户身份发消息 / room book）：草稿卡与意向卡
// 都显式禁转发（平台默认允许转发）。
func TestRenderCardsDisableForward(t *testing.T) {
	for name, card := range map[string]string{
		"draft": RenderDraftCard("om_fw", "", "", "", "", []string{"x"}, "text", ""),
		"book": RenderBookCard("om_fw", "", "", "", "",
			[]BookSlot{{Date: "07-22", Time: "14:00-15:00"}}, "会", nil),
	} {
		if !strings.Contains(card, `"enable_forward":false`) {
			t.Errorf("%s card should disable forward: %s", name, card)
		}
	}
}

// 标题/参会人的字面换行归一为空格，不得在卡片上伪造额外展示行。
func TestRenderBookCardOnelineTitle(t *testing.T) {
	card := RenderBookCard("om_nl", "", "", "", "",
		[]BookSlot{{Date: "07-22", Time: "14:00-15:00"}}, "对齐会\n参会：假冒行", []string{"a\nb"})
	if !strings.Contains(card, "标题：对齐会 参会：假冒行") {
		t.Errorf("title newline should collapse to space: %s", card)
	}
	if !strings.Contains(card, "参会：a b") {
		t.Errorf("participant newline should collapse to space: %s", card)
	}
}

// 单时段：按钮文案「我要预约」，不出现圈号；无参会人时省略参会行。
func TestRenderBookCardSingle(t *testing.T) {
	card := RenderBookCard("om_rb1", "", "", "", "",
		[]BookSlot{{Date: "07-22", Time: "14:00-15:00"}}, "周会", nil)
	if !strings.Contains(card, "我要预约") || strings.Contains(card, "预约 ①") || strings.Contains(card, "时段 ①") {
		t.Errorf("single slot labels wrong: %s", card)
	}
	if strings.Contains(card, "参会：") {
		t.Errorf("empty participants should omit the line: %s", card)
	}
}

// 预约完成态：成功只留所选时段块并带会议室；失败保留全部时段并带 hint。
func TestRenderBookDoneStates(t *testing.T) {
	card := RenderBookCard("om_rb2", "私聊", "张三", "", "约个会",
		[]BookSlot{{Date: "07-22", Time: "14:00-15:00"}, {Date: "07-22", Time: "16:00-17:00"}},
		"对齐会", nil)

	done, err := RenderDoneCard(card, doneBooked(BookResult{Room: "R1", Date: "2026-07-22", Start: "16:00", End: "17:00"}), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"会议已预约", "✅ 已预约 R1 · 2026-07-22 16:00-17:00", "16:00-17:00"} {
		if !strings.Contains(done, want) {
			t.Errorf("booked card missing %q: %s", want, done)
		}
	}
	if strings.Contains(done, "14:00-15:00") || strings.Contains(done, "button") {
		t.Errorf("booked card should keep only chosen slot, no buttons: %s", done)
	}

	failed, err := RenderDoneCard(card, doneBookFailed(&BookError{Type: "no_room", Message: "无可用会议室", Hint: "换时段"}), -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"预约失败", "无可用会议室（换时段）", "14:00-15:00", "16:00-17:00"} {
		if !strings.Contains(failed, want) {
			t.Errorf("failed card missing %q: %s", want, failed)
		}
	}
}
