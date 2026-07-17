package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReadNotifyScript(t *testing.T) {
	dir := t.TempDir()
	if got := ReadNotifyScript(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing file: got %q, want empty", got)
	}
	blank := writeConfig(t, dir, "blank", "  \n\t\n")
	if got := ReadNotifyScript(blank); got != "" {
		t.Errorf("blank file: got %q, want empty", got)
	}
	p := writeConfig(t, dir, "notify", "\n  echo hi  \n")
	if got := ReadNotifyScript(p); got != "echo hi" {
		t.Errorf("got %q, want %q", got, "echo hi")
	}
}

// stubBell 静音响铃并记录调用次数（响铃是 IO 边缘，测试不真响）。
func stubBell(t *testing.T) *int {
	t.Helper()
	called := 0
	old := bellFn
	bellFn = func(context.Context) { called++ }
	t.Cleanup(func() { bellFn = old })
	return &called
}

func TestRunNotify(t *testing.T) {
	rang := stubBell(t)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	script := `printf '%s|%s|%s|%s|%s|%s|%s|%s' "$LW_TITLE" "$LW_COUNT" "$LW_FROM" "$LW_CHAT" "$LW_TYPE" "$LW_TEXT" "$LW_LINK" "$LW_MESSAGE" > "$LW_TEST_OUT"`

	RunNotify(context.Background(), script, []Message{
		{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat",
			Link: "lark://applink.feishu.cn/client/chat/open?openChatId=oc_p2p1&position=5"},
		{From: strPtr("张三"), Chat: strPtr("测试群"), Ctype: "group", Type: "text", Text: "帮我看个问题"},
	})

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify output not written: %v", err)
	}
	want := "飞书 P0（2 条）|2|李四||video_chat||" +
		"lark://applink.feishu.cn/client/chat/open?openChatId=oc_p2p1&position=5|" +
		"李四（私聊）: 发起了音视频会议\n张三（测试群）: 帮我看个问题"
	if string(b) != want {
		t.Errorf("got %q, want %q", b, want)
	}
	if *rang != 1 {
		t.Errorf("bell rang %d times, want 1", *rang)
	}
}

func TestRunNotifyCommand(t *testing.T) {
	rang := stubBell(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, dir, "notify",
		`printf '%s|%s|%s|%s' "$LW_TITLE" "$LW_MESSAGE" "$LW_SUMMARY" "$LW_LINK" > "$LW_TEST_OUT"`)

	err := RunNotifyCommand(context.Background(), Paths{ConfigDir: dir},
		"Meeting", "3 点的会开始了", "lark://applink.feishu.cn/client/chat/open?openChatId=oc_x")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify output not written: %v", err)
	}
	want := "Meeting|3 点的会开始了|3 点的会开始了|lark://applink.feishu.cn/client/chat/open?openChatId=oc_x"
	if string(b) != want {
		t.Errorf("got %q, want %q", b, want)
	}
	if *rang != 1 {
		t.Errorf("bell rang %d times, want 1", *rang)
	}
}
