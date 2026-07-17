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

// bellFn 是响铃入口，可注入测试替身（响铃是 IO 边缘）。
var bellFn = ringBell

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

// RunNotify 经 sh -c 执行通知脚本，聚合整个 P0 批次为一次调用；执行前先响铃。
// 环境变量：LW_TITLE 标题（多条带条数）、LW_MESSAGE/LW_SUMMARY 每条一行的聚合
// 摘要、LW_LINK 首条 applink（点击跳转）、LW_COUNT 条数、LW_FROM/LW_CHAT/
// LW_CTYPE/LW_TYPE/LW_TEXT 取首条。
// 同步阻塞至命令退出——弹窗类命令会等用户点击，调用方需自行 go；
// ctx 取消时子进程被终止。
func RunNotify(ctx context.Context, script string, batch []Message) {
	first := batch[0]
	lines := make([]string, 0, len(batch))
	for _, m := range batch {
		scene := deref(m.Chat)
		if scene == "" {
			scene = "私聊"
		}
		lines = append(lines, deref(m.From)+"（"+scene+"）: "+notifyText(m))
	}
	title := "飞书 P0"
	if len(batch) > 1 {
		title = fmt.Sprintf("飞书 P0（%d 条）", len(batch))
	}
	summary := strings.Join(lines, "\n")
	bellFn(ctx)
	err := runNotifyScript(ctx, script, append(notifyEnv(title, summary, first.Link),
		"LW_COUNT="+strconv.Itoa(len(batch)),
		"LW_FROM="+deref(first.From),
		"LW_CHAT="+deref(first.Chat),
		"LW_CTYPE="+first.Ctype,
		"LW_TYPE="+first.Type,
		"LW_TEXT="+first.Text,
	)...)
	if err != nil && ctx.Err() == nil {
		logf("notify command failed: %v", err)
	}
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
