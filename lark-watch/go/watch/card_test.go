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

// cardEventIdx 构造带候选索引的回调事件（多候选卡片的发送按钮）。
func cardEventIdx(eventID, token, action, mid string, idx int) []byte {
	return []byte(fmt.Sprintf(
		`{"event_id":%q,"action_tag":"button","token":%q,"action_value":"{\"action\":\"%s\",\"mid\":\"%s\",\"idx\":%d}"}`,
		eventID, token, action, mid, idx))
}

func TestCardSend(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t1", []string{"测试草稿"}, "text", testCardContent, 1)

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
	s := openTestStore(t)
	cli := &fakeCLI{}
	local := `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"LOCAL_CARD_MARKER"},{"tag":"button"}]}}`
	s.PendingPut("om_t6", []string{"草稿6"}, "text", local, 1)

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e6", "tok6", "send", "om_t6"), 100)

	if !cli.hasCall("LOCAL_CARD_MARKER") {
		t.Errorf("update should use local card source: %v", cli.calls)
	}
}

// markdown 草稿：落盘 format 原样透传给回复发送。
func TestCardSendMarkdownFormat(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	s.PendingPut("om_t7", []string{"```go\nx\n```"}, "markdown", testCardContent, 1)

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e7", "tok7", "send", "om_t7"), 100)

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

	HandleCardEvent(s, cli, "ou_SELF", cardEventIdx("e10", "tok10", "send", "om_c", 1), 100)

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

	HandleCardEvent(s, cli, "ou_SELF", cardEventIdx("e11", "tok11", "send", "om_o", 5), 100)

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

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e12", "tok12", "copy", "om_cm"), 100)

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

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e2", "tok2", "ignore", "om_t2"), 100)

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
	s := openTestStore(t)
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
	s := openTestStore(t)
	cli := &fakeCLI{failReply: true}
	s.PendingPut("om_t5", []string{"草稿5"}, "text", testCardContent, 1)

	HandleCardEvent(s, cli, "ou_SELF", cardEvent("e5", "tok5", "send", "om_t5"), 100)

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

	HandleCardEvent(s, cli, "ou_SELF", cardEventIdx("e20", "tok20", "send", "om_log", 1), 100)

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
		`<at user_id="ou_1">邹洋</at> 帮我看下 *这个* <方案>`, []string{"好的，```稍后```回复"}, "text", "")

	for _, want := range []string{
		`"schema":"2.0"`,
		"@邹洋 帮我看下 &#42;这个&#42; &#60;方案&#62;", // at 转 @名字 + 特殊字符转义
		"**草稿**\\n\\n```\\n",                 // 代码围栏前空行
		"'''稍后'''",                           // 草稿内围栏降级
		`"action":"send","idx":0,"mid":"om_x"`,
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
		"**草稿 ①**", `"action":"send","idx":0,"mid":"om_m"`,
		"**草稿 ②**", `"action":"send","idx":1,"mid":"om_m"`,
		"**草稿 ③**", `"action":"send","idx":2,"mid":"om_m"`,
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
		`"action":"send","idx":0,"mid":"om_md"`,
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
		`"action":"send","idx":0,"mid":"om_min"`,
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
