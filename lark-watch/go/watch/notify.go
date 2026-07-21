package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
// 文件缺失 = 零配置默认：内置横幅（模板内嵌于二进制，硬依赖 PATH 里的 alerter）；
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
// brew install alerter）：内置通知硬依赖它（通知中心横幅，点横幅正文即动作、
// 不抢焦点），未装返回 ""——内置通知跳过并报 errAlerterMissing。
// 每次弹横幅现探测，装完即生效。
// var 便于测试注入（macOS 开发机装有 alerter 时测试不能真弹横幅）。
var lookAlerter = func() string {
	p, err := exec.LookPath("alerter")
	if err != nil {
		return ""
	}
	return p
}

// errAlerterMissing：内置通知硬依赖 alerter，未装时通知跳过（响铃已在
// notifyGate 响过），错误经现有日志/返回管道透出安装指引。
var errAlerterMissing = errors.New("alerter not found; brew install vjeantet/tap/alerter")

// vcDialogFn 是内置音视频会议横幅入口，可注入测试替身（弹横幅是 IO 边缘）。
var vcDialogFn = builtinNotifyVC

// sendDraftAlertFn 是 send-draft/send-text/react 结果提示横幅入口，可注入
// 测试替身（IO 边缘：macOS 开发机跑测试不能真弹横幅阻塞）。提示无消息上下文，
// link/draft/mid 恒空——走 plain 横幅。
var sendDraftAlertFn = func(ctx context.Context, configDir, title, message string) error {
	return builtinNotify(ctx, configDir, title, message, "", "", "")
}

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

// notifyGate 是通知统一前置：飞书前台且用户活跃时抑制（返回 false），否则
// 响铃放行。RunNotify/RunNotifyVC/StartNotify 共用，抑制与响铃语义不分叉；
// 抑制留痕也在此单点入档（带 mids，审计可按 mid 反查被抑制的消息）。
func notifyGate(ctx context.Context, mids []string) bool {
	if shouldSuppressNotify(ctx) {
		logf("notify suppressed: Lark frontmost and user active")
		evlog.Info("notify.suppress", "mids", mids)
		return false
	}
	bellFn(ctx)
	return true
}

// logNotifyFail 把通知发送失败入档 Error 级（kind 区分 p0/vc/draft 链路，
// mids 关联消息）；stderr 文本行由调用方既有 logf 继续负责。
func logNotifyFail(kind string, batch []Message, err error) {
	evlog.Error("notify.fail", "kind", kind, "mids", mids(batch), "err", err.Error())
}

// RunNotify 展示 P0 批次通知，聚合整个批次为一次调用；执行前先响铃。
// script 为空走内置横幅（见 builtinNotify）；非空经 sh -c 执行，环境变量：
// LW_TITLE 标题（多条带条数）、LW_MESSAGE/LW_SUMMARY 每条一行的聚合摘要、
// LW_LINK 首条 applink（点击跳转）、LW_COUNT 条数、LW_FROM/LW_CHAT/
// LW_CTYPE/LW_TYPE/LW_TEXT 取首条、LW_ICON 头像 URL（可为空）。
// icon 是横幅图标（私聊对方/群头像 URL，空 = 默认图标）。飞书前台且用户
// 活跃时整体跳过（见 shouldSuppressNotify）。同步阻塞至命令退出——弹窗会等
// 用户点击，调用方需自行 go；ctx 取消时子进程被终止。
func RunNotify(ctx context.Context, configDir, script string, batch []Message, icon string) {
	if !notifyGate(ctx, mids(batch)) {
		return
	}
	var err error
	if script == "" {
		// 快捷动作落在批次代表消息上（最后一条，与 send-card --mid 惯例一致）
		err = builtinNotify(ctx, configDir, batchTitle(p0NotifyTitle, len(batch)), batchSummary(batch),
			batch[0].Link, batch[len(batch)-1].Mid, icon)
	} else {
		err = runNotifyScript(ctx, script, batchNotifyEnv(p0NotifyTitle, batch, icon)...)
	}
	if err != nil && ctx.Err() == nil {
		logf("notify command failed: %v", err)
		logNotifyFail("p0", batch, err)
	}
}

