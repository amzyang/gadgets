package watch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
// 原子计数：dispatchNotify/flushDueNotify 在 goroutine 里响铃，与断言读并发，
// 文件系统轮询不构成 happens-before 边，读写都必须走 atomic。
func stubBell(t *testing.T) *atomic.Int32 {
	t.Helper()
	called := new(atomic.Int32)
	old := bellFn
	bellFn = func(context.Context) { called.Add(1) }
	t.Cleanup(func() { bellFn = old })
	return called
}

// stubProbes 注入 frontmost / idle 探测替身（系统探测是 IO 边缘，测试不真探）。
func stubProbes(t *testing.T, bundleID string, idleSecs float64) {
	t.Helper()
	oldF, oldI := frontmostBundleID, hidIdleSecs
	frontmostBundleID = func(context.Context) string { return bundleID }
	hidIdleSecs = func(context.Context) float64 { return idleSecs }
	t.Cleanup(func() { frontmostBundleID, hidIdleSecs = oldF, oldI })
}

// stubVCDialog 注入内置 VC 弹窗替身并以 channel 记录调用参数
// （poller 侧经 go 异步调用，channel 才能安全跨 goroutine 断言）。
func stubVCDialog(t *testing.T) chan [3]string {
	t.Helper()
	calls := make(chan [3]string, 4)
	old := vcDialogFn
	vcDialogFn = func(_ context.Context, title, message, link string) error {
		calls <- [3]string{title, message, link}
		return nil
	}
	t.Cleanup(func() { vcDialogFn = old })
	return calls
}

