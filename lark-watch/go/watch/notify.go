package watch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ReadNotifyScript 读取通知命令脚本；文件缺失或全空白视为未配置（notify-vc 用，
// notify 总配置走 LoadNotifyScript）。
func ReadNotifyScript(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// notifyOff 是 notify 配置的关闭哨兵。
const notifyOff = "off"

// LoadNotifyScript 解析 notify 总配置：返回自定义脚本与通知开关。
// 文件缺失 = 零配置默认：内置弹窗（弹窗模板内嵌于二进制，无需任何本地文件）；
// 内容空白或 "off" = 关闭通知（总开关）；其余 = 自定义脚本（sh -c，LW_* 环境）。
func LoadNotifyScript(configDir string) (script string, enabled bool) {
	b, err := os.ReadFile(filepath.Join(configDir, "notify"))
	if err != nil {
		return "", true
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == notifyOff {
		return "", false
	}
	return s, true
}

// 批次通知的标题基串：通用 P0 与音视频会议专用弹窗各一。
const (
	p0NotifyTitle = "飞书 P0"
	vcNotifyTitle = "📞 音视频会议"
)

// bellFn 是响铃入口，可注入测试替身（响铃是 IO 边缘）。
var bellFn = ringBell

// lookAlerter 探测 PATH 里的 alerter（github.com/vjeantet/alerter，
// brew install alerter）：装了则内置通知走通知中心横幅（点横幅正文即动作、
// 不抢焦点），未装返回 "" 回退 osascript 弹窗。每次弹窗现探测，装完即生效。
// var 便于测试注入（macOS 开发机装有 alerter 时测试不能真弹横幅）。
var lookAlerter = func() string {
	p, err := exec.LookPath("alerter")
	if err != nil {
		return ""
	}
	return p
}

// vcDialogFn 是内置音视频会议弹窗入口，可注入测试替身（osascript 是 IO 边缘）。
var vcDialogFn = builtinNotifyVC

// sendDraftAlertFn 是 send-draft 结果提示弹窗入口，可注入测试替身
// （IO 边缘：macOS 开发机跑测试不能真弹窗阻塞）。
var sendDraftAlertFn = builtinNotify

// ringBell 响铃提醒（内置自 ~/.local/bin/bell 的 always 逻辑）：
// 终端 bell 优先，无控制终端（daemon/Monitor 场景）回退 osascript beep；
// SSH 会话静默。
func ringBell(ctx context.Context) {
	if os.Getenv("SSH_CONNECTION") != "" {
		return
	}
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		_, werr := tty.Write([]byte{'\a'})
		tty.Close()
		if werr == nil {
			return
		}
	}
	exec.CommandContext(ctx, "osascript", "-e", "beep").Run()
}

// larkBundleMarkers 识别飞书系客户端的 bundle id 子串：
// electron.lark（飞书标准版）、larksuite（Lark 国际版）、
// dancesuite（字节 KA 定制版前缀，如高途 Lingxi）。
var larkBundleMarkers = []string{"electron.lark", "larksuite", "dancesuite"}

// suppressIdleMaxSecs：HIDIdleTime 超过该值视为人已离开（锁屏/走开），照常通知。
const suppressIdleMaxSecs = 120

// frontmostBundleID / hidIdleSecs 是系统探测入口，可注入测试替身（IO 边缘）。
var (
	frontmostBundleID = lsappinfoFrontBundleID
	hidIdleSecs       = ioregHIDIdleSecs
)

// shouldSuppressNotify：飞书处于前台且用户在机器前活跃时跳过系统提示——
// 消息本人已看到，弹窗纯属打扰。任一探测失败即 false（fail-open，
// 宁可多打扰不可漏消息）；锁屏/走开时 frontmost 仍是锁屏前的 app，
// 靠 idle 阈值兜住，照常通知。
func shouldSuppressNotify(ctx context.Context) bool {
	bid := frontmostBundleID(ctx)
	if bid == "" || !isLarkBundleID(bid) {
		return false
	}
	idle := hidIdleSecs(ctx)
	return idle >= 0 && idle < suppressIdleMaxSecs
}

func isLarkBundleID(bid string) bool {
	for _, m := range larkBundleMarkers {
		if strings.Contains(bid, m) {
			return true
		}
	}
	return false
}

// lsappinfoFrontBundleID 取 frontmost 应用的 bundle id
// （lsappinfo 无需 Automation 授权，daemon 场景可用）；探测失败返回 ""。
func lsappinfoFrontBundleID(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "sh", "-c",
		`lsappinfo info -only bundleid "$(lsappinfo front)"`).Output()
	if err != nil {
		return ""
	}
	return parseBundleID(string(out))
}

