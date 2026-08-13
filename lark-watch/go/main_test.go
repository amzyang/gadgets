package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// execute 跑一次完整命令树解析与执行；evlog 未 Init 保持 discard，测试不落盘。
func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// 缺必填参数必须在触达 store/lark-cli 前返回 usage 错误（横幅回调是生产路径）。
func TestMissingRequiredFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"send-card 无参", []string{"send-card"}},
		{"send-card format 枚举", []string{"send-card", "--mid", "m", "--draft", "-", "--format", "bogus"}},
		{"send-book-card 无参", []string{"send-book-card"}},
		{"send-draft 无 mid", []string{"send-draft"}},
		{"send-text 缺 text", []string{"send-text", "--mid", "m"}},
		{"react 无 mid", []string{"react"}},
		{"notify 无 message", []string{"notify"}},
		{"notify 只给 mid 不定时", []string{"notify", "--mid", "m", "--message", "x"}},
		{"notify 只给 cid 不定时", []string{"notify", "--message", "x", "--cid", "oc_x"}},
		{"notify 只给 from 不定时", []string{"notify", "--message", "x", "--from", "李四"}},
		{"mark 无参", []string{"mark"}},
		{"ignore-add 无参", []string{"ignore-add"}},
		{"ignore-add 空正则", []string{"ignore-add", ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := execute(t, c.args...)
			if err == nil || !strings.Contains(err.Error(), "usage:") {
				t.Fatalf("want usage error, got %v", err)
			}
		})
	}
}

// usage 文案由 Use 声明派生（usageErr + DisableFlagsInUseLine），钉住确切形状。
func TestUsageErrorShape(t *testing.T) {
	_, err := execute(t, "send-text", "--mid", "m")
	want := "usage: lark-watch send-text --mid <mid> --text <text>"
	if err == nil || err.Error() != want {
		t.Fatalf("want %q, got %v", want, err)
	}
}

func TestRootHelpListsAllCommands(t *testing.T) {
	out, err := execute(t, "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, name := range []string{
		"run", "poll", "card-daemon", "catchup", "mark", "ignore-add",
		"send-card", "send-book-card", "send-draft", "send-text", "react", "notify", "status",
	} {
		if !strings.Contains(out, name) {
			t.Errorf("root help 缺少命令 %s", name)
		}
	}
}

func TestSubcommandHelp(t *testing.T) {
	out, err := execute(t, "send-card", "--help")
	if err != nil {
		t.Fatalf("send-card --help: %v", err)
	}
	for _, flag := range []string{"--mid", "--draft", "--label", "--format", "--note"} {
		if !strings.Contains(out, flag) {
			t.Errorf("send-card help 缺少 %s", flag)
		}
	}
}

// --draft/--slot/--label 必须是 StringArray 语义（值含逗号不拆分）；
// send-book-card 的 -p 单横线写法（历史 CLI 契约）必须继续工作。
func TestStringArrayFlags(t *testing.T) {
	cases := []struct {
		name, cmd, flag string
		args            []string
		want            []string
	}{
		{"draft 含逗号不拆分", "send-card", "draft",
			[]string{"--draft", "a,b.md", "--draft", "c.md"}, []string{"a,b.md", "c.md"}},
		{"label 含逗号不拆分", "send-card", "label",
			[]string{"--label", "改期,顺延", "--label", "拒绝"}, []string{"改期,顺延", "拒绝"}},
		{"slot 含逗号不拆分", "send-book-card", "slot",
			[]string{"--slot", "07-01 10:00-11:00,备选", "--slot", "07-02 10:00-11:00"},
			[]string{"07-01 10:00-11:00,备选", "07-02 10:00-11:00"}},
		{"-p 单横线 shorthand", "send-book-card", "p",
			[]string{"-p", "alice", "-p", "oc_x"}, []string{"alice", "oc_x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, _, err := newRootCmd().Find([]string{c.cmd})
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.ParseFlags(c.args); err != nil {
				t.Fatal(err)
			}
			got, err := cmd.Flags().GetStringArray(c.flag)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("want %v, got %v", c.want, got)
			}
		})
	}
}

// 定时参数错误必须在触达 store 前返回（测试不落真实 StateDir）：互斥、格式错。
// 合法定时值会写库，此处只走必败路径。
func TestNotifyScheduleFlagErrors(t *testing.T) {
	cases := []struct {
		name, want string
		args       []string
	}{
		{"at 与 in 互斥", "互斥", []string{"notify", "--message", "x", "--at", "09:00", "--in", "5m"}},
		{"at 格式错", "invalid --at", []string{"notify", "--message", "x", "--at", "9:15"}},
		{"in 非法", "invalid", []string{"notify", "--message", "x", "--in", "-5m"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := execute(t, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestUnknownCommand(t *testing.T) {
	_, err := execute(t, "bogus")
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("want unknown command error, got %v", err)
	}
}