// waitForDialog 等待弹窗替身被调用（RunNotifyVC 可能在 goroutine 里跑）。
func waitForDialog(t *testing.T, calls chan [3]string) [3]string {
	t.Helper()
	select {
	case got := <-calls:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("vc dialog not shown")
		return [3]string{}
	}
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
	if rang.Load() != 0 {
		t.Errorf("bell rang %d times, want 0", rang.Load())
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
	if rang.Load() != 1 {
		t.Errorf("bell rang %d times, want 1", rang.Load())
	}
}

// 未配置 notify-vc 时走内置弹窗：标题（多条带条数）、每条一行摘要、link 取首条。
func TestRunNotifyVCBuiltin(t *testing.T) {
	link1 := "lark://applink.feishu.cn/client/chat/open?openChatId=oc_p2p1&position=5"
	link2 := "lark://applink.feishu.cn/client/chat/open?openChatId=oc_a&position=9"
	cases := []struct {
		name  string
		batch []Message
		want  [3]string
	}{
		{"单条", []Message{
			{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat", Link: link1},
		}, [3]string{"📞 音视频会议", "李四（私聊）: 发起了音视频会议", link1}},
		{"多条", []Message{
			{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat", Link: link1},
			{From: strPtr("张三"), Chat: strPtr("测试群"), Ctype: "group", Type: "vc_meeting", Link: link2},
		}, [3]string{"📞 音视频会议（2 条）",
			"李四（私聊）: 发起了音视频会议\n张三（测试群）: 发起了音视频会议", link1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rang := stubBell(t)
			stubProbes(t, "net.kovidgoyal.kitty", 0)
			calls := stubVCDialog(t)

			RunNotifyVC(context.Background(), Paths{ConfigDir: t.TempDir()}, c.batch)

			if got := waitForDialog(t, calls); got != c.want {
				t.Errorf("dialog args:\n got %q\nwant %q", got, c.want)
			}
			if rang.Load() != 1 {
				t.Errorf("bell rang %d times, want 1", rang.Load())
			}
		})
	}
}

// notify-vc 配置脚本覆盖内置弹窗：LW_* 环境同通用批次，仅标题换音视频会议。
func TestRunNotifyVCScript(t *testing.T) {
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	calls := stubVCDialog(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, dir, "notify-vc",
		`printf '%s|%s|%s|%s|%s' "$LW_TITLE" "$LW_COUNT" "$LW_TYPE" "$LW_LINK" "$LW_MESSAGE" > "$LW_TEST_OUT"`)

	RunNotifyVC(context.Background(), Paths{ConfigDir: dir}, []Message{
		{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat",
			Link: "lark://applink.feishu.cn/client/chat/open?openChatId=oc_p2p1&position=5"},
	})

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify-vc output not written: %v", err)
	}
	want := "📞 音视频会议|1|video_chat|" +
		"lark://applink.feishu.cn/client/chat/open?openChatId=oc_p2p1&position=5|" +
		"李四（私聊）: 发起了音视频会议"
	if string(b) != want {
		t.Errorf("got %q, want %q", b, want)
	}
	if len(calls) != 0 {
		t.Error("builtin dialog called, want notify-vc script only")
	}
	if rang.Load() != 1 {
		t.Errorf("bell rang %d times, want 1", rang.Load())
	}
}

// 前台抑制对 VC 同样生效：不响铃、不弹窗。
func TestRunNotifyVCSuppressed(t *testing.T) {
	rang := stubBell(t)
	stubProbes(t, "com.electron.lark", 3)
	calls := stubVCDialog(t)

	RunNotifyVC(context.Background(), Paths{ConfigDir: t.TempDir()}, []Message{
		{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat", Link: "lark://x"},
	})

	if len(calls) != 0 {
		t.Error("dialog shown, want suppressed")
	}
	if rang.Load() != 0 {
		t.Errorf("bell rang %d times, want 0", rang.Load())
	}
}

// stubAlerter 注入 alerter 探测替身（macOS 开发机装有 alerter 时测试不能真弹横幅）。
func stubAlerter(t *testing.T, path string) {
	t.Helper()
	old := lookAlerter
	lookAlerter = func() string { return path }
	t.Cleanup(func() { lookAlerter = old })
}

// alerter 草稿横幅：「发送」回调 send-draft、点正文 = 复制并跳转、60 秒超时；
// 位置参数序与脚本引用一致；未安装 alerter 回退（ok=false）。
func TestAlerterDraftArgs(t *testing.T) {
	stubAlerter(t, "/opt/bin/alerter")
	script, args, ok := alerterDraftArgs("标题", "摘要", "lark://x", "草稿内容", "om_1")
	if !ok {
		t.Fatal("want ok")
	}
	for _, want := range []string{`-actions "发送"`, `-closeLabel "忽略"`, "-timeout 60",
		"send-draft --mid", "@CONTENTCLICKED", "pbcopy"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if len(args) != 7 || args[0] != "/opt/bin/alerter" || args[1] != "标题" ||
		args[2] != "摘要\n\n—— 回复草稿 ——\n草稿内容" || args[4] != "om_1" ||
		args[5] != "草稿内容" || args[6] != "lark://x" {
		t.Errorf("args: %v", args)
	}

	stubAlerter(t, "")
	if _, _, ok := alerterDraftArgs("t", "m", "l", "d", "om_1"); ok {
		t.Error("no alerter should fall back to osascript")
	}
}

// alerter 通用/VC 横幅：复制内容优先候选话术；VC 点正文或「加入」即入会。
func TestAlerterGenericVCArgs(t *testing.T) {
	stubAlerter(t, "/opt/bin/alerter")
	script, args, ok := alerterGenericArgs("t", "msg", "lark://x", "")
	if !ok || !strings.Contains(script, `-actions "复制"`) || args[3] != "msg" {
		t.Errorf("generic: ok=%v args=%v", ok, args)
	}
	if _, args, _ := alerterGenericArgs("t", "msg", "", "话术"); args[3] != "话术" {
		t.Errorf("draft should win copy text: %v", args)
	}
	script, args, ok = alerterVCArgs("t", "msg", "lark://vc")
	if !ok || !strings.Contains(script, `"加入"|"@CONTENTCLICKED"`) || args[3] != "lark://vc" {
		t.Errorf("vc: ok=%v args=%v", ok, args)
	}
}

// 草稿弹窗：正文展示消息摘要＋候选①全文；「忽略/复制并跳转/发送」三键，
// 回车 = 发送、忽略兼任 cancel button（Esc 即忽略）、60 秒超时；「发送」回调
// send-draft、「复制并跳转」复制候选①并 open applink；argv 序与脚本 item 引用一致。
func TestBuiltinDraftNotifyArgs(t *testing.T) {
	lines, argv, ok := builtinDraftNotifyArgs("标题", "摘要", "lark://x", "草稿内容", "om_1")
	if !ok {
		t.Fatal("want ok")
	}
	script := strings.Join(lines, "\n")
	for _, want := range []string{
		`{"忽略", "复制并跳转", "发送"}`, `default button "发送"`,
		`cancel button "忽略"`, "giving up after 60",
		"send-draft --mid", "set the clipboard to (item 3 of argv)",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if len(argv) != 6 || argv[0] != "摘要\n\n—— 回复草稿 ——\n草稿内容" ||
		argv[1] != "标题" || argv[2] != "草稿内容" || argv[4] != "om_1" || argv[5] != "lark://x" {
		t.Errorf("argv: %v", argv)
	}
	if _, _, ok := builtinDraftNotifyArgs("t", "m", "lark://x", "", "om_1"); ok {
		t.Error("empty draft should fall back to generic popup")
	}
}

// LoadNotifyScript：缺失 = 内置弹窗默认开（零配置）；空白/off = 总开关关闭；
// 其余 = 自定义脚本。
func TestLoadNotifyScript(t *testing.T) {
	dir := t.TempDir()
	if script, enabled := LoadNotifyScript(dir); script != "" || !enabled {
		t.Errorf("missing file: got (%q, %v), want builtin enabled", script, enabled)
	}
	writeConfig(t, dir, "notify", "off\n")
	if script, enabled := LoadNotifyScript(dir); script != "" || enabled {
		t.Errorf("off: got (%q, %v), want disabled", script, enabled)
	}
	writeConfig(t, dir, "notify", "  \n")
	if _, enabled := LoadNotifyScript(dir); enabled {
		t.Error("blank file should disable notifications")
	}
	writeConfig(t, dir, "notify", `echo hi`)
	if script, enabled := LoadNotifyScript(dir); script != "echo hi" || !enabled {
		t.Errorf("script: got (%q, %v)", script, enabled)
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
	if rang.Load() != 1 {
		t.Errorf("bell rang %d times, want 1", rang.Load())
	}
}