// RunNotifyVC 是音视频会议批次的专用通知：会议邀请实时性最强、「加入」是唯一
// 有意义的动作，不走通用 notify 脚本。优先执行 notify-vc 配置脚本（每次现读，
// 改完即生效；LW_* 环境与通用批次一致，仅标题换 vcNotifyTitle），缺失时回退
// 内置「忽略/加入」横幅。抑制与响铃语义同 RunNotify；同步阻塞至横幅关闭，
// 调用方需自行 go。
func RunNotifyVC(ctx context.Context, paths Paths, batch []Message, icon string) {
	if !notifyGate(ctx, mids(batch)) {
		return
	}
	var err error
	if script := ReadNotifyScript(filepath.Join(paths.ConfigDir, "notify-vc")); script != "" {
		err = runNotifyScript(ctx, script, batchNotifyEnv(vcNotifyTitle, batch, icon)...)
	} else {
		err = vcDialogFn(ctx, batchTitle(vcNotifyTitle, len(batch)), batchSummary(batch), batch[0].Link, icon)
	}
	if err != nil && ctx.Err() == nil {
		logf("vc notify failed: %v", err)
		logNotifyFail("vc", batch, err)
	}
}

// StartNotify 同 RunNotify，但 Start 后不等待退出：send-card 短命进程释放
// 延迟通知用——横幅会阻塞到用户交互，不能拖住 send-card 返回；进程退出后
// 已 fork 的横幅/脚本继续存活。draft 是候选话术①、mid 是 pending 键：
// 内置横幅正文展示候选①，动作「发送」调回本二进制 send-draft 直接以候选①
// 回复对方（不必切回飞书），点正文 = 复制待发的回复并进飞书；自定义脚本经
// LW_DRAFT/LW_MID 拿到同一信息。
func StartNotify(ctx context.Context, configDir, script string, batch []Message, draft, mid, icon string) {
	if !notifyGate(ctx, mids(batch)) {
		return
	}
	var err error
	if script == "" {
		title, summary := batchTitle(p0NotifyTitle, len(batch)), batchSummary(batch)
		if s, args, ok := alerterDraftArgs(configDir, title, summary, batch[0].Link, draft, mid, icon); ok {
			err = startShellCmd(s, args)
		} else {
			err = errAlerterMissing
		}
	} else {
		cmd := exec.Command("sh", "-c", script)
		env := batchNotifyEnv(p0NotifyTitle, batch, icon)
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
		logNotifyFail("draft", batch, err)
	}
}

