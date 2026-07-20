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

// ReadNotifyScript 读取通知命令脚本；文件缺失或全空白视为未配置。
func ReadNotifyScript(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// 批次通知的标题基串：通用 P0 与音视频会议专用弹窗各一。
const (
	p0NotifyTitle = "飞书 P0"
	vcNotifyTitle = "📞 音视频会议"
)

// bellFn 是响铃入口，可注入测试替身（响铃是 IO 边缘）。
var bellFn = ringBell

// vcDialogFn 是内置音视频会议弹窗入口，可注入测试替身（osascript 是 IO 边缘）。
var vcDialogFn = builtinNotifyVC

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

// RunNotify 经 sh -c 执行通知脚本，聚合整个 P0 批次为一次调用；执行前先响铃。
// 飞书前台且用户活跃时整体跳过（见 shouldSuppressNotify）。
// 环境变量：LW_TITLE 标题（多条带条数）、LW_MESSAGE/LW_SUMMARY 每条一行的聚合
// 摘要、LW_LINK 首条 applink（点击跳转）、LW_COUNT 条数、LW_FROM/LW_CHAT/
// LW_CTYPE/LW_TYPE/LW_TEXT 取首条。
// 同步阻塞至命令退出——弹窗类命令会等用户点击，调用方需自行 go；
// ctx 取消时子进程被终止。
func RunNotify(ctx context.Context, script string, batch []Message) {
	if shouldSuppressNotify(ctx) {
		logf("notify suppressed: Lark frontmost and user active")
		return
	}
	bellFn(ctx)
	err := runNotifyScript(ctx, script, batchNotifyEnv(p0NotifyTitle, batch)...)
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

// StartNotify 同 RunNotify，但脚本 Start 后不等待退出：send-card 短命进程释放
// 延迟通知用——弹窗类脚本会阻塞到用户点击，不能拖住 send-card 返回；
// 进程退出后已 fork 的脚本继续存活。
func StartNotify(ctx context.Context, script string, batch []Message) {
	if shouldSuppressNotify(ctx) {
		logf("notify suppressed: Lark frontmost and user active")
		return
	}
	bellFn(ctx)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), batchNotifyEnv(p0NotifyTitle, batch)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
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
// 优先执行用户 notify 配置脚本（LW_SUMMARY 与批次模板兼容，取 message）；
// 未配置时回退内置 osascript 弹窗。
func RunNotifyCommand(ctx context.Context, paths Paths, title, message, link string) error {
	bellFn(ctx)
	if script := ReadNotifyScript(filepath.Join(paths.ConfigDir, "notify")); script != "" {
		return runNotifyScript(ctx, script, notifyEnv(title, message, link)...)
	}
	return builtinNotify(ctx, title, message, link)
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

// builtinNotify 内置 macOS 弹窗（未配置 notify 脚本时的回退，响铃在调用方）：
// 有 link 时给「忽略/复制/跳转」按钮，点「跳转」open applink 唤起飞书；无 link 给
// 「复制/OK」。「复制」把弹窗消息置入剪贴板。argv 传参防 AppleScript 注入；
// 60 秒无操作自动关闭，防弹窗进程堆积。
func builtinNotify(ctx context.Context, title, message, link string) error {
	lines := []string{
		"on run argv",
		`set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons {"复制", "OK"} default button "OK" giving up after 60`,
		`if button returned of r is "复制" then set the clipboard to (item 1 of argv)`,
		"end run",
	}
	argv := []string{message, title}
	if link != "" {
		lines = []string{
			"on run argv",
			`set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons {"忽略", "复制", "跳转"} default button "跳转" giving up after 60`,
			"set b to button returned of r",
			`if b is "复制" then set the clipboard to (item 1 of argv)`,
			`if b is "跳转" then do shell script "open " & quoted form of (item 3 of argv)`,
			"end run",
		}
		argv = append(argv, link)
	}
	return runOsascript(ctx, lines, argv)
}

// builtinNotifyVC 内置音视频会议弹窗（未配置 notify-vc 时的回退，响铃在调用方）：
// 「忽略/加入」按钮、默认「加入」，点「加入」open 首条 applink 直达会话中的
// 会议消息；60 秒无操作自动关闭，防弹窗进程堆积。VC 消息的 link 来自
// message_app_link 恒有值，不设无 link 分支。
func builtinNotifyVC(ctx context.Context, title, message, link string) error {
	return runOsascript(ctx, []string{
		"on run argv",
		`set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons {"忽略", "加入"} default button "加入" giving up after 60`,
		`if button returned of r is "加入" then do shell script "open " & quoted form of (item 3 of argv)`,
		"end run",
	}, []string{message, title, link})
}

// runOsascript 逐行 -e 组装 AppleScript 并以 argv 传参（消息不拼进源码，
// 正文里的引号不会破坏脚本或被注入）；输出走 stderr，不污染事件流。
func runOsascript(ctx context.Context, lines, argv []string) error {
	args := make([]string, 0, 2*len(lines)+len(argv))
	for _, l := range lines {
		args = append(args, "-e", l)
	}
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "osascript", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
