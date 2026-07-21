package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// send-card 多候选：逐个读文件、pending 入库多条、卡片含各候选的发送按钮；
// 成功后记 card.sent 结构化锚点（草稿链路按 mid join 的落点）。
func TestRunSendCardMulti(t *testing.T) {
	logs := captureEvlog(t)
	s := openTestStore(t)
	cli := &fakeCLI{}
	dir := t.TempDir()
	p1 := filepath.Join(dir, "d1.md")
	p2 := filepath.Join(dir, "d2.md")
	os.WriteFile(p1, []byte("候选一\n"), 0o644)
	os.WriteFile(p2, []byte("候选二\n"), 0o644)

	if err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_sc", []string{p1, p2}, "原消息", "张三", "私聊", "12:00", "text", ""); err != nil {
		t.Fatal(err)
	}
	sent := findLogs(logs(), "card.sent")
	if len(sent) != 1 || sent[0]["mid"] != "om_sc" || sent[0]["drafts"] != float64(2) ||
		sent[0]["format"] != "text" || sent[0]["level"] != "INFO" {
		t.Errorf("card.sent: %v", sent)
	}
	drafts, format, card, ok := s.PendingGet("om_sc")
	if !ok || len(drafts) != 2 || drafts[0] != "候选一" || drafts[1] != "候选二" || format != "text" {
		t.Fatalf("pending: %q %q %v", drafts, format, ok)
	}
	for _, want := range []string{"发送 ①", "发送 ②", `"idx":1`} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q\n%s", want, card)
		}
	}
	if !cli.hasCall("send-card ou_SELF") {
		t.Errorf("card not sent: %v", cli.calls)
	}
	// 发卡返回的卡片 message_id 落库（alerter 路径改卡完成态的凭证）
	if _, cardMid, ok := s.PendingCard("om_sc"); !ok || cardMid != "om_card_1" {
		t.Errorf("card_mid: got %q (ok=%v), want om_card_1", cardMid, ok)
	}
}

// waitForFile 轮询等待异步通知脚本落盘（StartNotify / go RunNotify 不阻塞调用方）。
func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(path); err == nil {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("notify output %s not written", path)
	return nil
}

// send-card 发卡后释放同会话的延迟通知：脚本收到聚合摘要与候选①话术
// （LW_DRAFT，供弹窗「复制」用——复制的是待发回复而非对方消息），条目被认领清空。
func TestRunSendCardReleasesDeferredNotify(t *testing.T) {
	logs := captureEvlog(t)
	s := openTestStore(t)
	cli := &fakeCLI{chatAvatarURL: "https://cdn/g.jpg"}
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, dir, "notify", `printf '%s|%s|%s' "$LW_MESSAGE" "$LW_DRAFT" "$LW_ICON" > "$LW_TEST_OUT"`)
	s.NotifyDeferPut([]Message{
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_1", Type: "text", Text: "在吗"},
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_2", Type: "text", Text: "帮我看个问题"},
	}, 99999)

	d1 := filepath.Join(dir, "d1.md")
	d2 := filepath.Join(dir, "d2.md")
	os.WriteFile(d1, []byte("好的，我看下"), 0o644)
	os.WriteFile(d2, []byte("在忙，晚点回你"), 0o644)
	if err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_2", []string{d1, d2}, "帮我看个问题", "张三", "私聊", "12:01", "text", ""); err != nil {
		t.Fatal(err)
	}

	want := "张三（私聊）: 在吗\n张三（私聊）: 帮我看个问题|好的，我看下|https://cdn/g.jpg"
	if got := string(waitForFile(t, out)); got != want {
		t.Errorf("notify message: got %q, want %q", got, want)
	}
	if rang.Load() != 1 {
		t.Errorf("bell rang %d times, want 1", rang.Load())
	}
	if _, ok := s.NotifyDeferClaimChat("om_1"); ok {
		t.Error("deferred entries should be claimed and cleared")
	}
	claims := findLogs(logs(), "notify.claim")
	if len(claims) != 1 || claims[0]["mid"] != "om_2" || claims[0]["n"] != float64(2) ||
		claims[0]["script"] != true || claims[0]["level"] != "INFO" {
		t.Errorf("notify.claim: %v", claims)
	}
}

