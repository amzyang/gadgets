package watch

import (
	"context"
	"errors"
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
func stubVCDialog(t *testing.T) chan [4]string {
	t.Helper()
	calls := make(chan [4]string, 4)
	old := vcDialogFn
	vcDialogFn = func(_ context.Context, title, message, link, icon string) error {
		calls <- [4]string{title, message, link, icon}
		return nil
	}
	t.Cleanup(func() { vcDialogFn = old })
	return calls
}

// waitForDialog 等待弹窗替身被调用（RunNotifyVC 可能在 goroutine 里跑）。
func waitForDialog(t *testing.T, calls chan [4]string) [4]string {
	t.Helper()
	select {
	case got := <-calls:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("vc dialog not shown")
		return [4]string{}
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

	RunNotify(context.Background(), t.TempDir(), `touch "$LW_TEST_OUT"`, []Message{
		{From: strPtr("张三"), Ctype: "p2p", Type: "text", Text: "在吗"},
	}, "")

	if _, err := os.Stat(out); err == nil {
		t.Error("notify script ran, want suppressed")
	}
	if rang.Load() != 0 {
		t.Errorf("bell rang %d times, want 0", rang.Load())
	}
}

// assertMids 断言事件记录的 mids attr 恰为期望列表（JSON 数组解析为 []any）。
func assertMids(t *testing.T, rec map[string]any, want ...string) {
	t.Helper()
	got, _ := rec["mids"].([]any)
	if len(got) != len(want) {
		t.Fatalf("mids: got %v, want %v", rec["mids"], want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mids[%d] = %v, want %q", i, got[i], want[i])
		}
	}
}

// 抑制必须带 mids 入档：事后审计「横幅为什么没弹」要能按 mid 反查到具体消息，
// 不能只剩无关联的 stderr 文本行。
func TestNotifySuppressEvent(t *testing.T) {
	logs := captureEvlog(t)
	stubBell(t)
	stubProbes(t, "com.electron.lark", 3)

	RunNotify(context.Background(), t.TempDir(), "true", []Message{
		{Mid: "om_s1", From: strPtr("张三"), Ctype: "p2p", Type: "text", Text: "在吗"},
	}, "")

	recs := findLogs(logs(), "notify.suppress")
	if len(recs) != 1 || recs[0]["level"] != "INFO" {
		t.Fatalf("notify.suppress: %v", recs)
	}
	assertMids(t, recs[0], "om_s1")
}

// 发送失败按 kind（p0/vc/draft）+ mids + err 入档 Error 级：
// `level=="ERROR"` 即审计失败面板，不再只能靠时间戳对齐前一条 notify.*。
func TestNotifyFailEvents(t *testing.T) {
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	batch := []Message{{Mid: "om_f1", From: strPtr("张三"), Ctype: "p2p", Type: "text", Text: "在吗"}}

	t.Run("p0 脚本失败", func(t *testing.T) {
		logs := captureEvlog(t)
		RunNotify(context.Background(), t.TempDir(), "exit 1", batch, "")
		recs := findLogs(logs(), "notify.fail")
		if len(recs) != 1 || recs[0]["kind"] != "p0" || recs[0]["level"] != "ERROR" || recs[0]["err"] == "" {
			t.Fatalf("notify.fail: %v", recs)
		}
		assertMids(t, recs[0], "om_f1")
	})

	t.Run("vc 内置横幅失败", func(t *testing.T) {
		logs := captureEvlog(t)
		old := vcDialogFn
		vcDialogFn = func(context.Context, string, string, string, string) error { return errors.New("boom") }
		t.Cleanup(func() { vcDialogFn = old })
		RunNotifyVC(context.Background(), Paths{ConfigDir: t.TempDir()}, batch, "")
		recs := findLogs(logs(), "notify.fail")
		if len(recs) != 1 || recs[0]["kind"] != "vc" || recs[0]["level"] != "ERROR" {
			t.Fatalf("notify.fail vc: %v", recs)
		}
		assertMids(t, recs[0], "om_f1")
	})

	t.Run("draft 横幅缺 alerter", func(t *testing.T) {
		logs := captureEvlog(t)
		stubAlerter(t, "")
		StartNotify(context.Background(), t.TempDir(), "", batch, "草稿", "om_f1", "")
		recs := findLogs(logs(), "notify.fail")
		if len(recs) != 1 || recs[0]["kind"] != "draft" || recs[0]["level"] != "ERROR" {
			t.Fatalf("notify.fail draft: %v", recs)
		}
		assertMids(t, recs[0], "om_f1")
	})
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
	script := `printf '%s|%s|%s|%s|%s|%s|%s|%s|%s' "$LW_TITLE" "$LW_COUNT" "$LW_FROM" "$LW_CHAT" "$LW_TYPE" "$LW_TEXT" "$LW_LINK" "$LW_MESSAGE" "$LW_ICON" > "$LW_TEST_OUT"`

	RunNotify(context.Background(), t.TempDir(), script, []Message{
		{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat",
			Link: "lark://applink.feishu.cn/client/chat/open?openChatId=oc_p2p1&position=5"},
		{From: strPtr("张三"), Chat: strPtr("测试群"), Ctype: "group", Type: "text", Text: "帮我看个问题"},
	}, "https://cdn/u.png")

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify output not written: %v", err)
	}
	want := "飞书 P0（2 条）|2|李四||video_chat||" +
		"lark://applink.feishu.cn/client/chat/open?openChatId=oc_p2p1&position=5|" +
		"李四（私聊）: 发起了音视频会议\n张三（测试群）: 帮我看个问题|https://cdn/u.png"
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
		want  [4]string
	}{
		{"单条", []Message{
			{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat", Link: link1},
		}, [4]string{"📞 音视频会议", "李四（私聊）: 发起了音视频会议", link1, "https://cdn/u.png"}},
		{"多条", []Message{
			{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat", Link: link1},
			{From: strPtr("张三"), Chat: strPtr("测试群"), Ctype: "group", Type: "vc_meeting", Link: link2},
		}, [4]string{"📞 音视频会议（2 条）",
			"李四（私聊）: 发起了音视频会议\n张三（测试群）: 发起了音视频会议", link1, "https://cdn/u.png"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rang := stubBell(t)
			stubProbes(t, "net.kovidgoyal.kitty", 0)
			calls := stubVCDialog(t)

			RunNotifyVC(context.Background(), Paths{ConfigDir: t.TempDir()}, c.batch, "https://cdn/u.png")

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
		`printf '%s|%s|%s|%s|%s|%s' "$LW_TITLE" "$LW_COUNT" "$LW_TYPE" "$LW_LINK" "$LW_MESSAGE" "$LW_ICON" > "$LW_TEST_OUT"`)

	RunNotifyVC(context.Background(), Paths{ConfigDir: dir}, []Message{
		{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat",
			Link: "lark://applink.feishu.cn/client/chat/open?openChatId=oc_p2p1&position=5"},
	}, "https://cdn/u.png")

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify-vc output not written: %v", err)
	}
	want := "📞 音视频会议|1|video_chat|" +
		"lark://applink.feishu.cn/client/chat/open?openChatId=oc_p2p1&position=5|" +
		"李四（私聊）: 发起了音视频会议|https://cdn/u.png"
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
	}, "")

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

// alerter 草稿横幅：动作下拉 = 发送＋常用语＋表情（内置默认），「发送」回调
// send-draft、常用语回调 send-text、表情回调 react、点正文 = 复制并跳转、
// 60 秒超时。标签与值全走位置参数（≥10 花括号），脚本文本零用户内容；
// 未安装 alerter ok=false（通知跳过）。
func TestAlerterDraftArgs(t *testing.T) {
	stubAlerter(t, "/opt/bin/alerter")
	// 空目录 = 内置默认动作：收到 / 好的，稍后回复 / 👍
	script, args, ok := alerterDraftArgs(t.TempDir(), "标题", "摘要", "lark://x", "草稿内容", "om_1", "https://cdn/a.png")
	if !ok {
		t.Fatal("want ok")
	}
	for _, want := range []string{
		`out=$("$1" --title "$2" --message "$3" --actions "$8" --close-label "忽略" --timeout 60 --ignore-dnd --app-icon "${15}") || exit $?`,
		`"发送") exec "$4" send-draft --mid "$5" ;;`,
		`"$9") exec "$4" send-text --mid "$5" --text "${10}" ;;`,
		`"${11}") exec "$4" send-text --mid "$5" --text "${12}" ;;`,
		`"${13}") exec "$4" react --mid "$5" --emoji "${14}" ;;`,
		"@CONTENTCLICKED", "pbcopy",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "收到") || strings.Contains(script, "THUMBSUP") {
		t.Errorf("script must not contain user content:\n%s", script)
	}
	want := []string{"/opt/bin/alerter", "标题", "摘要\n\n—— 回复草稿 ——\n草稿内容",
		args[3], "om_1", "草稿内容", "lark://x", "发送,收到,好的，稍后回复,👍 回应",
		"收到", "收到", "好的，稍后回复", "好的，稍后回复", "👍 回应", "THUMBSUP",
		"https://cdn/a.png"}
	if len(args) != len(want) {
		t.Fatalf("args len %d, want %d: %v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}

	// icon 空时不得带 --app-icon 旗标（--app-icon "" 行为未定义）
	script, _, _ = alerterDraftArgs(t.TempDir(), "标题", "摘要", "lark://x", "草稿内容", "om_1", "")
	if strings.Contains(script, "--app-icon") {
		t.Errorf("empty icon must omit flag:\n%s", script)
	}

	stubAlerter(t, "")
	if _, _, ok := alerterDraftArgs(t.TempDir(), "t", "m", "l", "d", "om_1", ""); ok {
		t.Error("no alerter must return ok=false")
	}
}

// alerter 通用/VC 横幅：有 mid 带快捷动作、无 mid 退回 plain 版（失败提示等
// 无消息上下文场景）；复制内容为通知正文；VC 点正文或「加入」即入会。
func TestAlerterGenericVCArgs(t *testing.T) {
	stubAlerter(t, "/opt/bin/alerter")
	dir := t.TempDir()
	script, args, ok := alerterGenericArgs(dir, "t", "msg", "lark://x", "", "")
	if !ok || script != alerterPlainScript("") || len(args) != 6 || args[3] != "msg" || args[5] != "" {
		t.Errorf("no-mid generic: ok=%v script=%q args=%v", ok, script, args)
	}
	// alerter ≥26 只认双横线长旗标;调用失败须透传退出码(不再被 case 吞掉)。
	if want := `out=$("$1" --title "$2" --message "$3" --actions "复制" --close-label "忽略" --timeout 60 --ignore-dnd) || exit $?`; !strings.Contains(script, want) {
		t.Errorf("plain script missing %q:\n%s", want, script)
	}
	// plain 带 icon：旗标引用 $6、args 末位携带 URL
	script, args, _ = alerterGenericArgs(dir, "t", "msg", "lark://x", "", "https://cdn/a.png")
	if !strings.Contains(script, `--ignore-dnd --app-icon "$6") || exit $?`) || args[5] != "https://cdn/a.png" {
		t.Errorf("plain with icon: script=%q args=%v", script, args)
	}

	script, args, ok = alerterGenericArgs(dir, "t", "msg", "lark://x", "om_9", "https://cdn/a.png")
	if !ok || len(args) != 15 || args[6] != "om_9" || args[7] != "复制,收到,好的，稍后回复,👍 回应" || args[14] != "https://cdn/a.png" {
		t.Fatalf("mid generic: ok=%v args=%v", ok, args)
	}
	for _, want := range []string{
		`"复制") printf '%s' "$4" | pbcopy ;;`,
		`"$9") exec "$6" send-text --mid "$7" --text "${10}" ;;`,
		`"${13}") exec "$6" react --mid "$7" --emoji "${14}" ;;`,
		`--ignore-dnd --app-icon "${15}") || exit $?`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}

	script, args, ok = alerterVCArgs("t", "msg", "lark://vc", "https://cdn/g.jpg")
	if !ok || !strings.Contains(script, `"加入"|"@CONTENTCLICKED"`) || args[3] != "lark://vc" || args[4] != "https://cdn/g.jpg" {
		t.Errorf("vc: ok=%v args=%v", ok, args)
	}
	if want := `out=$("$1" --title "$2" --message "$3" --actions "加入" --close-label "忽略" --timeout 60 --ignore-dnd --app-icon "$5") || exit $?`; !strings.Contains(script, want) {
		t.Errorf("vc script missing %q:\n%s", want, script)
	}
}

// startShellCmd 须捕捉秒退失败（旗标不兼容等启动即败场景），长驻横幅照常放行。
// 回归背景：alerter 26 改用双横线旗标，旧调用瞬间 exit 64，但 Start 成功、
// case 吞掉空输出，横幅从未出现且日志零错误——失败必须在启动窗口内可观察。
func TestStartShellCmdEarlyExit(t *testing.T) {
	if err := startShellCmd("exit 64", nil); err == nil {
		t.Error("early non-zero exit should surface as error")
	}
	if err := startShellCmd("sleep 2", nil); err != nil {
		t.Errorf("long-lived command should start cleanly: %v", err)
	}
}

// 内置通知硬依赖 alerter：未装时返回安装指引错误（不再有 osascript 弹窗兜底），
// 自动通知经调用方 logf、notify 子命令直接报给用户。
func TestBuiltinNotifyNoAlerter(t *testing.T) {
	stubAlerter(t, "")
	for name, err := range map[string]error{
		"generic": builtinNotify(context.Background(), t.TempDir(), "t", "m", "lark://x", "", ""),
		"vc":      builtinNotifyVC(context.Background(), "t", "m", "lark://x", ""),
	} {
		if err == nil || !strings.Contains(err.Error(), "alerter") {
			t.Errorf("%s: want alerter-missing error, got %v", name, err)
		}
	}
}

// LoadNotifyScript：缺失 = 内置横幅默认开（零配置）；空白/off = 总开关关闭；
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
