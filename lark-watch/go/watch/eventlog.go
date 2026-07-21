package watch

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const eventLogName = "events.log"

// evlog 是包级事件诊断日志器（与 logf/bellFn 同一 seam 范式）：默认 discard，
// InitEventLog 激活为 <StateDir>/events.log 的 NDJSON；测试直接替换（captureEvlog）。
var evlog = slog.New(slog.DiscardHandler)

// 事件日志开关与阈值（对齐 envInt/LW_* 风格）：默认开启，LW_EVENT_LOG=0 关闭。
func eventLogEnabled() bool   { return os.Getenv("LW_EVENT_LOG") != "0" }
func eventLogMaxBytes() int64 { return int64(envInt("LW_EVENT_LOG_MAX_MB", 10)) << 20 }

func eventLogLevel() slog.Level {
	if os.Getenv("LW_EVENT_LOG_LEVEL") == "debug" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func eventLogPath(stateDir string) string { return filepath.Join(stateDir, eventLogName) }

// InitEventLog 激活事件诊断日志。返回 closer：只关底层文件、不还原 evlog——
// 已 fork 的通知 goroutine 退出前可能还在写，写已关闭文件仅得 error 且被 slog
// 吞掉。关闭或打开失败时保持 discard 并 logf 告警（best-effort）。
// 短命令（send-card/status）与守护进程并发追加靠 O_APPEND 保序。
func InitEventLog(stateDir string) func() {
	noop := func() {}
	if !eventLogEnabled() {
		return noop
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		logf("event log disabled: %v", err)
		return noop
	}
	w, err := openRotatingWriter(eventLogPath(stateDir), eventLogMaxBytes())
	if err != nil {
		logf("event log disabled: %v", err)
		return noop
	}
	evlog = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: eventLogLevel()}))
	return func() { w.Close() }
}

// LogCmdError 把顶层子命令失败记入事件日志（main.dispatch 出口统一调用，须在
// evlog closer 生效前）：return err 路径原本只到 stderr（前台才可见），审计面
// 必须留痕；命令级失败统一按 level=="ERROR" 过滤。
func LogCmdError(cmd string, err error) {
	evlog.Error("cmd.error", "cmd", cmd, "err", err.Error())
}

// logEmit 把每条 stdout 事件记入诊断日志（kind + 关键 id；正文已在 msg.keep 记过）。
// attr 键避开 slog 保留的 msg/level/time（alert 正文用 text）。
func logEmit(v any) {
	switch e := v.(type) {
	case Message:
		n := e.N
		if n == 0 {
			n = 1
		}
		evlog.Info("emit", "kind", "p0", "mid", e.Mid, "cid", e.Cid, "n", n, "replied", e.Replied)
	case Digest:
		evlog.Info("emit", "kind", "digest", "n", e.N, "chats", len(e.Chats))
	case Alert:
		evlog.Info("emit", "kind", "alert", "alert_kind", e.Kind, "text", e.Msg)
	case Backlog:
		evlog.Info("emit", "kind", "backlog", "offline_secs", e.OfflineSecs)
	default:
		evlog.Info("emit", "kind", fmt.Sprintf("%T", v))
	}
}

// msgAttrs 是 msg.keep/msg.drop 的公共 attrs（text 截 30 码点，足够对上原消息）。
func msgAttrs(m Message) []any {
	return []any{"mid", m.Mid, "cid", m.Cid, "from", deref(m.From), "chat", deref(m.Chat),
		"reason", m.Reason, "text", truncateRunes(m.Text, 30)}
}

// mids 提取批次 mid 列表（通知链路日志用）。
func mids(batch []Message) []string {
	out := make([]string, 0, len(batch))
	for _, m := range batch {
		out = append(out, m.Mid)
	}
	return out
}

// rotatingWriter 是 size 感知的追加 writer：Write 前超限即轮转一代
// （events.log → events.log.1，旧 .1 被覆盖）。mutex 串行化 poller/卡片/
// 通知 goroutine 的并发写。size 从打开时 Stat 起算；其他短命进程同时
// O_APPEND 造成的少量计数漂移可接受——轮转阈值是软限，多进程同时轮转
// 最坏丢一代日志（单用户场景可接受）。
type rotatingWriter struct {
	mu    sync.Mutex
	f     *os.File
	path  string
	size  int64
	limit int64
}

func openRotatingWriter(path string, limit int64) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &rotatingWriter{f: f, path: path, limit: limit}
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
	return w, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.limit {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate 轮转一代；调用方持锁。任一步失败 best-effort 降级继续写旧句柄。
// 告警必须直写 os.Stderr，绝不可经 logf——tee 回 evlog 会重入 w.mu 死锁。
func (w *rotatingWriter) rotate() {
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		fmt.Fprintf(os.Stderr, "[lark-watch] event log rotate failed: %v\n", err)
		return
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil { // 旧句柄此刻指向 .1，继续追加不丢日志
		fmt.Fprintf(os.Stderr, "[lark-watch] event log reopen failed: %v\n", err)
		return
	}
	w.f.Close()
	w.f, w.size = f, 0
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
