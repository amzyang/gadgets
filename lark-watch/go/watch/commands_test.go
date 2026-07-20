package watch

import (
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

	if err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_sc", []string{p1, p2}, "原消息", "张三", "私聊", "12:00", "text"); err != nil {
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

// send-card 发卡后释放同会话的延迟通知：脚本收到聚合摘要，条目被认领清空。
func TestRunSendCardReleasesDeferredNotify(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, dir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)
	s.NotifyDeferPut([]Message{
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_1", Type: "text", Text: "在吗"},
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_2", Type: "text", Text: "帮我看个问题"},
	}, 99999)

	draft := filepath.Join(dir, "d.md")
	os.WriteFile(draft, []byte("好的，我看下"), 0o644)
	if err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_2", []string{draft}, "帮我看个问题", "张三", "私聊", "12:01", "text"); err != nil {
		t.Fatal(err)
	}

	want := "张三（私聊）: 在吗\n张三（私聊）: 帮我看个问题"
	if got := string(waitForFile(t, out)); got != want {
		t.Errorf("notify message: got %q, want %q", got, want)
	}
	if rang.Load() != 1 {
		t.Errorf("bell rang %d times, want 1", rang.Load())
	}
	if _, ok := s.NotifyDeferClaimChat("om_1"); ok {
		t.Error("deferred entries should be claimed and cleared")
	}
}

// 发卡时无延迟条目（已超时弹出/未配置延迟）：不弹通知、不响铃。
func TestRunSendCardNoDeferredNotify(t *testing.T) {
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
	if err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_x", []string{draft}, "", "", "", "", "text"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Error("notify script ran, want skipped (nothing deferred)")
	}
	if rang.Load() != 0 {
		t.Errorf("bell rang %d times, want 0", rang.Load())
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

	err := RunSendCard(s, cli, Paths{ConfigDir: dir}, "om_e", []string{p1, p2}, "", "", "", "", "text")
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