// 发卡时无延迟条目（已超时弹出/未配置延迟）：不弹通知、不响铃。
func TestRunSendCardNoDeferredNotify(t *testing.T) {
	logs := captureEvlog(t)
	s := openTestStore(t)
	cli := &fakeCLI{}
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, dir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)

	draft := filepath.Join(dir, "d.md")
	os.WriteFile(draft, []byte("好的"), 0o644)
	if err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_x", []string{draft}, "", "", "", "", "text", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Error("notify script ran, want skipped (nothing deferred)")
	}
	if rang.Load() != 0 {
		t.Errorf("bell rang %d times, want 0", rang.Load())
	}
	claims := findLogs(logs(), "notify.claim")
	if len(claims) != 1 || claims[0]["n"] != float64(0) || claims[0]["level"] != "DEBUG" {
		t.Errorf("no-claim should log at DEBUG: %v", claims)
	}
}

// 发卡认领时本人已亲自回复：卡片照发（草稿仍有参考价值）、条目认领清空，
// 但系统通知不再弹——等草稿期间会话可能已被本人处理，弹旧通知只会误导。
func TestRunSendCardSkipsRepliedNotify(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, dir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)
	s.NotifyDeferPut([]Message{
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_1", Type: "text", Text: "在吗", T: "2026-07-17 12:00"},
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_2", Type: "text", Text: "必须是这个", T: "2026-07-17 12:01"},
	}, 99999)
	s.SelfLastUpsert(map[string]string{"oc_a": "2026-07-17 12:02"})

	draft := filepath.Join(dir, "d.md")
	os.WriteFile(draft, []byte("好的"), 0o644)
	if err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_2", []string{draft}, "必须是这个", "张三", "私聊", "12:01", "text", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Error("notify ran for replied chat, want dropped")
	}
	if rang.Load() != 0 {
		t.Errorf("bell rang %d times, want 0", rang.Load())
	}
	if !cli.hasCall("send-card ou_SELF") {
		t.Errorf("card must still be sent: %v", cli.calls)
	}
	if _, ok := s.NotifyDeferClaimChat("om_1"); ok {
		t.Error("deferred entries should be claimed and cleared")
	}
}

// 认领批次部分过期：只弹本人回复之后到达的那条，之前的丢弃。
func TestRunSendCardDropsRepliedKeepsNewer(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, dir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)
	s.NotifyDeferPut([]Message{
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_1", Type: "text", Text: "在吗", T: "2026-07-17 12:00"},
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_2", Type: "text", Text: "必须是这个", T: "2026-07-17 12:02"},
	}, 99999)
	s.SelfLastUpsert(map[string]string{"oc_a": "2026-07-17 12:01"})

	draft := filepath.Join(dir, "d.md")
	os.WriteFile(draft, []byte("好的"), 0o644)
	if err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_2", []string{draft}, "必须是这个", "张三", "私聊", "12:02", "text", ""); err != nil {
		t.Fatal(err)
	}
	if got := string(waitForFile(t, out)); got != "张三（私聊）: 必须是这个" {
		t.Errorf("notify message: got %q", got)
	}
	if rang.Load() != 1 {
		t.Errorf("bell rang %d times, want 1", rang.Load())
	}
}

// stubSendDraftAlert 注入 send-draft 提示弹窗替身（真弹窗会在 macOS 上阻塞测试）。
func stubSendDraftAlert(t *testing.T) *[]string {
	t.Helper()
	var alerts []string
	old := sendDraftAlertFn
	sendDraftAlertFn = func(_ context.Context, _, title, _ string) error {
		alerts = append(alerts, title)
		return nil
	}
	t.Cleanup(func() { sendDraftAlertFn = old })
	return &alerts
}

