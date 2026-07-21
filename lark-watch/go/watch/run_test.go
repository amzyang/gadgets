package watch

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// consumeCLI 覆写 EventConsumeCmd 注入任意子进程命令，其余复用 fakeCLI。
type consumeCLI struct {
	fakeCLI
	cmd func(ctx context.Context) *exec.Cmd
}

func (c *consumeCLI) EventConsumeCmd(ctx context.Context) *exec.Cmd { return c.cmd(ctx) }

// cancelReturnsWithin 跑 runConsumerOnce，稍候取消 ctx，断言限时内返回。
func cancelReturnsWithin(t *testing.T, shellCmd string, limit time.Duration) {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli := &consumeCLI{cmd: func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", shellCmd)
	}}
	done := make(chan struct{})
	go func() { runConsumerOnce(ctx, s, cli, "ou_SELF"); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("runConsumerOnce did not return within %s after cancel", limit)
	}
}

// 关停死锁回归（2026-07-21 实证）：cmd.Cancel 只信号 npm bin shim，真正的
// node 进程孤儿化后继续握着 stdout 管道写端，sc.Scan 永不 EOF → cmd.Wait
// 走不到（shim 成僵尸）→ RunDaemon 的 wg.Wait 永久挂起。
// 场景 A：直接子进程可被 TERM，后台孙进程握管道——进程组信号须全灭。
func TestRunConsumerOnceCancelKillsProcessTree(t *testing.T) {
	cancelReturnsWithin(t, "sleep 5 & exec sleep 5", 2*time.Second)
}

// 场景 B：整棵树忽略 TERM（忽略处置随 fork/exec 继承），组信号无效——
// 读端兜底在宽限期后强关 stdout 解除 Scan，WaitDelay 强杀直接子进程，
// 关停不依赖子进程配合。
func TestRunConsumerOnceCancelSurvivesTermImmuneTree(t *testing.T) {
	oldGrace, oldDelay := consumerStopGrace, consumerWaitDelay
	consumerStopGrace, consumerWaitDelay = 200*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { consumerStopGrace, consumerWaitDelay = oldGrace, oldDelay })
	cancelReturnsWithin(t, `trap "" TERM; sleep 5 & exec sleep 5`, 2*time.Second)
}
