package watch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// utf16Width 按 UTF-16 code unit 计宽——Monitor 截断口径的保守上界
// （rune 模型下只会更短）。
func utf16Width(s string) int {
	w := 0
	for _, r := range s {
		if r > 0xFFFF {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// assertChunkLines 校验分片行的通用性质（行宽、判别字段、seq 连续），
// 返回按序拼接的 data 还原串。
func assertChunkLines(t *testing.T, lines [][]byte) string {
	t.Helper()
	var data strings.Builder
	for i, line := range lines {
		body := string(bytes.TrimSuffix(line, []byte("\n")))
		if w := utf16Width(body); w > 480 {
			t.Errorf("line %d 宽度 %d 超 480", i, w)
		}
		var c Chunk
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatalf("line %d 不是合法 JSON: %v", i, err)
		}
		if c.P != "chunk" || c.Seq != i+1 || c.Of != len(lines) {
			t.Errorf("line %d 判别字段异常: p=%q seq=%d of=%d（期望 seq=%d of=%d）",
				i, c.P, c.Seq, c.Of, i+1, len(lines))
		}
		data.WriteString(c.Data)
	}
	return data.String()
}

func TestEncodeLinesPassthrough(t *testing.T) {
	chat := "测试群A"
	from := "张三"
	for name, v := range map[string]any{
		"alert":  NewAlert("auth", "token 过期，请重新 lark-cli auth login"),
		"digest": BuildDigest([]Message{{Cid: "oc_1", Chat: &chat, From: &from, Text: "短消息", T: "2026-07-21 15:51"}}),
	} {
		lines := EncodeLines(v)
		if len(lines) != 1 {
			t.Fatalf("%s: 短事件应恰 1 行，得 %d 行", name, len(lines))
		}
		if !bytes.Equal(lines[0], EncodeLine(v)) {
			t.Errorf("%s: 直通行与 EncodeLine 字节不一致", name)
		}
	}
}

func TestEncodeLinesRoundTrip(t *testing.T) {
	from := "严萍"
	msgs := make([]Message, 0, 12)
	for i := 0; i < 12; i++ {
		chat := fmt.Sprintf("测试群「%d」含引号\"反斜杠\\换行\n表情🚀", i)
		msgs = append(msgs, Message{
			Cid:  fmt.Sprintf("oc_%032d", i),
			Chat: &chat,
			From: &from,
			Text: strings.Repeat("正文🚀带引号\"和反斜杠\\", 5),
			T:    "2026-07-21 15:51",
		})
	}
	d := BuildDigest(msgs)
	lines := EncodeLines(d)
	if len(lines) < 2 {
		t.Fatalf("超长事件应分片，只得 %d 行", len(lines))
	}
	got := assertChunkLines(t, lines)
	orig := EncodeLine(d)
	want := string(bytes.TrimSuffix(orig, []byte("\n")))
	if got != want {
		t.Errorf("data 拼接与原行不一致\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	var back Digest
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("拼接串不是合法 JSON: %v", err)
	}
	if !reflect.DeepEqual(back, d) {
		t.Errorf("还原结构与原事件不等\n--- got ---\n%+v\n--- want ---\n%+v", back, d)
	}
}

func TestEncodeLinesAdversarialWidth(t *testing.T) {
	for name, msg := range map[string]string{
		"quotes-backslashes": strings.Repeat(`"\`, 400),
		"astral-emoji":       strings.Repeat("🚀", 500),
	} {
		a := NewAlert("test", msg)
		lines := EncodeLines(a)
		if len(lines) < 2 {
			t.Fatalf("%s: 应分片，只得 %d 行", name, len(lines))
		}
		got := assertChunkLines(t, lines)
		orig := EncodeLine(a)
		if got != string(bytes.TrimSuffix(orig, []byte("\n"))) {
			t.Errorf("%s: data 拼接与原行不一致", name)
		}
	}
}

func TestSplitEscaped(t *testing.T) {
	s := `abc"def\月亮🚀xyz`
	segs := splitEscaped(s, 4)
	if strings.Join(segs, "") != s {
		t.Errorf("段拼接 %q != 原串 %q", strings.Join(segs, ""), s)
	}
	for i, seg := range segs {
		if !utf8.ValidString(seg) {
			t.Errorf("段 %d 不是合法 UTF-8: %q", i, seg)
		}
		w := 0
		for _, r := range seg {
			w += escapedWidth(r)
		}
		if w > 4 {
			t.Errorf("段 %d 转义宽度 %d 超预算 4: %q", i, w, seg)
		}
	}
	// 宽 2 字符恰在预算边缘时整体进下一段
	if got := splitEscaped("aaa🚀", 4); !reflect.DeepEqual(got, []string{"aaa", "🚀"}) {
		t.Errorf("边界切分 = %q，期望 [aaa 🚀]", got)
	}
}

func TestEmitLinesChunkedAudit(t *testing.T) {
	logs := captureEvlog(t)
	var calls [][]byte
	out := func(b []byte) { calls = append(calls, append([]byte(nil), b...)) }

	long := NewAlert("test", strings.Repeat("长", 600))
	emitLines(out, long)
	if len(calls) != 1 {
		t.Fatalf("长事件应单次写出，实际 %d 次", len(calls))
	}
	lines := bytes.Split(bytes.TrimSuffix(calls[0], []byte("\n")), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("长事件应多行，得 %d 行", len(lines))
	}
	for i, line := range lines {
		if w := utf16Width(string(line)); w > 480 {
			t.Errorf("line %d 宽度 %d 超 480", i, w)
		}
	}
	if got := findLogs(logs(), "emit"); len(got) != 1 {
		t.Errorf("emit 审计应 1 条，得 %d", len(got))
	}
	chunked := findLogs(logs(), "emit.chunked")
	if len(chunked) != 1 {
		t.Fatalf("emit.chunked 审计应 1 条，得 %d", len(chunked))
	}
	if n := int(chunked[0]["lines"].(float64)); n != len(lines) {
		t.Errorf("emit.chunked lines=%d，实际行数 %d", n, len(lines))
	}

	calls = nil
	short := NewAlert("test", "ok")
	emitLines(out, short)
	if !bytes.Equal(calls[0], EncodeLine(short)) {
		t.Errorf("短事件写出与 EncodeLine 字节不一致")
	}
	if got := findLogs(logs(), "emit.chunked"); len(got) != 1 {
		t.Errorf("短事件不应新增 emit.chunked，共 %d 条", len(got))
	}
}
