// lark-watch — 用户视角飞书消息监控与卡片直发（单二进制：poller + 卡片回调）。
// stdout 事件契约：P0/digest/alert/backlog/catchup 单行 JSON；诊断走 stderr。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"lark-watch/watch"
)

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "[lark-watch] %v\n", err)
		os.Exit(1)
	}
}

// realMain 把 evlog closer 收进 defer 作用域（os.Exit 不跑 defer）。
func realMain() error {
	// 事件诊断日志对全部子命令统一开启（默认开，LW_EVENT_LOG=0 关闭）：
	// 守护进程与短命令（send-card/status 等）并发追加同一文件，O_APPEND 保序。
	closeEvlog := watch.InitEventLog(watch.DefaultPaths().StateDir)
	defer closeEvlog()
	root := newRootCmd()
	// 子命令入口留痕（cmd.invoke），与出口的 cmd.error 配对构成命令级审计面。
	// 只认注册在案的子命令：help/completion/__complete 是 Execute 时才挂上的
	// cobra 自带命令，天然不留痕。记原始 argv 才对得上调用形态。
	name := auditedName(root, os.Args)
	if name != "" {
		watch.LogCmdInvoke(name, os.Args[2:])
	}
	err := root.Execute()
	if err != nil && name != "" {
		// 命令级失败入档须在 closer 生效前——evlog 文件关闭后写入被 slog 吞掉。
		watch.LogCmdError(name, err)
	}
	return err
}

// auditedName 返回本次调用要留痕的子命令名；未匹配注册命令时返回空。
func auditedName(root *cobra.Command, argv []string) string {
	if len(argv) < 2 {
		return ""
	}
	for _, c := range root.Commands() {
		if c.Name() == argv[1] {
			return argv[1]
		}
	}
	return ""
}

// withStore 打开 store 执行 fn，统一收口 defer Close（多数子命令的公共前奏）。
func withStore(fn func(*watch.Store) error) error {
	s, err := watch.OpenStore(watch.DefaultPaths().StateDir)
	if err != nil {
		return err
	}
	defer s.Close()
	return fn(s)
}

func daemonCtx() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	return ctx
}

// usageErr 从 Use 声明派生 usage 错误：命令规格只写一处，不随 flag 增删漂移。
// 依赖各命令 DisableFlagsInUseLine，保证文案严格为 "usage: lark-watch <Use>"。
func usageErr(cmd *cobra.Command) error {
	return fmt.Errorf("usage: %s", cmd.UseLine())
}

func newRootCmd() *cobra.Command {
	cli := &watch.ExecLarkCLI{}
	root := &cobra.Command{
		Use:   "lark-watch",
		Short: "用户视角飞书消息监控与卡片直发（单二进制：poller + 卡片回调）",
		// 错误统一由 main 按 "[lark-watch] err" 前缀输出，不让 cobra
		// 重复打印错误与全量 usage。
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newDaemonCmd(cli, "run", "poller + 卡片回调（生产入口，一个 Monitor）", true),
		newDaemonCmd(cli, "poll", "仅消息轮询", false),
		newCardDaemonCmd(cli),
		newCatchupCmd(cli),
		newMarkCmd(),
		newIgnoreAddCmd(),
		newSendCardCmd(cli),
		newSendBookCardCmd(cli),
		newSendDraftCmd(cli),
		newSendTextCmd(cli),
		newReactCmd(cli),
		newNotifyCmd(),
		newStatusCmd(cli),
	)
	return root
}

func newDaemonCmd(cli *watch.ExecLarkCLI, name, short string, withCards bool) *cobra.Command {
	var (
		interval     int
		digestWindow int64
		digestMax    int
	)
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return withStore(func(s *watch.Store) error {
				return watch.RunDaemon(daemonCtx(), s, cli, watch.DefaultPaths(),
					time.Duration(interval)*time.Second, digestWindow, digestMax, withCards)
			})
		},
	}
	cmd.Flags().IntVar(&interval, "interval", 5, "轮询间隔秒数")
	cmd.Flags().Int64Var(&digestWindow, "digest-window", 600, "摘要时间窗秒数")
	cmd.Flags().IntVar(&digestMax, "digest-max", 20, "摘要条数阈值")
	return cmd
}

func newCardDaemonCmd(cli *watch.ExecLarkCLI) *cobra.Command {
	return &cobra.Command{
		Use:   "card-daemon",
		Short: "仅卡片回调",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return withStore(func(s *watch.Store) error {
				auth, err := cli.AuthSelf()
				if err != nil {
					return err
				}
				watch.SuperviseCardConsumerStandalone(daemonCtx(), s, cli, auth.OpenID)
				return nil
			})
		},
	}
}

