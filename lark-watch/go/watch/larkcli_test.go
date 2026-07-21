package watch

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 取自真实 `lark-cli auth status` 输出裁剪（scope 等无关字段省略）。
const authStatusFixture = `{
  "appId": "cli_x",
  "identities": {
    "bot": {"status": "ready", "available": true},
    "user": {
      "status": "ready",
      "available": true,
      "openId": "ou_abc",
      "tokenStatus": "valid",
      "expiresAt": "2026-07-17T19:31:21+08:00",
      "refreshExpiresAt": "2026-07-24T17:31:21+08:00"
    }
  },
  "identity": "user"
}`

func TestParseAuthStatus(t *testing.T) {
	info, err := parseAuthStatus([]byte(authStatusFixture))
	if err != nil {
		t.Fatal(err)
	}
	if info.OpenID != "ou_abc" {
		t.Fatalf("openId: want ou_abc, got %q", info.OpenID)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-24T17:31:21+08:00")
	if !info.RefreshExpiresAt.Equal(want) {
		t.Fatalf("refreshExpiresAt: want %v, got %v", want, info.RefreshExpiresAt)
	}
}

func TestParseAuthStatusUnavailable(t *testing.T) {
	cases := map[string]string{
		"available false": `{"identities":{"user":{"available":false,"openId":"ou_abc"}}}`,
		"openId empty":    `{"identities":{"user":{"available":true,"openId":""}}}`,
		"bad json":        `not json`,
	}
	for name, in := range cases {
		if _, err := parseAuthStatus([]byte(in)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

// 发卡响应的 message_id 解析：兼容 data 包装与顶层两种形态；解析不出返回空串
// （发卡本身已成功，改卡凭证缺失只降级为不改卡，不算失败）。
func TestParseSentMessageID(t *testing.T) {
	cases := map[string][2]string{
		"data 包装": {`{"ok":true,"data":{"message_id":"om_wrap","chat_id":"oc_x"}}`, "om_wrap"},
		"顶层":      {`{"message_id":"om_top","chat_id":"oc_x"}`, "om_top"},
		"缺字段":     {`{"ok":true,"data":{}}`, ""},
		"非 JSON":  {`sent`, ""},
	}
	for name, c := range cases {
		if got := parseSentMessageID([]byte(c[0])); got != c[1] {
			t.Errorf("%s: want %q, got %q", name, c[1], got)
		}
	}
}

func TestParseChatAvatar(t *testing.T) {
	cases := map[string][2]string{
		"群":       {`{"ok":true,"data":{"avatar":"https://cdn/g.jpg","name":"群A"}}`, "https://cdn/g.jpg"},
		"p2p 无字段": {`{"ok":true,"data":{"chat_mode":"p2p"}}`, ""},
		"非 JSON":  {`err`, ""},
	}
	for name, c := range cases {
		if got := parseChatAvatar([]byte(c[0])); got != c[1] {
			t.Errorf("%s: want %q, got %q", name, c[1], got)
		}
	}
}

func TestParseUserAvatar(t *testing.T) {
	// search/v1/user 响应：重名靠 open_id 精确匹配（孙七 vs 孙七七）
	multi := `{"ok":true,"data":{"users":[
		{"name":"孙七七","open_id":"ou_other","avatar":{"avatar_240":"https://cdn/other240.png"}},
		{"name":"孙七","open_id":"ou_peer","avatar":{"avatar_240":"https://cdn/peer240.png","avatar_72":"https://cdn/peer72.png"}}]}}`
	cases := map[string][2]string{
		"open_id 匹配": {multi, "https://cdn/peer240.png"},
		"无匹配":        {`{"ok":true,"data":{"users":[{"open_id":"ou_other","avatar":{"avatar_240":"https://cdn/o.png"}}]}}`, ""},
		"空结果":        {`{"ok":true,"data":{"users":[]}}`, ""},
		"非 JSON":     {`err`, ""},
	}
	for name, c := range cases {
		if got := parseUserAvatar([]byte(c[0]), "ou_peer"); got != c[1] {
			t.Errorf("%s: want %q, got %q", name, c[1], got)
		}
	}
}

func TestReplyArgs(t *testing.T) {
	text := strings.Join(replyArgs("om_1", "草稿", "text", "om_1"), " ")
	if !strings.Contains(text, "--text 草稿") || strings.Contains(text, "--markdown") {
		t.Fatalf("text format: %s", text)
	}
	md := strings.Join(replyArgs("om_1", "```go\nx\n```", "markdown", "om_1"), " ")
	if !strings.Contains(md, "--markdown") || strings.Contains(md, "--text") {
		t.Fatalf("markdown format: %s", md)
	}
	for _, argv := range [][]string{replyArgs("om_1", "d", "text", "om_1"), replyArgs("om_1", "d", "markdown", "om_1")} {
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "--message-id om_1") || !strings.Contains(joined, "--idempotency-key om_1") ||
			!strings.Contains(joined, "--as user") {
			t.Fatalf("common args missing: %s", joined)
		}
	}
	// 快捷回复走独立幂等键（与「发送」的 mid 键分离）
	keyed := strings.Join(replyArgs("om_1", "收到", "text", "om_1-q-abcd1234"), " ")
	if !strings.Contains(keyed, "--idempotency-key om_1-q-abcd1234") {
		t.Fatalf("keyed args: %s", keyed)
	}
}

// run 的三个出口（成功 / exec 失败 / ok=false 信封）都在 cmd.exec 留痕。
func TestRunLogsCmdExec(t *testing.T) {
	logs := captureEvlog(t)

	if _, err := (&ExecLarkCLI{Bin: "echo"}).run("hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := (&ExecLarkCLI{Bin: "false"}).run("im", "x"); err == nil {
		t.Fatal("false: want error")
	}
	if _, err := (&ExecLarkCLI{Bin: "echo"}).run(`{"ok":false}`); err == nil {
		t.Fatal("ok=false envelope: want error")
	}

	recs := findLogs(logs(), "cmd.exec")
	if len(recs) != 3 {
		t.Fatalf("want 3 cmd.exec records, got %d: %v", len(recs), recs)
	}
	if r := recs[0]; r["level"] != "DEBUG" || r["bin"] != "echo" || r["out_bytes"].(float64) <= 0 || r["ms"] == nil {
		t.Errorf("success record: %v", r)
	}
	for _, r := range recs[1:] {
		if r["level"] != "ERROR" || r["err"] == nil {
			t.Errorf("failure record: %v", r)
		}
	}
}

func TestIsRestrictedModeError(t *testing.T) {
	// 取自真实 231203 响应：群开启防泄密模式，API 禁止读取消息
	envelope := &ExecError{Args: []string{"im", "+chat-messages-list"},
		Stderr: `{"ok":false,"code":231203,"msg":"The chat type is not supported, ext=Chat open Restricted Mode, don't allow copying or forwarding messages"}`,
		Err:    fmt.Errorf("ok=false")}
	if !IsRestrictedModeError(envelope) {
		t.Fatal("231203 envelope: want true")
	}
	if !IsRestrictedModeError(errors.New("ext=Chat open Restricted Mode")) {
		t.Fatal("text marker: want true")
	}
	if IsRestrictedModeError(errors.New("token expired")) {
		t.Fatal("unrelated error: want false")
	}
	if IsRestrictedModeError(nil) {
		t.Fatal("nil: want false")
	}
}

func TestAuthAlertMsg(t *testing.T) {
	notFound := &ExecError{Args: []string{"auth", "status"},
		Err: &exec.Error{Name: "lark-cli", Err: exec.ErrNotFound}}
	if msg := authAlertMsg(notFound); !strings.Contains(msg, "未安装") {
		t.Fatalf("not-found: want install guidance, got %q", msg)
	}
	if msg := authAlertMsg(errors.New("token expired")); !strings.Contains(msg, "auth login") {
		t.Fatalf("auth error: want login guidance, got %q", msg)
	}
}

func TestAuthExpiringMsg(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if msg := authExpiringMsg(now.Add(25*time.Hour), now); msg != "" {
		t.Fatalf("25h left: want empty, got %q", msg)
	}
	if msg := authExpiringMsg(now.Add(23*time.Hour), now); !strings.Contains(msg, "auth login") {
		t.Fatalf("23h left: want reminder, got %q", msg)
	}
	if msg := authExpiringMsg(now.Add(-time.Hour), now); msg == "" {
		t.Fatal("expired: want reminder, got empty")
	}
	// 字段缺失（零值）视为未知，不告警
	if msg := authExpiringMsg(time.Time{}, now); msg != "" {
		t.Fatalf("zero time: want empty, got %q", msg)
	}
}
