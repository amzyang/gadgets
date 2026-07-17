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
