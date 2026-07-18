package watch

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// authFake 可编程 AuthSelf 返回（buildStatus 的 auth 分支注入）。
type authFake struct {
	fakeCLI
	info AuthInfo
	err  error
}

func (f *authFake) AuthSelf() (AuthInfo, error) { return f.info, f.err }

func TestBuildStatusAuthOK(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_700_000_000, 0)
	cli := &authFake{info: AuthInfo{OpenID: "ou_x", RefreshExpiresAt: now.Add(48 * time.Hour)}}

	st := buildStatus(s, cli, now)
	if !st.AuthOK {
		t.Fatal("want auth_ok true")
	}
	if st.AuthRefreshExpiresIn != 48*3600 {
		t.Fatalf("auth_refresh_expires_in_secs: want %d, got %d", 48*3600, st.AuthRefreshExpiresIn)
	}
	if st.AuthWarning != "" {
		t.Fatalf("want no warning, got %q", st.AuthWarning)
	}
}

func TestBuildStatusAuthExpiring(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_700_000_000, 0)
	cli := &authFake{info: AuthInfo{OpenID: "ou_x", RefreshExpiresAt: now.Add(2 * time.Hour)}}

	st := buildStatus(s, cli, now)
	if !st.AuthOK {
		t.Fatal("want auth_ok true")
	}
	if !strings.Contains(st.AuthWarning, "auth login") {
		t.Fatalf("want expiring warning, got %q", st.AuthWarning)
	}
}

func TestBuildStatusRestricted(t *testing.T) {
	s := openTestStore(t)
	s.RestrictedSet("oc_r", "产品技术部", 1000)

	st := buildStatus(s, &authFake{info: AuthInfo{OpenID: "ou_x"}}, time.Unix(2000, 0))
	if len(st.RestrictedChats) != 1 {
		t.Fatalf("restricted chats: %+v", st.RestrictedChats)
	}
	rc := st.RestrictedChats[0]
	if rc.Cid != "oc_r" || rc.Name != "产品技术部" || rc.Since != 1000 {
		t.Fatalf("restricted chat fields: %+v", rc)
	}
}

func TestBuildStatusAuthFailed(t *testing.T) {
	s := openTestStore(t)
	cli := &authFake{err: errors.New("token expired")}

	st := buildStatus(s, cli, time.Unix(1_700_000_000, 0))
	if st.AuthOK {
		t.Fatal("want auth_ok false")
	}
	if !strings.Contains(st.AuthWarning, "auth login") {
		t.Fatalf("want login guidance, got %q", st.AuthWarning)
	}
	if st.AuthRefreshExpiresIn != 0 {
		t.Fatalf("want zero expires_in, got %d", st.AuthRefreshExpiresIn)
	}
}