// parseBundleID 从 `"CFBundleIdentifier"="com.foo.bar"` 中取值；解析不到返回 ""。
func parseBundleID(s string) string {
	_, rest, ok := strings.Cut(s, `"CFBundleIdentifier"="`)
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, `"`)
	if !ok {
		return ""
	}
	return id
}

// ioregHIDIdleSecs 取用户输入空闲秒数（HIDIdleTime，纳秒）；探测失败返回 -1。
func ioregHIDIdleSecs(ctx context.Context) float64 {
	out, err := exec.CommandContext(ctx, "ioreg", "-c", "IOHIDSystem").Output()
	if err != nil {
		return -1
	}
	return parseHIDIdleSecs(string(out))
}

// parseHIDIdleSecs 从 ioreg 输出扫首个 `"HIDIdleTime" = N`（纳秒转秒）；
// 解析不到返回 -1。
func parseHIDIdleSecs(s string) float64 {
	_, rest, ok := strings.Cut(s, `"HIDIdleTime" = `)
	if !ok {
		return -1
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return -1
	}
	ns, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return -1
	}
	return ns / 1e9
}

// RunNotify 展示 P0 批次通知，聚合整个批次为一次调用；执行前先响铃。
// script 为空走内置「忽略/复制/跳转」弹窗；非空经 sh -c 执行，环境变量：
// LW_TITLE 标题（多条带条数）、LW_MESSAGE/LW_SUMMARY 每条一行的聚合摘要、
// LW_LINK 首条 applink（点击跳转）、LW_COUNT 条数、LW_FROM/LW_CHAT/
// LW_CTYPE/LW_TYPE/LW_TEXT 取首条。飞书前台且用户活跃时整体跳过
// （见 shouldSuppressNotify）。同步阻塞至命令退出——弹窗会等用户点击，
// 调用方需自行 go；ctx 取消时子进程被终止。
func RunNotify(ctx context.Context, script string, batch []Message) {
	if shouldSuppressNotify(ctx) {
		logf("notify suppressed: Lark frontmost and user active")
		return
	}
	bellFn(ctx)
	var err error
	if script == "" {
		err = builtinNotify(ctx, batchTitle(p0NotifyTitle, len(batch)), batchSummary(batch), batch[0].Link, "")
	} else {
		err = runNotifyScript(ctx, script, batchNotifyEnv(p0NotifyTitle, batch)...)
	}
	if err != nil && ctx.Err() == nil {
		logf("notify command failed: %v", err)
	}
}

// RunNotifyVC 是音视频会议批次的专用通知：会议邀请实时性最强、「加入」是唯一
// 有意义的动作，不走通用 notify 脚本。优先执行 notify-vc 配置脚本（每次现读，
// 改完即生效；LW_* 环境与通用批次一致，仅标题换 vcNotifyTitle），缺失时回退
// 内置「忽略/加入」弹窗。抑制与响铃语义同 RunNotify；同步阻塞至弹窗关闭，
// 调用方需自行 go。
func RunNotifyVC(ctx context.Context, paths Paths, batch []Message) {
	if shouldSuppressNotify(ctx) {
		logf("notify suppressed: Lark frontmost and user active")
		return
	}
	bellFn(ctx)
	var err error
	if script := ReadNotifyScript(filepath.Join(paths.ConfigDir, "notify-vc")); script != "" {
		err = runNotifyScript(ctx, script, batchNotifyEnv(vcNotifyTitle, batch)...)
	} else {
		err = vcDialogFn(ctx, batchTitle(vcNotifyTitle, len(batch)), batchSummary(batch), batch[0].Link)
	}
	if err != nil && ctx.Err() == nil {
		logf("vc notify failed: %v", err)
	}
}

