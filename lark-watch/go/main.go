// lark-watch — 用户视角飞书消息监控与卡片直发（单二进制：poller + 卡片回调）。
// stdout 事件契约：P0/digest/alert/backlog/catchup 单行 JSON；诊断走 stderr。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lark-watch/watch"
)

// multiFlag 收集可重复出现的 flag 值（send-card 的多候选 --draft）。
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := dispatch(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "[lark-watch] %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: lark-watch <command> [flags]
  run         poller + 卡片回调（生产入口，一个 Monitor）
  poll        仅消息轮询
  card-daemon 仅卡片回调
  catchup     补课：拉积压消息按会话分组
  mark        标记会话已处理游标
  ignore-add  追加噪音正则
  send-card   起草确认卡片（pending 入库 + 渲染 + bot 私发）
  send-draft  发送 pending 候选草稿（通知弹窗「发送」按钮的回调）
  send-text   以常用语快捷回复源消息（通知横幅动作的回调）
  react       给源消息加表情回应（通知横幅动作的回调）
  notify      发送系统通知（--title --message --link；优先 notify 配置脚本，缺省内置弹窗）
  status      健康 JSON`)
}

func openStore() (*watch.Store, error) {
	return watch.OpenStore(watch.DefaultPaths().StateDir)
}

func daemonCtx() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	return ctx
}

func dispatch(cmd string, args []string) error {
	// 事件诊断日志对全部子命令统一开启（默认开，LW_EVENT_LOG=0 关闭）：
	// 守护进程与短命令（send-card/status 等）并发追加同一文件，O_APPEND 保序。
	closeEvlog := watch.InitEventLog(watch.DefaultPaths().StateDir)
	defer closeEvlog()
	cli := &watch.ExecLarkCLI{}
	switch cmd {
	case "run", "poll":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		interval := fs.Int("interval", 5, "轮询间隔秒数")
		digestWindow := fs.Int64("digest-window", 600, "摘要时间窗秒数")
		digestMax := fs.Int("digest-max", 20, "摘要条数阈值")
		fs.Parse(args)
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		return watch.RunDaemon(daemonCtx(), s, cli, watch.DefaultPaths(),
			time.Duration(*interval)*time.Second, *digestWindow, *digestMax, cmd == "run")

	case "card-daemon":
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		auth, err := cli.AuthSelf()
		if err != nil {
			return err
		}
		watch.SuperviseCardConsumerStandalone(daemonCtx(), s, cli, auth.OpenID)
		return nil

	case "catchup":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		since := fs.String("since", "24h", "首次回看窗口")
		peek := fs.Int("peek", 5, "每会话预览条数")
		fs.Parse(args)
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		return watch.RunCatchup(s, cli, watch.DefaultPaths(), *since, *peek)

	case "mark":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		all := fs.Bool("all", false, "标记最近一次 catchup 的全部会话")
		at := fs.Int64("at", time.Now().Unix(), "游标时间（epoch 秒）")
		fs.Parse(args)
		if !*all && len(fs.Args()) == 0 {
			return fmt.Errorf("usage: lark-watch mark <cid>... | mark --all [--at <epoch>]")
		}
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		return watch.RunMark(s, fs.Args(), *all, *at)

	case "ignore-add":
		if len(args) != 1 || args[0] == "" {
			return fmt.Errorf("usage: lark-watch ignore-add '<regex>'")
		}
		return watch.RunIgnoreAdd(watch.DefaultPaths(), args[0])

	case "send-card":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		mid := fs.String("mid", "", "原消息 message_id")
		var drafts multiFlag
		fs.Var(&drafts, "draft", "草稿文件路径（- 为 stdin；可重复给出多候选）")
		original := fs.String("original", "", "原消息文本")
		from := fs.String("from", "", "发送者名")
		scene := fs.String("scene", "", "私聊或群名")
		t := fs.String("t", "", "消息时间")
		format := fs.String("format", "text", "草稿格式：text | markdown（markdown 以 post 富文本回复）")
		note := fs.String("note", "", "判断依据状态行（表态门禁场景带上）")
		fs.Parse(args)
		if *mid == "" || len(drafts) == 0 || (*format != "text" && *format != "markdown") {
			return fmt.Errorf("usage: lark-watch send-card --mid <mid> --draft <file|-> [--draft <file>]... [--format text|markdown] [--original <text>] [--from <name>] [--scene <私聊|群名>] [--t <time>] [--note <text>]")
		}
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		return watch.RunSendCard(s, cli, watch.DefaultPaths(), *mid, drafts, *original, *from, *scene, *t, *format, *note)

	case watch.CmdSendDraft:
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		mid := fs.String(watch.FlagMid, "", "原消息 message_id（pending 键）")
		idx := fs.Int("idx", 0, "候选索引（0 = 候选①）")
		fs.Parse(args)
		if *mid == "" {
			return fmt.Errorf("usage: lark-watch send-draft --mid <mid> [--idx N]")
		}
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		return watch.RunSendDraft(daemonCtx(), s, cli, watch.DefaultPaths(), *mid, *idx)

	case watch.CmdSendText:
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		mid := fs.String(watch.FlagMid, "", "源消息 message_id")
		text := fs.String(watch.FlagText, "", "常用语文本")
		fs.Parse(args)
		if *mid == "" || *text == "" {
			return fmt.Errorf("usage: lark-watch send-text --mid <mid> --text <text>")
		}
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		return watch.RunSendText(daemonCtx(), s, cli, watch.DefaultPaths(), *mid, *text)

	case watch.CmdReact:
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		mid := fs.String(watch.FlagMid, "", "源消息 message_id")
		emoji := fs.String(watch.FlagEmoji, "THUMBSUP", "飞书 emoji_type（如 THUMBSUP/OK/DONE）")
		fs.Parse(args)
		if *mid == "" {
			return fmt.Errorf("usage: lark-watch react --mid <mid> [--emoji THUMBSUP]")
		}
		return watch.RunReact(daemonCtx(), cli, watch.DefaultPaths(), *mid, *emoji)

	case "notify":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		title := fs.String("title", "飞书提醒", "通知标题")
		message := fs.String("message", "", "通知内容")
		link := fs.String("link", "", "点击「跳转」打开的 applink（lark://…）")
		fs.Parse(args)
		if *message == "" {
			return fmt.Errorf("usage: lark-watch notify --message <text> [--title <t>] [--link <lark://…>]")
		}
		return watch.RunNotifyCommand(daemonCtx(), watch.DefaultPaths(), *title, *message, *link)

	case "status":
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		return watch.RunStatus(s, cli, watch.DefaultPaths())

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}
