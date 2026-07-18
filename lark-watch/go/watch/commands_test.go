package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	if err := RunSendCard(s, cli, "om_sc", []string{p1, p2}, "原消息", "张三", "私聊", "12:00", "text"); err != nil {
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

// 空草稿（含只有换行）按序号报错，不入库不发卡。
func TestRunSendCardEmptyDraft(t *testing.T) {
	s := openTestStore(t)
	cli := &fakeCLI{}
	dir := t.TempDir()
	p1 := filepath.Join(dir, "d1.md")
	p2 := filepath.Join(dir, "d2.md")
	os.WriteFile(p1, []byte("候选一"), 0o644)
	os.WriteFile(p2, []byte("\n"), 0o644)

	err := RunSendCard(s, cli, "om_e", []string{p1, p2}, "", "", "", "", "text")
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
