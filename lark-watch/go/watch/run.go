package watch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// authAlertMsg 把 auth 类错误（AuthSelf 失败、轮询鉴权失效）翻成给用户的
// 行动指引（alert msg 即全部信息，模型只需转述）。
func authAlertMsg(err error) string {
	if errors.Is(err, exec.ErrNotFound) {
		return "lark-cli 未安装或不在 PATH：npm i -g @larksuite/cli 后重启 Monitor"
	}
	return "user 身份不可用：请运行 lark-cli auth login --domain im,contact，完成后重启 Monitor"
}

// authExpiringMsg 在 user token 刷新期不足 24h 时返回提醒文案；零值视为未知不告警。
func authExpiringMsg(refreshExpiresAt, now time.Time) string {
	if refreshExpiresAt.IsZero() || refreshExpiresAt.Sub(now) >= 24*time.Hour {
		return ""
	}
	hours := int(refreshExpiresAt.Sub(now).Hours())
	if hours < 0 {
		hours = 0
	}
	return fmt.Sprintf("user token 刷新期仅剩约 %d 小时，请尽快 lark-cli auth login（Monitor 继续运行）", hours)
}

// RunDaemon 是 run/poll 子命令：poller goroutine，withCards 时外加卡片 consume
// 子进程监督 goroutine。稳态卡片链路零 stdout（零模型唤醒）；仅 poller 事件与
// 异常 alert 走 stdout。
func RunDaemon(ctx context.Context, s *Store, cli LarkCLI, paths Paths, interval time.Duration, digestWindow int64, digestMax int, withCards bool) error {
	w := &lineWriter{}
	auth, err := cli.AuthSelf()
	if err != nil {
		a := NewAlert("auth", authAlertMsg(err))
		w.Write(EncodeLine(a))
		logEmit(a)
		return err
	}
	if msg := authExpiringMsg(auth.RefreshExpiresAt, time.Now()); msg != "" {
		a := NewAlert("auth-expiring", msg)
		w.Write(EncodeLine(a))
		logEmit(a)
	}
	self := auth.OpenID

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

	h := &CardHandler{Store: s, CLI: cli, Booker: &ExecRoomBooker{}, Self: self, Out: out}
	fastFails := 0
	alerted := false
	backoffs := []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second}

	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := runConsumerOnce(ctx, s, cli, h)
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
			a := NewAlert("card-daemon",
				"卡片回调监听连续快速失败，正在退避重启；卡片按钮暂不可用，详见 stderr")
			out(EncodeLine(a))
			logEmit(a)
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

// 关停时序参数（测试注入）：WaitDelay 兜底强杀直接子进程；StopGrace 是
// 组 TERM 后强关 stdout 读端前的优雅退订窗口。
var (
	consumerWaitDelay = 10 * time.Second
	consumerStopGrace = 10 * time.Second
)

// runConsumerOnce 跑一轮 consume 子进程直到其退出。
// stdin 由父进程持有写端保活（无界 consume 在 stdin EOF 时会优雅退出）。
func runConsumerOnce(ctx context.Context, s *Store, cli LarkCLI, h *CardHandler) error {
	cmd := cli.EventConsumeCmd(ctx)
	// npm bin shim 会再 spawn 真正的 node 进程：信号只发直接子进程会漏掉它，
	// 孤儿继续握着 stdout 写端把 Scan 钉死、cmd.Wait 永不可达（2026-07-21
	// 关停死锁实证）。自建进程组、按组 SIGTERM，整棵树一起优雅退订。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = consumerWaitDelay
	stopGrace := consumerStopGrace // 入口取值：兜底 goroutine 可比本函数晚退，不读包级变量
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
	// 读端兜底：对自建会话/忽略 TERM 的后代，组信号无效，宽限期后强关
	// stdout 读端解除 Scan 阻塞——关停不依赖子进程配合。与 Wait 的收尾
	// Close 并发安全（*os.File 双 Close 幂等）。
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-watchDone:
		case <-ctx.Done():
			select {
			case <-watchDone:
			case <-time.After(stopGrace):
				stdout.Close()
			}
		}
	}()
	s.MetaSet("consumer_state", "alive")
	cardLogf("consumer started (pid %d)", cmd.Process.Pid)

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		h.Handle(line, time.Now().Unix())
	}
	return cmd.Wait()
}