// StartNotify 同 RunNotify，但 Start 后不等待退出：send-card 短命进程释放
// 延迟通知用——弹窗会阻塞到用户点击，不能拖住 send-card 返回；进程退出后
// 已 fork 的弹窗/脚本继续存活。draft 是候选话术①、mid 是 pending 键：
// 内置弹窗正文展示候选①，按钮「忽略/复制并跳转/发送」（回车发送、Esc 忽略）——
// 「复制并跳转」复制待发的回复并进飞书，「发送」调回本二进制 send-draft 直接
// 以候选①回复对方（不必切回飞书）；自定义脚本经 LW_DRAFT/LW_MID 拿到同一信息。
func StartNotify(ctx context.Context, script string, batch []Message, draft, mid string) {
	if shouldSuppressNotify(ctx) {
		logf("notify suppressed: Lark frontmost and user active")
		return
	}
	bellFn(ctx)
	var err error
	if script == "" {
		title, summary := batchTitle(p0NotifyTitle, len(batch)), batchSummary(batch)
		if s, args, ok := alerterDraftArgs(title, summary, batch[0].Link, draft, mid); ok {
			err = startShellCmd(s, args)
		} else if lines, argv, ok := builtinDraftNotifyArgs(title, summary, batch[0].Link, draft, mid); ok {
			err = startOsascript(lines, argv)
		} else {
			lines, argv := builtinNotifyArgs(title, summary, batch[0].Link, draft)
			err = startOsascript(lines, argv)
		}
	} else {
		cmd := exec.Command("sh", "-c", script)
		env := batchNotifyEnv(p0NotifyTitle, batch)
		if draft != "" {
			env = append(env, "LW_DRAFT="+draft, "LW_MID="+mid)
		}
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		err = cmd.Start()
	}
	if err != nil {
		logf("notify command failed: %v", err)
	}
}

// batchNotifyEnv 是批次通知的完整 LW_* 环境：标题（多条带条数）、每条一行的
// 聚合摘要、首条的链接与扩展字段。
func batchNotifyEnv(titleBase string, batch []Message) []string {
	first := batch[0]
	return append(notifyEnv(batchTitle(titleBase, len(batch)), batchSummary(batch), first.Link),
		"LW_COUNT="+strconv.Itoa(len(batch)),
		"LW_FROM="+deref(first.From),
		"LW_CHAT="+deref(first.Chat),
		"LW_CTYPE="+first.Ctype,
		"LW_TYPE="+first.Type,
		"LW_TEXT="+first.Text,
	)
}

// batchTitle 是批次通知标题：多条时带条数。
func batchTitle(base string, n int) string {
	if n > 1 {
		return fmt.Sprintf("%s（%d 条）", base, n)
	}
	return base
}

// batchSummary 是每条一行的批次摘要（发送者（群名|私聊）: 正文）。
func batchSummary(batch []Message) string {
	lines := make([]string, 0, len(batch))
	for _, m := range batch {
		scene := deref(m.Chat)
		if scene == "" {
			scene = "私聊"
		}
		lines = append(lines, deref(m.From)+"（"+scene+"）: "+notifyText(m))
	}
	return strings.Join(lines, "\n")
}

// notifyEnv 是 LW_* 基础环境变量（标题/内容/摘要/链接）；批次调用再追加扩展字段。
func notifyEnv(title, message, link string) []string {
	return []string{
		"LW_TITLE=" + title,
		"LW_MESSAGE=" + message,
		"LW_SUMMARY=" + message,
		"LW_LINK=" + link,
	}
}

// RunNotifyCommand 是 notify 子命令入口：响铃后发送一条系统通知。
// 优先执行用户 notify 自定义脚本（LW_SUMMARY 与批次模板兼容，取 message）；
// 无脚本时走内置弹窗——子命令是显式触发，off 总开关只管自动通知，不拦这里。
func RunNotifyCommand(ctx context.Context, paths Paths, title, message, link string) error {
	bellFn(ctx)
	if script, _ := LoadNotifyScript(paths.ConfigDir); script != "" {
		return runNotifyScript(ctx, script, notifyEnv(title, message, link)...)
	}
	return builtinNotify(ctx, title, message, link, "")
}

