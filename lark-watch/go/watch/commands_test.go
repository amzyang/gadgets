package watch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// send-card 多候选：逐个读文件、pending 入库多条、卡片含各候选的发送按钮。
func TestRunSendCardMulti(t *testing.T) {
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
	cli := &fakeCLI{}
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, dir, "notify", `printf '%s|%s' "$LW_MESSAGE" "$LW_DRAFT" > "$LW_TEST_OUT"`)
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

	want := "张三（私聊）: 在吗\n张三（私聊）: 帮我看个问题|好的，我看下"
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
	sendDraftAlertFn = func(_ context.Context, title, _, _, _ string) error {
		alerts = append(alerts, title)
		return nil
	}
	t.Cleanup(func() { sendDraftAlertFn = old })
	return &alerts
}

// send-draft（弹窗「发送」回调）：按 idx 发送 pending 候选并删除 pending，
// 语义与卡片「发送」一致（幂等键 = 源消息 mid）。
func TestRunSendDraft(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	stubSendDraftAlert(t)
	s.PendingPut("om_p1", []string{"候选一", "候选二"}, "text", "{}", 1)

	if err := RunSendDraft(context.Background(), s, cli, "om_p1", 0); err != nil {
		t.Fatal(err)
	}
	if !cli.hasCall("reply om_p1 候选一 format=text") {
		t.Errorf("reply args wrong: %v", cli.calls)
	}
	if _, _, _, ok := s.PendingGet("om_p1"); ok {
		t.Error("pending should be deleted after send")
	}
}

// send-draft 异常路径：pending 缺失 / idx 越界报错不发送；发送失败保留 pending
// （可回卡片或终端重试），且缺失/失败均有提示弹窗（静默失败会误导）。
func TestRunSendDraftErrors(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	alerts := stubSendDraftAlert(t)
	if err := RunSendDraft(context.Background(), s, cli, "om_none", 0); err == nil {
		t.Error("missing pending should error")
	}
	if cli.hasCall("reply") {
		t.Errorf("no reply expected: %v", cli.calls)
	}

	s.PendingPut("om_p2", []string{"候选一"}, "text", "{}", 1)
	if err := RunSendDraft(context.Background(), s, cli, "om_p2", 1); err == nil {
		t.Error("idx out of range should error")
	}
	cli.failReply = true
	if err := RunSendDraft(context.Background(), s, cli, "om_p2", 0); err == nil {
		t.Error("reply failure should propagate")
	}
	if _, _, _, ok := s.PendingGet("om_p2"); !ok {
		t.Error("pending must be kept after failed send")
	}
	if len(*alerts) != 2 || (*alerts)[0] != "草稿已失效" || (*alerts)[1] != "回复发送失败" {
		t.Errorf("alerts: %v", *alerts)
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
