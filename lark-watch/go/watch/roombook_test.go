package watch

import (
	"errors"
	"strings"
	"testing"
)

func TestParseBookSuccess(t *testing.T) {
	out := `{"ok":true,"data":{"event_id":"ev_1","date":"2026-07-22","start_time":"14:00","end_time":"15:00","room":{"name":"A栋3F-301"}}}`
	res, err := parseBookSuccess([]byte(out + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := BookResult{Room: "A栋3F-301", Date: "2026-07-22", Start: "14:00", End: "15:00", EventID: "ev_1"}
	if res != want {
		t.Errorf("want %+v, got %+v", want, res)
	}
}

// exit 0 但输出解析不出：不编造预订细节，以 parse 类错误提示核对（不误报成功）。
func TestParseBookSuccessUnparseable(t *testing.T) {
	for name, out := range map[string]string{
		"非JSON":    "oops",
		"ok=false": `{"ok":false}`,
	} {
		_, err := parseBookSuccess([]byte(out))
		var be *BookError
		if !errors.As(err, &be) || be.Type != "parse" {
			t.Errorf("%s: want parse BookError, got %v", name, err)
		}
	}
}

func TestParseBookError(t *testing.T) {
	stderr := "some log line\n" +
		`{"ok":false,"error":{"type":"no_room","message":"该时段无可用会议室","hint":"换时段重试","retryable":false}}`
	be := parseBookError([]byte(stderr), errors.New("exit status 1"))
	if be.Type != "no_room" || be.Message != "该时段无可用会议室" || be.Hint != "换时段重试" {
		t.Errorf("envelope fields: %+v", be)
	}
}

// stderr 最后一行不是 JSON（flag 用法错误 / room 未安装）：降级为原始错误文本。
func TestParseBookErrorNonJSON(t *testing.T) {
	be := parseBookError([]byte("usage: room book ..."), errors.New("exit status 2"))
	if be.Type != "exec" || !strings.Contains(be.Message, "exit status 2") {
		t.Errorf("want exec fallback, got %+v", be)
	}
	if !strings.Contains(be.Hint, "usage") {
		t.Errorf("hint should carry stderr tail: %+v", be)
	}
}