// send-draft（弹窗「发送」回调）：按 idx 发送 pending 候选并删除 pending，
// 语义与卡片「发送」一致（幂等键 = mid+候选指纹，双端共键不双发；裸 mid 会
// 让同 mid 二次起草的发送被服务端去重吞掉）；成功后按 card_mid 改卡
// 「已发送」（只留所发候选，按钮剥除），改卡成功记 Debug 级 card.done。
func TestRunSendDraft(t *testing.T) {
	logs := captureEvlog(t)
	s := openTestStore(t)
	cli := &fakeCLI{}
	stubSendDraftAlert(t)
	card := RenderDraftCard("om_p1", "私聊", "张三", "", "原消息", []string{"候选一", "候选二"}, "text", "")
	s.PendingPut("om_p1", []string{"候选一", "候选二"}, "text", card, 1)
	s.PendingSetCardMid("om_p1", "om_card_p1")

	if err := RunSendDraft(context.Background(), s, cli, Paths{ConfigDir: t.TempDir()}, "om_p1", 0); err != nil {
		t.Fatal(err)
	}
	done := findLogs(logs(), "card.done")
	if len(done) != 1 || done[0]["mid"] != "om_p1" || done[0]["state"] != "回复已发送" || done[0]["level"] != "DEBUG" {
		t.Errorf("card.done: %v", done)
	}
	key := draftIdemKey("om_p1", "候选一")
	if key == "om_p1" || key == draftIdemKey("om_p1", "候选二") ||
		key == quickIdemKey("om_p1", "候选一") {
		t.Errorf("draft idem key must differ from mid/quick key and vary by candidate: %s", key)
	}
	if !cli.hasCall("reply om_p1 候选一 format=text key=" + key) {
		t.Errorf("reply args wrong: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_p1"); ok {
		t.Error("pending should be deleted after send")
	}
	if !cli.hasCall("patch-card om_card_p1") || !cli.hasCall("回复已发送") {
		t.Errorf("card should be patched to sent: %v", cli.calls)
	}
	for _, c := range cli.calls {
		if strings.HasPrefix(c, "patch-card") &&
			(strings.Contains(c, "button") || strings.Contains(c, "候选二") || !strings.Contains(c, "候选一")) {
			t.Errorf("patched card should keep only sent candidate, no buttons: %s", c)
		}
	}
}

// send-draft 改卡是 best-effort：PATCH 失败不影响发送结果；card_mid 未落库
// （发卡时解析失败/存量 pending）则跳过改卡。
func TestRunSendDraftPatchBestEffort(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{failPatch: true}
	stubSendDraftAlert(t)
	paths := Paths{ConfigDir: t.TempDir()}
	s.PendingPut("om_p3", []string{"候选"}, "text", testCardContent, 1)
	s.PendingSetCardMid("om_p3", "om_card_p3")

	if err := RunSendDraft(context.Background(), s, cli, paths, "om_p3", 0); err != nil {
		t.Fatalf("patch failure must not block send: %v", err)
	}
	if _, _, _, ok := s.PendingGet("om_p3"); ok {
		t.Error("pending should be deleted despite patch failure")
	}

	cli = &fakeCLI{}
	s.PendingPut("om_p4", []string{"候选"}, "text", testCardContent, 1)
	if err := RunSendDraft(context.Background(), s, cli, paths, "om_p4", 0); err != nil {
		t.Fatal(err)
	}
	if cli.hasCall("patch-card") {
		t.Errorf("no card_mid, should skip patch: %v", cli.calls)
	}
}

// send-draft 异常路径：pending 缺失 / idx 越界报错不发送；发送失败保留 pending
// （可回卡片或终端重试），且缺失/失败均有提示弹窗（静默失败会误导）。
func TestRunSendDraftErrors(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	alerts := stubSendDraftAlert(t)
	paths := Paths{ConfigDir: t.TempDir()}
	if err := RunSendDraft(context.Background(), s, cli, paths, "om_none", 0); err == nil {
		t.Error("missing pending should error")
	}
	if cli.hasCall("reply") {
		t.Errorf("no reply expected: %v", cli.calls)
	}

	s.PendingPut("om_p2", []string{"候选一"}, "text", "{}", 1)
	if err := RunSendDraft(context.Background(), s, cli, paths, "om_p2", 1); err == nil {
		t.Error("idx out of range should error")
	}
	cli.failReply = true
	if err := RunSendDraft(context.Background(), s, cli, paths, "om_p2", 0); err == nil {
		t.Error("reply failure should propagate")
	}
	if _, _, _, ok := s.PendingGet("om_p2"); !ok {
		t.Error("pending must be kept after failed send")
	}
	if len(*alerts) != 2 || (*alerts)[0] != "草稿已失效" || (*alerts)[1] != "回复发送失败" {
		t.Errorf("alerts: %v", *alerts)
	}
}

// PendingDelete 失败不再静默：pending 残留会让后续卡片状态难解释，必须留痕
// （行为仍 best-effort：回复已发出，错误只入档不上抛）。
func TestRunSendDraftPendingDeleteFailureLogged(t *testing.T) {
	logs := captureEvlog(t)
	s := openTestStore(t)
	cli := &fakeCLI{}
	stubSendDraftAlert(t)
	s.PendingPut("om_pd", []string{"候选"}, "text", "{}", 1)
	cli.onReply = func() { s.Close() } // PendingGet 之后、PendingDelete 之前关库注错

	if err := RunSendDraft(context.Background(), s, cli, Paths{ConfigDir: t.TempDir()}, "om_pd", 0); err != nil {
		t.Fatalf("delete failure must not fail the send: %v", err)
	}
	if !logsContain(logs(), "pending delete failed") {
		t.Error("pending delete failure should be logged")
	}
}

// 提示横幅自身失败也要留痕：弹窗场景无终端，提示横幅是最后的可观测通道。
func TestResultAlertFailureLogged(t *testing.T) {
	logs := captureEvlog(t)
	s := openTestStore(t)
	old := sendDraftAlertFn
	sendDraftAlertFn = func(context.Context, string, string, string) error { return errors.New("alerter gone") }
	t.Cleanup(func() { sendDraftAlertFn = old })

	if err := RunSendDraft(context.Background(), s, &fakeCLI{}, Paths{ConfigDir: t.TempDir()}, "om_none", 0); err == nil {
		t.Error("missing pending should error")
	}
	if !logsContain(logs(), "result alert failed") {
		t.Error("alert failure should be logged")
	}
}

// send-text（横幅常用语回调）：独立幂等键（≠ mid、随文本互异）、成功删 pending；
// 有卡片时改卡「已快捷回复」（草稿并未发出，不标「已发送」，全部候选正文保留）。
func TestRunSendText(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	stubSendDraftAlert(t)
	card := RenderDraftCard("om_q1", "私聊", "张三", "", "原消息", []string{"候选"}, "text", "")
	s.PendingPut("om_q1", []string{"候选"}, "text", card, 1)
	s.PendingSetCardMid("om_q1", "om_card_q1")
	paths := Paths{ConfigDir: t.TempDir()}

	if err := RunSendText(context.Background(), s, cli, paths, "om_q1", "收到"); err != nil {
		t.Fatal(err)
	}
	key := quickIdemKey("om_q1", "收到")
	if key == "om_q1" || key == quickIdemKey("om_q1", "好的") {
		t.Errorf("idem key must differ from mid and vary by text: %s", key)
	}
	if !cli.hasCall("reply om_q1 收到 format=text key=" + key) {
		t.Errorf("quick reply args wrong: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_q1"); ok {
		t.Error("pending should be deleted after quick reply")
	}
	if !cli.hasCall("patch-card om_card_q1") || !cli.hasCall("已快捷回复") {
		t.Errorf("card should be patched to quick-replied: %v", cli.calls)
	}
	if cli.hasCall("回复已发送") {
		t.Errorf("quick reply must not claim draft was sent: %v", cli.calls)
	}

	// 无 pending（即时/兜底通知场景）：照发、不改卡
	cli = &fakeCLI{}
	if err := RunSendText(context.Background(), s, cli, paths, "om_q3", "收到"); err != nil {
		t.Fatal(err)
	}
	if cli.hasCall("patch-card") {
		t.Errorf("no pending, should skip patch: %v", cli.calls)
	}

	// 失败：pending 保留、错误上抛、弹提示、不改卡
	s.PendingPut("om_q2", []string{"候选"}, "text", testCardContent, 1)
	s.PendingSetCardMid("om_q2", "om_card_q2")
	cli.failReply = true
	if err := RunSendText(context.Background(), s, cli, paths, "om_q2", "收到"); err == nil {
		t.Error("reply failure should propagate")
	}
	if _, _, _, ok := s.PendingGet("om_q2"); !ok {
		t.Error("pending must be kept after failed quick reply")
	}
	if cli.hasCall("patch-card") {
		t.Errorf("failed reply should not patch card: %v", cli.calls)
	}
}

// react（横幅表情回调）：调 ReactAsUser、不碰 pending；坏 emoji type 拒绝。
func TestRunReact(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	stubSendDraftAlert(t)
	s.PendingPut("om_r1", []string{"候选"}, "text", "{}", 1)
	paths := Paths{ConfigDir: t.TempDir()}

	if err := RunReact(context.Background(), cli, paths, "om_r1", "THUMBSUP"); err != nil {
		t.Fatal(err)
	}
	if !cli.hasCall("react om_r1 THUMBSUP") {
		t.Errorf("react args wrong: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_r1"); !ok {
		t.Error("react must not touch pending")
	}
	if err := RunReact(context.Background(), cli, paths, "om_r1", "bad; rm"); err == nil {
		t.Error("invalid emoji type should be rejected")
	}
	cli.failReply = true
	if err := RunReact(context.Background(), cli, paths, "om_r1", "OK"); err == nil {
		t.Error("react failure should propagate")
	}
}

// 空草稿（含只有换行）按序号报错，不入库不发卡。
func TestRunSendCardEmptyDraft(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	dir := t.TempDir()
	p1 := filepath.Join(dir, "d1.md")
	p2 := filepath.Join(dir, "d2.md")
	os.WriteFile(p1, []byte("候选一"), 0o644)
	os.WriteFile(p2, []byte("\n"), 0o644)

	err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_e", []string{p1, p2}, "", "", "", "", "text", "")
	if err == nil || !strings.Contains(err.Error(), "draft 2 is empty") {
		t.Fatalf("want draft 2 empty error, got %v", err)
	}
	if _, _, _, ok := s.PendingGet("om_e"); ok {
		t.Error("pending should not be stored")
	}
	if cli.hasCall("send-card") {
		t.Errorf("card should not be sent: %v", cli.calls)
	}
}

func TestParseBookSlots(t *testing.T) {
	slots, err := ParseBookSlots([]string{"07-22 14:00-15:00", "07-23 09:30-10:00"})
	if err != nil {
		t.Fatal(err)
	}
	if slots[0] != (BookSlot{Date: "07-22", Time: "14:00-15:00"}) || slots[1].Date != "07-23" {
		t.Errorf("parsed: %+v", slots)
	}
	for name, vals := range map[string][]string{
		"空列表":  {},
		"超过3条": {"07-22 14:00-15:00", "07-22 14:00-15:00", "07-22 14:00-15:00", "07-22 14:00-15:00"},
		"缺时段":  {"07-22"},
		"日期格式": {"7-22 14:00-15:00"},
		"多余空格": {"07-22  14:00-15:00"},
	} {
		if _, err := ParseBookSlots(vals); err == nil {
			t.Errorf("%s: want error for %v", name, vals)
		}
	}
}

// room 拒绝飞书 open_id（ou_）：必败参数在发卡入口立即报错，不入库不发卡。
func TestRunSendBookCardRejectsOpenID(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	err := RunSendBookCard(s, cli, "om_ou", []BookSlot{{Date: "07-22", Time: "14:00-15:00"}},
		"会", []string{"alice@corp.com", "ou_abc"}, "", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "ou_abc") {
		t.Fatalf("want ou_ rejection, got %v", err)
	}
	if _, ok := s.BookPendingGet("om_ou"); ok || len(cli.calls) != 0 {
		t.Errorf("nothing should be persisted or sent: %v", cli.calls)
	}
}

// 发卡即固化预订参数入库；卡片经 bot 私发给本人；成功记 card.book_sent 锚点。
func TestRunSendBookCard(t *testing.T) {
	logs := captureEvlog(t)
	s := openTestStore(t)
	cli := &fakeCLI{}
	slots := []BookSlot{{Date: "07-22", Time: "14:00-15:00"}}

	if err := RunSendBookCard(s, cli, "om_sb1", slots, "对齐会", []string{"alice@corp.com"}, "约个会", "张三", "私聊", "12:03"); err != nil {
		t.Fatal(err)
	}
	sent := findLogs(logs(), "card.book_sent")
	if len(sent) != 1 || sent[0]["mid"] != "om_sb1" || sent[0]["slots"] != float64(1) || sent[0]["level"] != "INFO" {
		t.Errorf("card.book_sent: %v", sent)
	}
	bp, ok := s.BookPendingGet("om_sb1")
	if !ok || bp.Title != "对齐会" || len(bp.Slots) != 1 || bp.Participants[0] != "alice@corp.com" {
		t.Fatalf("book pending: ok=%v %+v", ok, bp)
	}
	if bp.Card == "" || !strings.Contains(bp.Card, "会议预约待确认") {
		t.Errorf("card source should be persisted: %q", bp.Card)
	}
	if !cli.hasCall("send-card ou_SELF") || !cli.hasCall("我要预约") {
		t.Errorf("card not sent to self: %v", cli.calls)
	}
}

// # 开头的合法正则原样落盘后会被读取侧（readConfigLines）当整行注释静默跳过：
// 落盘前转义为等价的 \#，校验、落盘、读取三侧同一形态，规则真实生效。
func TestRunIgnoreAddHashPrefix(t *testing.T) {
	paths := Paths{ConfigDir: t.TempDir()}
	if err := RunIgnoreAdd(paths, "#话题"); err != nil {
		t.Fatal(err)
	}
	lines := readConfigLines(filepath.Join(paths.ConfigDir, ignoreFile))
	if len(lines) != 1 {
		t.Fatalf("ignore lines = %q, want 1 effective rule", lines)
	}
	if !regexp.MustCompile(lines[0]).MatchString("#话题 求关注") {
		t.Errorf("escaped pattern %q should match literal #话题", lines[0])
	}
}

// --peek 负值（flag 手误透传）按 0 处理：不在 CatchupGroup 的切片分配处
// panic，照常输出分组结果（peek 列表为空）。
func TestRunCatchupNegativePeek(t *testing.T) {
	s := openTestStore(t)
	f := &listFake{searchResp: chatMsgsResp(false,
		rawMsgJSON("om_cp", "ou_alice", "张三", "积压消息", FmtMinute(time.Now().Unix())))}
	if err := RunCatchup(s, f, Paths{ConfigDir: t.TempDir()}, "24h", -1); err != nil {
		t.Fatal(err)
	}
}
