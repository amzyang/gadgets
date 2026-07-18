package watch

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

const emptyMessagesResp = `{"ok":true,"data":{"messages":[],"has_more":false}}`

// fakeCLI 记录调用并可注入失败（替代 bash 测试的 PATH shim）。
type fakeCLI struct {
	calls     []string
	failReply bool
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
func (f *fakeCLI) ReplyAsUser(mid, draft, format string) error {
	f.record("reply %s %s format=%s", mid, draft, format)
	if f.failReply {
		return fmt.Errorf("api error")
	}
	return nil
}
func (f *fakeCLI) SendTextAsBot(userID, text string) error {
	f.record("send-text %s %s", userID, text)
	return nil
}
func (f *fakeCLI) SendCardToUser(userID, cardJSON string) error {
	f.record("send-card %s %s", userID, cardJSON)
	return nil
}
func (f *fakeCLI) UpdateCard(token, cardJSON string) error {
	f.record("update-card %s %s", token, cardJSON)
	return nil
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

func cardEvent(eventID, token, action, mid string) []byte {
	return []byte(fmt.Sprintf(
		`{"event_id":%q,"action_tag":"button","token":%q,"action_value":"{\"action\":\"%s\",\"mid\":\"%s\"}","card_content":"{\"schema\":\"2.0\",\"body\":{\"elements\":[{\"tag\":\"markdown\",\"content\":\"原文\"},{\"tag\":\"button\"}]}}"}`,
		eventID, token, action, mid))
}

func TestCardSend(t *testing.T) {
	s, _ := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t1", "测试草稿", "text", testCardContent, 1)

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e1", "tok1", "send", "om_t1"), 100)

	if !cli.hasCall("reply om_t1 测试草稿 format=text") {
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
	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e1", "tok1", "send", "om_t1"), 101)
	if len(cli.calls) != n {
		t.Errorf("duplicate event produced calls: %v", cli.calls[n:])
	}
}

func TestCardSendUsesLocalCardSource(t *testing.T) {
	s, _ := openTestStore(t)
	cli := &fakeCLI{}
	local := `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"LOCAL_CARD_MARKER"},{"tag":"button"}]}}`
	s.PendingPut("om_t6", "草稿6", "text", local, 1)

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e6", "tok6", "send", "om_t6"), 100)

	if !cli.hasCall("LOCAL_CARD_MARKER") {
		t.Errorf("update should use local card source: %v", cli.calls)
	}
}

// markdown 草稿：落盘 format 原样透传给回复发送。
func TestCardSendMarkdownFormat(t *testing.T) {
	s, _ := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t7", "```go\nx\n```", "markdown", testCardContent, 1)

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e7", "tok7", "send", "om_t7"), 100)

	if !cli.hasCall("format=markdown") {
		t.Errorf("format not passed through: %v", cli.calls)
	}
}

func TestCardIgnore(t *testing.T) {
	s, _ := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t2", "草稿2", "text", testCardContent, 1)

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e2", "tok2", "ignore", "om_t2"), 100)

	if _, _, _, ok := s.PendingGet("om_t2"); ok {
		t.Error("pending should be deleted")
	}
	if !cli.hasCall("已忽略") || cli.hasCall("reply") {
		t.Errorf("calls: %v", cli.calls)
	}
}

func TestCardCopy(t *testing.T) {
	s, _ := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t3", "草稿3", "text", testCardContent, 1)

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e3", "tok3", "copy", "om_t3"), 100)

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
	s, _ := openTestStore(t)
	cli := &fakeCLI{}

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e4", "tok4", "send", "om_none"), 100)

	if cli.hasCall("reply") {
		t.Errorf("should not reply: %v", cli.calls)
	}
	if !cli.hasCall("已失效") {
		t.Errorf("should update stale status (card_content fallback): %v", cli.calls)
	}
}

func TestCardSendFailureKeepsPending(t *testing.T) {
	s, _ := openTestStore(t)
	cli := &fakeCLI{failReply: true}
	s.PendingPut("om_t5", "草稿5", "text", testCardContent, 1)

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e5", "tok5", "send", "om_t5"), 100)

	if _, _, _, ok := s.PendingGet("om_t5"); !ok {
		t.Error("pending should be kept on failure")
	}
	if !cli.hasCall("发送失败") {
		t.Errorf("should update failure status: %v", cli.calls)
	}
}

func TestRenderDraftCard(t *testing.T) {
	card := RenderDraftCard("om_x", "私聊", "张三", "12:03",
		`<at user_id="ou_1">邹洋</at> 帮我看下 *这个* <方案>`, "好的，```稍后```回复", "text")

	for _, want := range []string{
		`"schema":"2.0"`,
		"@邹洋 帮我看下 &#42;这个&#42; &#60;方案&#62;", // at 转 @名字 + 特殊字符转义
		"**草稿**\\n\\n```\\n",                 // 代码围栏前空行
		"'''稍后'''",                           // 草稿内围栏降级
		`"action":"send","mid":"om_x"`,
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
}

// markdown 草稿：直接按 markdown 渲染（不包围栏、不降级 ```），围栏前补空行。
func TestRenderDraftCardMarkdown(t *testing.T) {
	card := RenderDraftCard("om_md", "", "", "", "", "看这段：\n```go\nx := 1\n```\n跑一下", "markdown")

	for _, want := range []string{
		"**草稿**\\n\\n看这段：\\n\\n```go\\nx := 1\\n```\\n跑一下", // 卡片方言：围栏前补空行
		`"action":"send","mid":"om_md"`,
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
	card := RenderDraftCard("om_min", "", "", "", "", "只有草稿", "text")

	for _, want := range []string{
		`"schema":"2.0"`,
		"**草稿**",
		`"action":"send","mid":"om_min"`,
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
	got, err := RenderDoneCard(testCardContent, doneSent)
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

// 完成态更新头部标题（脱离「待确认」）：发卡渲染标题，改卡替换为完成态标题。
func TestRenderDoneCardUpdatesTitle(t *testing.T) {
	draft := RenderDraftCard("om_title", "私聊", "张三", "12:03", "原始消息", "草稿内容", "text")
	if !strings.Contains(draft, "回复草稿待确认") {
		t.Fatalf("draft card should have pending title: %s", draft)
	}

	got, err := RenderDoneCard(draft, doneSent)
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