// runNotifyScript 经 sh -c 执行脚本，消息字段由 LW_* 环境变量注入
// （不拼进命令行，正文里的引号/元字符不会破坏脚本）。
func runNotifyScript(ctx context.Context, script string, env ...string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr // 通知命令输出走 stderr，不污染事件流
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// alerter 版内置通知（PATH 有 alerter 时优先）：通知中心横幅，不抢焦点、
// 点横幅正文即主动作。alerter 阻塞至用户交互并把结果打到 stdout（动作按钮 =
// 按钮文案、点正文 = @CONTENTCLICKED、关闭 = @CLOSED、超时 = @TIMEOUT），
// 由 sh 片段消费分发；值经位置参数传入（不拼进脚本，防注入）。
// 常驻按钮需在系统设置 → 通知里把 alerter 样式设为「提醒」。
const (
	// 草稿弹窗：正文展示摘要＋候选①，「发送」回调 send-draft，点正文 =
	// 复制并跳转（候选①进剪贴板 + open applink），关闭/超时 = 忽略。
	// $1 alerter $2 标题 $3 正文 $4 本二进制 $5 mid $6 候选① $7 link
	alerterDraftScript = `out=$("$1" -title "$2" -message "$3" -actions "发送" -closeLabel "忽略" -timeout 60)
case "$out" in
"发送") exec "$4" send-draft --mid "$5" ;;
"@CONTENTCLICKED") printf '%s' "$6" | pbcopy; if [ -n "$7" ]; then open "$7"; fi ;;
esac`
	// 通用弹窗：「复制」进剪贴板，点正文 = 跳转（无 link 不动作）。
	// $1 alerter $2 标题 $3 正文 $4 复制内容 $5 link
	alerterGenericScript = `out=$("$1" -title "$2" -message "$3" -actions "复制" -closeLabel "忽略" -timeout 60)
case "$out" in
"复制") printf '%s' "$4" | pbcopy ;;
"@CONTENTCLICKED") if [ -n "$5" ]; then open "$5"; fi ;;
esac`
	// VC 弹窗：「加入」或点正文 = open 首条 applink 入会。
	// $1 alerter $2 标题 $3 正文 $4 link
	alerterVCScript = `out=$("$1" -title "$2" -message "$3" -actions "加入" -closeLabel "忽略" -timeout 60)
case "$out" in
"加入"|"@CONTENTCLICKED") open "$4" ;;
esac`
)

// alerterDraftArgs 组装草稿弹窗的 alerter 调用；alerter 未安装或取不到自身
// 可执行路径时 ok=false（回退 osascript 弹窗）。
func alerterDraftArgs(title, message, link, draft, mid string) (script string, args []string, ok bool) {
	ap := lookAlerter()
	exe, err := os.Executable()
	if ap == "" || err != nil || draft == "" || mid == "" {
		return "", nil, false
	}
	text := message + "\n\n—— 回复草稿 ——\n" + draft
	return alerterDraftScript, []string{ap, title, text, exe, mid, draft, link}, true
}

// alerterGenericArgs 组装通用弹窗的 alerter 调用；复制内容优先候选话术，
// 无草稿回退正文。alerter 未安装时 ok=false。
func alerterGenericArgs(title, message, link, draft string) (script string, args []string, ok bool) {
	ap := lookAlerter()
	if ap == "" {
		return "", nil, false
	}
	copyText := draft
	if copyText == "" {
		copyText = message
	}
	return alerterGenericScript, []string{ap, title, message, copyText, link}, true
}

// alerterVCArgs 组装音视频会议弹窗的 alerter 调用；未安装时 ok=false。
func alerterVCArgs(title, message, link string) (script string, args []string, ok bool) {
	ap := lookAlerter()
	if ap == "" {
		return "", nil, false
	}
	return alerterVCScript, []string{ap, title, message, link}, true
}

// runShellCmd 以 sh -c 执行内置 shell 片段并阻塞至退出；值经位置参数传入
// （$0 占位 "sh"），输出走 stderr，不污染事件流。
func runShellCmd(ctx context.Context, script string, args []string) error {
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// startShellCmd 同 runShellCmd 但 Start 后不等待（send-card 短命进程场景），
// 父进程退出后横幅与动作分发继续存活。
func startShellCmd(script string, args []string) error {
	cmd := exec.Command("sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// builtinNotify 内置通知（notify 默认路径，响铃在调用方）：PATH 有 alerter 时
// 走通知中心横幅（见 alerterGenericScript），否则 osascript 弹窗——有 link 时给
// 「忽略/复制/跳转」按钮，点「跳转」open applink 唤起飞书；无 link 给「复制/OK」。
// 「忽略」/「OK」兼任 cancel button——Esc 即忽略关闭（-128 由 try 吞掉，不算
// 脚本失败）。「复制」把候选话术（draft）置入剪贴板——复制到手的是自己要发的
// 回复；无草稿（即时/兜底通知、notify 子命令）回退弹窗消息。argv 传参防
// AppleScript 注入；60 秒无操作自动关闭，防弹窗进程堆积。
func builtinNotify(ctx context.Context, title, message, link, draft string) error {
	if script, args, ok := alerterGenericArgs(title, message, link, draft); ok {
		return runShellCmd(ctx, script, args)
	}
	lines, argv := builtinNotifyArgs(title, message, link, draft)
	return runOsascript(ctx, lines, argv)
}

// builtinDraftNotifyArgs 组装草稿就绪弹窗（display dialog 三键）：正文展示
// 对方消息摘要＋候选①全文（看清要发什么再按），按钮「忽略/复制并跳转/发送」、
// 默认「发送」（回车即发）、「忽略」兼任 cancel button（Esc 即忽略，-128 由
// try 吞掉）。「发送」经 do shell script 调回本二进制 `send-draft --mid <mid>`
// 直接以候选①回复对方——无需切回飞书；「复制并跳转」把候选①置入剪贴板并
// open applink 进飞书（想手改后发走这条）。60 秒无操作自动关闭即忽略。
// 取不到自身可执行路径（ok=false）时回退通用弹窗。
func builtinDraftNotifyArgs(title, message, link, draft, mid string) (lines, argv []string, ok bool) {
	exe, err := os.Executable()
	if err != nil || draft == "" || mid == "" {
		return nil, nil, false
	}
	lines = []string{
		"on run argv",
		"try",
		`set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons {"忽略", "复制并跳转", "发送"} default button "发送" cancel button "忽略" giving up after 60`,
		"set b to button returned of r",
		`if b is "复制并跳转" then`,
		"set the clipboard to (item 3 of argv)",
		`if (item 6 of argv) is not "" then do shell script "open " & quoted form of (item 6 of argv)`,
		"end if",
		`if b is "发送" then do shell script quoted form of (item 4 of argv) & " send-draft --mid " & quoted form of (item 5 of argv)`,
		"end try",
		"end run",
	}
	dialogText := message + "\n\n—— 回复草稿 ——\n" + draft
	return lines, []string{dialogText, title, draft, exe, mid, link}, true
}

// builtinNotifyArgs 组装内置弹窗的 AppleScript 与 argv（见 builtinNotify）。
func builtinNotifyArgs(title, message, link, draft string) (lines, argv []string) {
	copyText := draft
	if copyText == "" {
		copyText = message
	}
	lines = []string{
		"on run argv",
		"try",
		`set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons {"复制", "OK"} default button "OK" cancel button "OK" giving up after 60`,
		`if button returned of r is "复制" then set the clipboard to (item 3 of argv)`,
		"end try",
		"end run",
	}
	argv = []string{message, title, copyText}
	if link != "" {
		lines = []string{
			"on run argv",
			"try",
			`set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons {"忽略", "复制", "跳转"} default button "跳转" cancel button "忽略" giving up after 60`,
			"set b to button returned of r",
			`if b is "复制" then set the clipboard to (item 3 of argv)`,
			`if b is "跳转" then do shell script "open " & quoted form of (item 4 of argv)`,
			"end try",
			"end run",
		}
		argv = append(argv, link)
	}
	return lines, argv
}

// builtinNotifyVC 内置音视频会议弹窗（未配置 notify-vc 时的回退，响铃在调用方）：
// PATH 有 alerter 时走横幅（「加入」或点正文即入会），否则 osascript
// 「忽略/加入」按钮、默认「加入」，点「加入」open 首条 applink 直达会话中的
// 会议消息；「忽略」兼任 cancel button（Esc 即忽略）；60 秒无操作自动关闭，
// 防弹窗进程堆积。VC 消息的 link 来自 message_app_link 恒有值，不设无 link 分支。
func builtinNotifyVC(ctx context.Context, title, message, link string) error {
	if script, args, ok := alerterVCArgs(title, message, link); ok {
		return runShellCmd(ctx, script, args)
	}
	return runOsascript(ctx, []string{
		"on run argv",
		"try",
		`set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons {"忽略", "加入"} default button "加入" cancel button "忽略" giving up after 60`,
		`if button returned of r is "加入" then do shell script "open " & quoted form of (item 3 of argv)`,
		"end try",
		"end run",
	}, []string{message, title, link})
}

// osascriptArgs 逐行 -e 组装 AppleScript 命令行并以 argv 传参（消息不拼进源码，
// 正文里的引号不会破坏脚本或被注入）。
func osascriptArgs(lines, argv []string) []string {
	args := make([]string, 0, 2*len(lines)+len(argv))
	for _, l := range lines {
		args = append(args, "-e", l)
	}
	return append(args, argv...)
}

// runOsascript 执行 AppleScript 并阻塞至退出；输出走 stderr，不污染事件流。
func runOsascript(ctx context.Context, lines, argv []string) error {
	cmd := exec.CommandContext(ctx, "osascript", osascriptArgs(lines, argv)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// startOsascript 同 runOsascript 但 Start 后不等待（send-card 短命进程场景），
// 父进程退出后弹窗继续存活。
func startOsascript(lines, argv []string) error {
	cmd := exec.Command("osascript", osascriptArgs(lines, argv)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// notifyText 是通知摘要里的单条正文：截 80 字；音视频会议正文常为空，给可读占位。
func notifyText(m Message) string {
	if m.Text != "" {
		return truncateRunes(m.Text, 80)
	}
	if vcTypes[m.Type] {
		return "发起了音视频会议"
	}
	return "[" + m.Type + "]"
}