// batchNotifyEnv 是批次通知的完整 LW_* 环境：标题（多条带条数）、每条一行的
// 聚合摘要、首条的链接与扩展字段、头像 URL（LW_ICON，可为空）。
func batchNotifyEnv(titleBase string, batch []Message, icon string) []string {
	first := batch[0]
	return append(notifyEnv(batchTitle(titleBase, len(batch)), batchSummary(batch), first.Link),
		"LW_COUNT="+strconv.Itoa(len(batch)),
		"LW_FROM="+deref(first.From),
		"LW_CHAT="+deref(first.Chat),
		"LW_CTYPE="+first.Ctype,
		"LW_TYPE="+first.Type,
		"LW_TEXT="+first.Text,
		"LW_ICON="+icon,
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
// 无脚本时走内置横幅——子命令是显式触发，off 总开关只管自动通知，不拦这里。
func RunNotifyCommand(ctx context.Context, paths Paths, title, message, link string) error {
	bellFn(ctx)
	if script, _ := LoadNotifyScript(paths.ConfigDir); script != "" {
		return runNotifyScript(ctx, script, notifyEnv(title, message, link)...)
	}
	return builtinNotify(ctx, paths.ConfigDir, title, message, link, "", "")
}

// runNotifyScript 经 sh -c 执行脚本，消息字段由 LW_* 环境变量注入
// （不拼进命令行，正文里的引号/元字符不会破坏脚本）。调用与结果经 logCmd
// 留痕（env 不入档——正文已在 msg.keep 记过）。
func runNotifyScript(ctx context.Context, script string, env ...string) error {
	argv := []string{"-c", script}
	cmd := exec.CommandContext(ctx, "sh", argv...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr // 通知命令输出走 stderr，不污染事件流
	cmd.Stderr = os.Stderr
	start := time.Now()
	err := cmd.Run()
	logCmd("sh", argv, time.Since(start), 0, err)
	return err
}

// alerter 版内置通知（内置路径唯一实现）：通知中心横幅，不抢焦点、
// 点横幅正文即主动作。alerter 阻塞至用户交互并把结果打到 stdout（动作按钮 =
// 按钮文案、点正文 = @CONTENTCLICKED、关闭 = @CLOSED、超时 = @TIMEOUT），
// 由 sh 片段消费分发；值经位置参数传入（不拼进脚本，防注入）。
// 旗标为 alerter ≥26 的双横线语法（旧单横线写法会 exit 64）；调用失败经
// `|| exit $?` 透传，不被 case 空匹配吞掉。通知以 alerter 默认
// --sender com.apple.Terminal 名义投递，常驻按钮需在系统设置 → 通知里
// 把「终端」样式设为「提醒」。
// alerterIconFlag 条件性生成 --app-icon 旗标引用（横幅左侧图标 = 飞书头像，
// alerter 自行下载 URL，坏 URL 不影响横幅投递）：icon 为空返回空串——
// --app-icon "" 行为未定义，不得带空旗标。n 是 icon 的位置参数索引；icon 值
// 恒占 args 末位，布局不随空非空变化，仅旗标条件生成。
func alerterIconFlag(icon string, n int) string {
	if icon == "" {
		return ""
	}
	return " --app-icon " + posParam(n)
}

// alerterPlainScript 无快捷动作的通用横幅（无 mid 上下文：notify 子命令、
// 失败提示）。$1 alerter $2 标题 $3 正文 $4 复制内容 $5 link $6 icon
func alerterPlainScript(icon string) string {
	return `out=$("$1" --title "$2" --message "$3" --actions "复制" --close-label "忽略" --timeout 60 --ignore-dnd` + alerterIconFlag(icon, 6) + `) || exit $?
case "$out" in
"复制") printf '%s' "$4" | pbcopy ;;
"@CONTENTCLICKED") if [ -n "$5" ]; then open "$5"; fi ;;
esac`
}

// alerterVCScript VC 横幅：「加入」或点正文 = open 首条 applink 入会。
// $1 alerter $2 标题 $3 正文 $4 link $5 icon
func alerterVCScript(icon string) string {
	return `out=$("$1" --title "$2" --message "$3" --actions "加入" --close-label "忽略" --timeout 60 --ignore-dnd` + alerterIconFlag(icon, 5) + `) || exit $?
case "$out" in
"加入"|"@CONTENTCLICKED") open "$4" ;;
esac`
}

// alerterActionScript 生成带快捷动作（常用语/表情）的横幅 sh 片段。
// 位置参数布局（见 alerterDraftArgs/alerterGenericArgs）：$8 是动作 CSV，
// $9 起每个快捷动作占两位（标签、值），icon 恒居末位 $(9+2k)。动作标签与值
// 全部经位置参数按字面匹配/传参，脚本文本里只有 $n 索引、零用户内容——
// 注入面与静态脚本一致。草稿横幅：首键「发送」回调 send-draft，点正文 =
// 复制并跳转；通用横幅：首键「复制」，点正文 = 跳转。关闭/超时 = 忽略。
func alerterActionScript(draft bool, actions []quickAction, icon string) string {
	exeRef, midRef := `"$4"`, `"$5"`
	first := fmt.Sprintf(`"发送") exec "$4" %s --%s "$5" ;;`, CmdSendDraft, FlagMid)
	content := `"@CONTENTCLICKED") printf '%s' "$6" | pbcopy; if [ -n "$7" ]; then open "$7"; fi ;;`
	if !draft {
		exeRef, midRef = `"$6"`, `"$7"`
		first = `"复制") printf '%s' "$4" | pbcopy ;;`
		content = `"@CONTENTCLICKED") if [ -n "$5" ]; then open "$5"; fi ;;`
	}
	var b strings.Builder
	b.WriteString(`out=$("$1" --title "$2" --message "$3" --actions "$8" --close-label "忽略" --timeout 60 --ignore-dnd` +
		alerterIconFlag(icon, 9+2*len(actions)) + `) || exit $?` + "\n")
	b.WriteString(`case "$out" in` + "\n")
	b.WriteString(first + "\n")
	for i, a := range actions {
		verb := fmt.Sprintf("%s --%s %s --%s", a.Cmd, FlagMid, midRef, a.Flag)
		fmt.Fprintf(&b, "%s) exec %s %s %s ;;\n",
			posParam(9+2*i), exeRef, verb, posParam(10+2*i))
	}
	b.WriteString(content + "\nesac")
	return b.String()
}

// posParam 是带引号的 sh 位置参数引用；≥10 需花括号（$10 会被解释成 $1 后跟 0）。
func posParam(n int) string {
	if n < 10 {
		return fmt.Sprintf(`"$%d"`, n)
	}
	return fmt.Sprintf(`"${%d}"`, n)
}

// actionsCSV 是 alerter --actions 的单参 CSV（标签已在加载时清洗掉 ASCII 逗号）。
func actionsCSV(first string, actions []quickAction) string {
	labels := make([]string, 0, len(actions)+1)
	labels = append(labels, first)
	for _, a := range actions {
		labels = append(labels, a.Label)
	}
	return strings.Join(labels, ",")
}

// alerterDraftArgs 组装草稿横幅的 alerter 调用：正文展示摘要＋候选①，动作
// 下拉 = 发送＋常用语＋表情（快捷动作每次现读配置）。位置参数布局：
// $1 alerter $2 标题 $3 正文 $4 本二进制 $5 mid $6 候选① $7 link
// $8 动作 CSV $9.. 每动作（标签, 值）对，icon 恒居末位。
// alerter 未安装或取不到自身可执行路径时 ok=false（调用方报 alerter 缺失）。
func alerterDraftArgs(configDir, title, message, link, draft, mid, icon string) (script string, args []string, ok bool) {
	ap := lookAlerter()
	exe, err := os.Executable()
	if ap == "" || err != nil || draft == "" || mid == "" {
		return "", nil, false
	}
	actions := loadQuickActions(configDir)
	args = []string{ap, title, draftBody(message, draft), exe, mid, draft, link, actionsCSV("发送", actions)}
	for _, a := range actions {
		args = append(args, a.Label, a.Value)
	}
	args = append(args, icon)
	return alerterActionScript(true, actions, icon), args, true
}

// alerterGenericArgs 组装通用横幅的 alerter 调用：复制内容为通知正文。
// 有 mid（P0 批次场景，取批次代表消息）时动作下拉 =
// 复制＋常用语＋表情，位置参数布局：$1 alerter $2 标题 $3 正文 $4 复制内容
// $5 link $6 本二进制 $7 mid $8 动作 CSV $9.. 每动作（标签, 值）对，icon
// 恒居末位；无 mid（notify 子命令、失败提示）退回无快捷动作的 plain 版。
// alerter 未安装时 ok=false。
func alerterGenericArgs(configDir, title, message, link, mid, icon string) (script string, args []string, ok bool) {
	ap := lookAlerter()
	if ap == "" {
		return "", nil, false
	}
	exe, err := os.Executable()
	if mid == "" || err != nil {
		return alerterPlainScript(icon), []string{ap, title, message, message, link, icon}, true
	}
	actions := loadQuickActions(configDir)
	args = []string{ap, title, message, message, link, exe, mid, actionsCSV("复制", actions)}
	for _, a := range actions {
		args = append(args, a.Label, a.Value)
	}
	args = append(args, icon)
	return alerterActionScript(false, actions, icon), args, true
}

// alerterVCArgs 组装音视频会议横幅的 alerter 调用；未安装时 ok=false。
func alerterVCArgs(title, message, link, icon string) (script string, args []string, ok bool) {
	ap := lookAlerter()
	if ap == "" {
		return "", nil, false
	}
	return alerterVCScript(icon), []string{ap, title, message, link, icon}, true
}

// runShellCmd 以 sh -c 执行内置 shell 片段并阻塞至退出；值经位置参数传入
// （$0 占位 "sh"），输出走 stderr，不污染事件流。调用与结果经 logCmd 留痕。
func runShellCmd(ctx context.Context, script string, args []string) error {
	argv := append([]string{"-c", script, "sh"}, args...)
	cmd := exec.CommandContext(ctx, "sh", argv...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	start := time.Now()
	err := cmd.Run()
	logCmd("sh", argv, time.Since(start), 0, err)
	return err
}

// startFailWindow 是 startShellCmd 捕捉秒退失败的观察窗口：启动即败（旗标
// 不兼容、二进制缺失等）在数十毫秒内退出，正常横幅至少存活到用户交互/超时。
const startFailWindow = 500 * time.Millisecond

// startShellCmd 同 runShellCmd 但只在启动窗口内短暂观察（send-card 短命进程
// 场景），窗口内非零秒退返回错误，存活则放手——父进程退出后横幅与动作分发
// 继续存活。调用与观察窗结论经 logCmd 留痕（放行视为成功）。
func startShellCmd(script string, args []string) error {
	argv := append([]string{"-c", script, "sh"}, args...)
	cmd := exec.Command("sh", argv...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	start := time.Now()
	err := startAndWatch(cmd)
	logCmd("sh", argv, time.Since(start), 0, err)
	return err
}

// startAndWatch 启动并只在 startFailWindow 内观察秒退。
func startAndWatch(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(startFailWindow):
		return nil
	}
}

// builtinNotify 内置通知（notify 默认路径，响铃在调用方）：通知中心横幅，
// mid 非空时带常用语/表情快捷动作，「复制」把通知正文置入剪贴板。
// 硬依赖 alerter，未装返回安装指引错误（自动通知走调用方 logf，
// notify 子命令直接报给用户）。
func builtinNotify(ctx context.Context, configDir, title, message, link, mid, icon string) error {
	script, args, ok := alerterGenericArgs(configDir, title, message, link, mid, icon)
	if !ok {
		return errAlerterMissing
	}
	return runShellCmd(ctx, script, args)
}

// draftBody 是草稿通知正文：对方消息摘要＋候选①全文。
func draftBody(message, draft string) string {
	return message + "\n\n—— 回复草稿 ——\n" + draft
}

// builtinNotifyVC 内置音视频会议横幅（未配置 notify-vc 时的回退，响铃在调用方）：
// 「加入」或点正文 open 首条 applink 直达会话中的会议消息；60 秒无操作自动
// 关闭，防横幅进程堆积。VC 消息的 link 来自 message_app_link 恒有值，不设
// 无 link 分支。硬依赖 alerter，未装返回安装指引错误。
func builtinNotifyVC(ctx context.Context, title, message, link, icon string) error {
	script, args, ok := alerterVCArgs(title, message, link, icon)
	if !ok {
		return errAlerterMissing
	}
	return runShellCmd(ctx, script, args)
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
