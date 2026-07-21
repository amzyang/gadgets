package watch

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// syncBuf 是并发安全的内存 buffer：emit/notify 链路在 goroutine 里写日志，
// -race 下捕获必须加锁。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuf) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.b.Bytes()...)
}

// captureEvlogAt 把 evlog 换成指定级别的内存 JSON handler，返回逐行解析的取行
// 函数。包级 seam（同 bellFn 约束）：使用方不得 t.Parallel。
func captureEvlogAt(t *testing.T, level slog.Level) func() []map[string]any {
	t.Helper()
	buf := &syncBuf{}
	old := evlog
	evlog = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level}))
	t.Cleanup(func() { evlog = old })
	return func() []map[string]any { return parseLogLines(t, buf.bytes()) }
}

// captureEvlog 是 debug 级捕获（默认取全量，级别断言用 level 字段判）。
func captureEvlog(t *testing.T) func() []map[string]any {
	t.Helper()
	return captureEvlogAt(t, slog.LevelDebug)
}

func parseLogLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// logsContain 判断是否存在 msg 含指定子串的记录（logf/cardLogf 文本行断言用）。
func logsContain(recs []map[string]any, substr string) bool {
	for _, r := range recs {
		if s, _ := r["msg"].(string); strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// findLogs 过滤出指定 msg 的日志行。
func findLogs(recs []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// 轮转：越限时 rename 为 .1（旧 .1 被覆盖，只留一代），当前文件从零续写。
func TestRotatingWriterRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	w, err := openRotatingWriter(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	chunk := func(c byte) []byte { return bytes.Repeat([]byte{c}, 40) }
	for _, c := range []byte{'a', 'b'} {
		if _, err := w.Write(chunk(c)); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(path + ".1"); !bytes.Equal(b, chunk('a')) {
		t.Errorf("rotated file: got %q, want 40×a", b)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, chunk('b')) {
		t.Errorf("current file: got %q, want 40×b", b)
	}

	if _, err := w.Write(chunk('c')); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path + ".1"); !bytes.Equal(b, chunk('b')) {
		t.Errorf("old generation should be overwritten: got %q, want 40×b", b)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, chunk('c')) {
		t.Errorf("current file: got %q, want 40×c", b)
	}
}

// 打开时 Stat 恢复 size：进程重启后既有内容计入轮转阈值。
func TestRotatingWriterResumesSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	pre := bytes.Repeat([]byte("x"), 60)
	if err := os.WriteFile(path, pre, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := openRotatingWriter(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path + ".1"); !bytes.Equal(b, pre) {
		t.Errorf("pre-existing content should rotate out: got %q", b)
	}
	if b, _ := os.ReadFile(path); string(b) != "fresh" {
		t.Errorf("current file: got %q, want %q", b, "fresh")
	}
}

// InitEventLog：激活后 evlog 写 <stateDir>/events.log，每行合法 JSON。
func TestInitEventLogWritesNDJSON(t *testing.T) {
	dir := t.TempDir()
	old := evlog
	t.Cleanup(func() { evlog = old })

	closeLog := InitEventLog(dir)
	evlog.Info("hello", "k", "v")
	closeLog()

	b, err := os.ReadFile(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatalf("events.log not written: %v", err)
	}
	recs := parseLogLines(t, b)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d: %s", len(recs), b)
	}
	r := recs[0]
	if r["msg"] != "hello" || r["k"] != "v" || r["level"] != "INFO" || r["time"] == nil {
		t.Errorf("record fields: %v", r)
	}
}

// LW_EVENT_LOG=0 关闭：不建文件，evlog 保持 discard。
func TestInitEventLogDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LW_EVENT_LOG", "0")
	old := evlog
	t.Cleanup(func() { evlog = old })

	closeLog := InitEventLog(dir)
	evlog.Info("dropped")
	closeLog()

	if _, err := os.Stat(filepath.Join(dir, "events.log")); err == nil {
		t.Error("events.log created despite LW_EVENT_LOG=0")
	}
}

// 级别：默认 info 抑制 Debug；LW_EVENT_LOG_LEVEL=debug 放行。
func TestInitEventLogLevel(t *testing.T) {
	old := evlog
	t.Cleanup(func() { evlog = old })

	dir := t.TempDir()
	closeLog := InitEventLog(dir)
	evlog.Debug("quiet")
	evlog.Info("loud")
	closeLog()
	b, _ := os.ReadFile(filepath.Join(dir, "events.log"))
	if recs := parseLogLines(t, b); len(recs) != 1 || recs[0]["msg"] != "loud" {
		t.Errorf("info level should drop debug records, got %s", b)
	}

	t.Setenv("LW_EVENT_LOG_LEVEL", "debug")
	dir2 := t.TempDir()
	closeLog2 := InitEventLog(dir2)
	evlog.Debug("quiet")
	closeLog2()
	b2, _ := os.ReadFile(filepath.Join(dir2, "events.log"))
	if recs := parseLogLines(t, b2); len(recs) != 1 || recs[0]["msg"] != "quiet" {
		t.Errorf("debug level should keep debug records, got %s", b2)
	}
}

// tee：logf/cardLogf 保持 stderr 行为不变，同时以 Info 级入档（component 区分链路）。
func TestLogfTee(t *testing.T) {
	logs := captureEvlog(t)

	logf("hello %d", 1)
	cardLogf("world %s", "x")

	recs := logs()
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d: %v", len(recs), recs)
	}
	if r := recs[0]; r["msg"] != "hello 1" || r["component"] != "watch" {
		t.Errorf("logf tee: %v", r)
	}
	if r := recs[1]; r["msg"] != "world x" || r["component"] != "card" {
		t.Errorf("cardLogf tee: %v", r)
	}
}

// cmd.error：顶层子命令 return err 原本只到 stderr，审计必须在 events.log
// 留痕（Error 级 + cmd/err attrs，`level=="ERROR"` 可过滤全部命令级失败）。
func TestLogCmdError(t *testing.T) {
	logs := captureEvlog(t)

	LogCmdError("send-card", errors.New("send card failed: boom"))

	recs := findLogs(logs(), "cmd.error")
	if len(recs) != 1 {
		t.Fatalf("want 1 cmd.error record, got %v", recs)
	}
	r := recs[0]
	if r["level"] != "ERROR" || r["cmd"] != "send-card" || r["err"] != "send card failed: boom" {
		t.Errorf("cmd.error fields: %v", r)
	}
}

// 打开失败（stateDir 是普通文件）：降级为禁用，不 panic。
func TestInitEventLogOpenFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := evlog
	t.Cleanup(func() { evlog = old })

	closeLog := InitEventLog(file)
	evlog.Info("dropped")
	closeLog()
}