func newCatchupCmd(cli *watch.ExecLarkCLI) *cobra.Command {
	var (
		since string
		peek  int
	)
	cmd := &cobra.Command{
		Use:   "catchup",
		Short: "补课：拉积压消息按会话分组",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return withStore(func(s *watch.Store) error {
				return watch.RunCatchup(s, cli, watch.DefaultPaths(), since, peek)
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "24h", "首次回看窗口")
	cmd.Flags().IntVar(&peek, "peek", 5, "每会话预览条数")
	return cmd
}

func newMarkCmd() *cobra.Command {
	var (
		all bool
		at  int64
	)
	cmd := &cobra.Command{
		Use:                   "mark <cid>... | mark --all [--at <epoch>]",
		Short:                 "标记会话已处理游标",
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !all && len(args) == 0 {
				return usageErr(cmd)
			}
			return withStore(func(s *watch.Store) error {
				return watch.RunMark(s, args, all, at)
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "标记最近一次 catchup 的全部会话")
	cmd.Flags().Int64Var(&at, "at", time.Now().Unix(), "游标时间（epoch 秒）")
	return cmd
}

func newIgnoreAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "ignore-add '<regex>'",
		Short:                 "追加噪音正则（正则以 - 开头时经 -- 分隔）",
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 || args[0] == "" {
				return usageErr(cmd)
			}
			return watch.RunIgnoreAdd(watch.DefaultPaths(), args[0])
		},
	}
}

func newSendCardCmd(cli *watch.ExecLarkCLI) *cobra.Command {
	var (
		mid, original, from, scene, t, format, note string
		drafts, labels                              []string
	)
	cmd := &cobra.Command{
		Use:                   "send-card --mid <mid> --draft <file|-> [--draft <file>]... [--label <短标签>]... [--format text|markdown] [--original <text>] [--from <name>] [--scene <私聊|群名>] [--t <time>] [--note <text>]",
		Short:                 "起草确认卡片（pending 入库 + 渲染 + bot 私发）",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if mid == "" || len(drafts) == 0 || (format != "text" && format != "markdown") {
				return usageErr(cmd)
			}
			return withStore(func(s *watch.Store) error {
				return watch.RunSendCard(s, cli, watch.DefaultPaths(), mid, drafts, labels, original, from, scene, t, format, note)
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&mid, "mid", "", "原消息 message_id")
	// StringArray 不按逗号拆值（StringSlice 会）——草稿路径/label 可含逗号。
	fl.StringArrayVar(&drafts, "draft", nil, "草稿文件路径（- 为 stdin；可重复给出多候选）")
	fl.StringArrayVar(&labels, "label", nil, "候选方向短标签，与 --draft 等数按序配对（全不给=不标注，空串=该位不标）")
	fl.StringVar(&original, "original", "", "原消息文本")
	fl.StringVar(&from, "from", "", "发送者名")
	fl.StringVar(&scene, "scene", "", "私聊或群名")
	fl.StringVar(&t, "t", "", "消息时间")
	fl.StringVar(&format, "format", "text", "草稿格式：text | markdown（markdown 以 post 富文本回复）")
	fl.StringVar(&note, "note", "", "判断依据状态行（表态门禁场景带上）")
	return cmd
}

func newSendBookCardCmd(cli *watch.ExecLarkCLI) *cobra.Command {
	var (
		mid, title, original, from, scene, t string
		slots, participants                  []string
	)
	cmd := &cobra.Command{
		Use:                   "send-book-card --mid <mid> --slot 'MM-DD HH:MM-HH:MM' [--slot ...] --title <标题> [-p <参会人>]... [--original <text>] [--from <name>] [--scene <私聊|群名>] [--t <time>]",
		Short:                 "预约意向卡片（点「预约」由卡片回调直接 room book）",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if mid == "" || title == "" || len(slots) == 0 {
				return usageErr(cmd)
			}
			parsed, err := watch.ParseBookSlots(slots)
			if err != nil {
				return err
			}
			return withStore(func(s *watch.Store) error {
				return watch.RunSendBookCard(s, cli, mid, parsed, title, participants, original, from, scene, t)
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&mid, "mid", "", "原消息 message_id")
	fl.StringArrayVar(&slots, "slot", nil, "候选时段 'MM-DD HH:MM-HH:MM'（可重复，至多 3 条）")
	fl.StringVar(&title, "title", "", "会议标题")
	fl.StringArrayVarP(&participants, "p", "p", nil, "参会人：邮箱前缀/完整邮箱/oc_ 群 ID（可重复；room 拒绝 ou_）")
	fl.StringVar(&original, "original", "", "原消息文本")
	fl.StringVar(&from, "from", "", "发送者名")
	fl.StringVar(&scene, "scene", "", "私聊或群名")
	fl.StringVar(&t, "t", "", "消息时间")
	return cmd
}

func newSendDraftCmd(cli *watch.ExecLarkCLI) *cobra.Command {
	var (
		mid string
		idx int
	)
	cmd := &cobra.Command{
		Use:                   watch.CmdSendDraft + " --mid <mid> [--idx N]",
		Short:                 "发送 pending 候选草稿（通知横幅「发送」动作的回调）",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if mid == "" {
				return usageErr(cmd)
			}
			return withStore(func(s *watch.Store) error {
				return watch.RunSendDraft(daemonCtx(), s, cli, watch.DefaultPaths(), mid, idx)
			})
		},
	}
	cmd.Flags().StringVar(&mid, watch.FlagMid, "", "原消息 message_id（pending 键）")
	cmd.Flags().IntVar(&idx, "idx", 0, "候选索引（0 = 候选①）")
	return cmd
}

func newSendTextCmd(cli *watch.ExecLarkCLI) *cobra.Command {
	var mid, text string
	cmd := &cobra.Command{
		Use:                   watch.CmdSendText + " --mid <mid> --text <text>",
		Short:                 "以常用语快捷回复源消息（通知横幅动作的回调）",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if mid == "" || text == "" {
				return usageErr(cmd)
			}
			return withStore(func(s *watch.Store) error {
				return watch.RunSendText(daemonCtx(), s, cli, watch.DefaultPaths(), mid, text)
			})
		},
	}
	cmd.Flags().StringVar(&mid, watch.FlagMid, "", "源消息 message_id")
	cmd.Flags().StringVar(&text, watch.FlagText, "", "常用语文本")
	return cmd
}

func newReactCmd(cli *watch.ExecLarkCLI) *cobra.Command {
	var mid, emoji string
	cmd := &cobra.Command{
		Use:                   watch.CmdReact + " --mid <mid> [--emoji THUMBSUP]",
		Short:                 "给源消息加表情回应（通知横幅动作的回调）",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if mid == "" {
				return usageErr(cmd)
			}
			return watch.RunReact(daemonCtx(), cli, watch.DefaultPaths(), mid, emoji)
		},
	}
	cmd.Flags().StringVar(&mid, watch.FlagMid, "", "源消息 message_id")
	cmd.Flags().StringVar(&emoji, watch.FlagEmoji, "THUMBSUP", "飞书 emoji_type（如 THUMBSUP/OK/DONE）")
	return cmd
}

func newNotifyCmd() *cobra.Command {
	var title, message, link, at, in, mid string
	cmd := &cobra.Command{
		Use:                   "notify --message <text> [--title <t>] [--link <lark://…>] [--at '[MM-DD ]HH:MM' | --in <时长>] [--mid <mid>]",
		Short:                 "发送系统通知；--at/--in 定时（落盘延时提醒，由 run daemon 到期弹出）",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if message == "" {
				return usageErr(cmd)
			}
			now := time.Now()
			due, err := watch.ParseRemindDue(at, in, now)
			if err != nil {
				return err
			}
			if due == 0 {
				// --mid 只对定时提醒有意义：立即通知带它多半是想定时却漏了 --at/--in
				if mid != "" {
					return usageErr(cmd)
				}
				return watch.RunNotifyCommand(daemonCtx(), watch.DefaultPaths(), title, message, link)
			}
			return withStore(func(s *watch.Store) error {
				r := watch.Reminder{Mid: mid, Title: title, Message: message, Link: link, Due: due}
				return watch.RunRemind(s, r, now.Unix())
			})
		},
	}
	cmd.Flags().StringVar(&title, "title", "飞书提醒", "通知标题")
	cmd.Flags().StringVar(&message, "message", "", "通知内容")
	cmd.Flags().StringVar(&link, "link", "", "点击「跳转」打开的 applink（lark://…）")
	cmd.Flags().StringVar(&at, "at", "", "定时：'[MM-DD ]HH:MM'（本地时区，缺日期=今天，须为未来时刻）")
	cmd.Flags().StringVar(&in, "in", "", "延时：'90m'/'2h'/纯秒（与 --at 互斥）")
	cmd.Flags().StringVar(&mid, "mid", "", "去重键：同 mid 重复安排覆盖旧提醒（仅配合 --at/--in）")
	return cmd
}

func newStatusCmd(cli *watch.ExecLarkCLI) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "健康 JSON",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return withStore(func(s *watch.Store) error {
				return watch.RunStatus(s, cli, watch.DefaultPaths())
			})
		},
	}
}
