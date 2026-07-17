// lark-watch — 用户视角飞书消息监控与卡片直发（单二进制：poller + 卡片回调）。
// stdout 事件契约：P0/digest/alert/backlog/catchup 单行 JSON；诊断走 stderr。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lark-watch/watch"
)

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
	cli := &watch.ExecLarkCLI{}
	switch cmd {
	case "run", "poll":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		interval := fs.Int("interval", 45, "轮询间隔秒数")
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
		self, err := cli.AuthSelf()
		if err != nil {
			return err
		}
		watch.SuperviseCardConsumerStandalone(daemonCtx(), s, cli, self)
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
		draft := fs.String("draft", "", "草稿文件路径（- 为 stdin）")
		original := fs.String("original", "", "原消息文本")
		from := fs.String("from", "", "发送者名")
		scene := fs.String("scene", "", "私聊或群名")
		t := fs.String("t", "", "消息时间")
		fs.Parse(args)
		if *mid == "" || *draft == "" {
			return fmt.Errorf("usage: lark-watch send-card --mid <mid> --draft <file|-> [--original <text>] [--from <name>] [--scene <私聊|群名>] [--t <time>]")
		}
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		return watch.RunSendCard(s, cli, *mid, *draft, *original, *from, *scene, *t)

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
		return watch.RunStatus(s)

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}
