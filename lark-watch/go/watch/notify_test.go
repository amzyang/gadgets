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

// stubProbes 注入 frontmost / idle 探测替身（系统探测是 IO 边缘，测试不真探）。
func stubProbes(t *testing.T, bundleID string, idleSecs float64) {
	t.Helper()
	oldF, oldI := frontmostBundleID, hidIdleSecs
	frontmostBundleID = func(context.Context) string { return bundleID }
	hidIdleSecs = func(context.Context) float64 { return idleSecs }
	t.Cleanup(func() { frontmostBundleID, hidIdleSecs = oldF, oldI })
}

func TestShouldSuppressNotify(t *testing.T) {
	cases := []struct {
		name     string
		bundleID string
		idleSecs float64
		want     bool
	}{
		{"飞书标准版前台且活跃", "com.electron.lark", 3, true},
		{"Lark 国际版前台且活跃", "com.larksuite.larkApp", 3, true},
		{"KA 定制版（Lingxi）前台且活跃", "com.dancesuite.dance.ka.sagtjy516.mac", 3, true},
		{"飞书前台但人已走开", "com.electron.lark", 300, false},
		{"其他 app 前台", "net.kovidgoyal.kitty", 3, false},
		{"frontmost 探测失败", "", 3, false},
		{"idle 探测失败", "com.electron.lark", -1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubProbes(t, c.bundleID, c.idleSecs)
			if got := shouldSuppressNotify(context.Background()); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestRunNotifySuppressed(t *testing.T) {
	rang := stubBell(t)
	stubProbes(t, "com.electron.lark", 3)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)

	RunNotify(context.Background(), `touch "$LW_TEST_OUT"`, []Message{
		{From: strPtr("张三"), Ctype: "p2p", Type: "text", Text: "在吗"},
	})

	if _, err := os.Stat(out); err == nil {
		t.Error("notify script ran, want suppressed")
	}
	if *rang != 0 {
		t.Errorf("bell rang %d times, want 0", *rang)
	}
}

func TestParseBundleID(t *testing.T) {
	got := parseBundleID("\"CFBundleIdentifier\"=\"net.kovidgoyal.kitty\"\n")
	if got != "net.kovidgoyal.kitty" {
		t.Errorf("got %q, want %q", got, "net.kovidgoyal.kitty")
	}
	if got := parseBundleID("no such key"); got != "" {
		t.Errorf("garbage input: got %q, want empty", got)
	}
}

func TestParseHIDIdleSecs(t *testing.T) {
	got := parseHIDIdleSecs(`    | | |   "HIDIdleTime" = 462824375` + "\n")
	if got != 0.462824375 {
		t.Errorf("got %v, want 0.462824375", got)
	}
	if got := parseHIDIdleSecs("no such key"); got != -1 {
		t.Errorf("garbage input: got %v, want -1", got)
	}
}

func TestRunNotify(t *testing.T) {
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
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
