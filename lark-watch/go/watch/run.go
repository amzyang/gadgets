package watch

import (
	"bufio"
	"context"
	"os"
	"sync"
	"syscall"
	"time"
)

// stdout 单写者：poller 与卡片链路可能并发发事件行，避免交错写。
type lineWriter struct {
	mu sync.Mutex
}

func (w *lineWriter) Write(line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	os.Stdout.Write(line)
}

// RunDaemon 是 run/poll 子命令：poller goroutine，withCards 时外加卡片 consume
// 子进程监督 goroutine。稳态卡片链路零 stdout（零模型唤醒）；仅 poller 事件与
// 异常 alert 走 stdout。
func RunDaemon(ctx context.Context, s *Store, cli LarkCLI, paths Paths, interval time.Duration, digestWindow int64, digestMax int, withCards bool) error {
	w := &lineWriter{}
	self, err := cli.AuthSelf()
	if err != nil {
		w.Write(EncodeLine(NewAlert("auth", "无法获取用户身份 open_id，请先 lark-cli auth login")))
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	if withCards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			superviseCardConsumer(ctx, s, cli, self, w.Write)
		}()
	}

	p := &Poller{
		Store: s, CLI: cli, Paths: paths,
		Interval: interval, DigestWindow: digestWindow, DigestMax: digestMax,
		Out: w.Write,
	}
	err = p.Run(ctx, self)
	cancel() // poller 结束（auth 失效或取消）时同步停掉卡片链路
	wg.Wait()
	return err
}

// superviseCardConsumer 监督 lark-cli event consume 子进程：
// 异常退出退避重启（5s→15s→60s），连续 3 次快速失败发一条 alert（仅降级卡片功能，
// poller 不受影响）。SIGTERM 经 cmd.Cancel 传递，勿 kill -9（泄漏服务端订阅）。
func superviseCardConsumer(ctx context.Context, s *Store, cli LarkCLI, self string, out func(line []byte)) {
	defer s.MetaSet("consumer_state", "stopped")

	fastFails := 0
	alerted := false
	backoffs := []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second}

	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := runConsumerOnce(ctx, s, cli, self)
		if ctx.Err() != nil {
			cardLogf("consumer stopped")
			return
		}
		s.MetaSet("consumer_state", "restarting")
		if time.Since(start) < 30*time.Second {
			fastFails++
		} else {
			fastFails = 0
			alerted = false
		}
		if fastFails >= 3 && !alerted {
			out(EncodeLine(NewAlert("card-daemon",
				"卡片回调监听连续快速失败，正在退避重启；卡片按钮暂不可用，详见 stderr")))
			alerted = true
		}
		wait := backoffs[min(fastFails, len(backoffs)-1)]
		cardLogf("consumer exited (%v), restart in %s", err, wait)
		if sleepCtx(ctx, wait) != nil {
			return
		}
	}
}

// SuperviseCardConsumerStandalone 独立跑卡片回调链路（card-daemon 子命令）。
func SuperviseCardConsumerStandalone(ctx context.Context, s *Store, cli LarkCLI, self string) {
	w := &lineWriter{}
	superviseCardConsumer(ctx, s, cli, self, w.Write)
}

// runConsumerOnce 跑一轮 consume 子进程直到其退出。
// stdin 由父进程持有写端保活（无界 consume 在 stdin EOF 时会优雅退出）。
func runConsumerOnce(ctx context.Context, s *Store, cli LarkCLI, self string) error {
	cmd := cli.EventConsumeCmd(ctx)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.MetaSet("consumer_state", "alive")
	cardLogf("consumer started (pid %d)", cmd.Process.Pid)

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		HandleCardEvent(s, cli, self, line, time.Now().Unix())
	}
	return cmd.Wait()
}
