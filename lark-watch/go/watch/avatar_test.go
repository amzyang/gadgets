package watch

import "testing"

func newTestResolver(t *testing.T, cli LarkCLI, now *int64) *avatarResolver {
	t.Helper()
	return &avatarResolver{CLI: cli, Store: openTestStore(t), Now: func() int64 { return *now }}
}

func TestAvatarResolveByChatType(t *testing.T) {
	now := int64(1000)
	f := &fakeCLI{chatAvatarURL: "https://cdn/g.jpg", userAvatarURL: "https://cdn/u.png"}
	r := newTestResolver(t, f, &now)

	group := Message{Cid: "oc_g", Ctype: "group", Fid: "ou_x"}
	if got := r.Resolve([]Message{group}); got != "https://cdn/g.jpg" {
		t.Fatalf("group: want 群头像, got %q", got)
	}
	if !f.hasCall("chat-avatar oc_g") || f.hasCall("user-avatar") {
		t.Fatalf("group 应走 ChatAvatar(cid): %v", f.calls)
	}

	from := "陈珊"
	p2p := Message{Cid: "oc_p", Ctype: "p2p", Fid: "ou_peer", From: &from}
	if got := r.Resolve([]Message{p2p}); got != "https://cdn/u.png" {
		t.Fatalf("p2p: want 对方头像, got %q", got)
	}
	if !f.hasCall("user-avatar ou_peer 陈珊") {
		t.Fatalf("p2p 应走 UserAvatar(fid, from): %v", f.calls)
	}

	// 批次取首条（与 batchNotifyEnv 一致）
	f.calls = nil
	batch := []Message{{Cid: "oc_first", Ctype: "group"}, {Cid: "oc_second", Ctype: "group"}}
	r.Resolve(batch)
	if !f.hasCall("chat-avatar oc_first") || f.hasCall("oc_second") {
		t.Fatalf("批次应取首条: %v", f.calls)
	}

	// key 空：零调用返回空
	f.calls = nil
	if got := r.Resolve([]Message{{Ctype: "p2p"}}); got != "" || len(f.calls) != 0 {
		t.Fatalf("空 key: got %q calls=%v", got, f.calls)
	}
}

func TestAvatarResolveCacheTTL(t *testing.T) {
	now := int64(1000)
	f := &fakeCLI{chatAvatarURL: "https://cdn/g.jpg"}
	r := newTestResolver(t, f, &now)
	m := Message{Cid: "oc_g", Ctype: "group"}

	r.Resolve([]Message{m})
	f.calls = nil
	// TTL 内命中缓存，零 CLI 调用
	now += avatarTTL - 1
	if got := r.Resolve([]Message{m}); got != "https://cdn/g.jpg" || len(f.calls) != 0 {
		t.Fatalf("cache hit: got %q calls=%v", got, f.calls)
	}
	// 过期重拉
	now += 2
	f.chatAvatarURL = "https://cdn/g2.jpg"
	if got := r.Resolve([]Message{m}); got != "https://cdn/g2.jpg" || !f.hasCall("chat-avatar oc_g") {
		t.Fatalf("expired refetch: got %q calls=%v", got, f.calls)
	}
}

func TestAvatarResolveNegativeCache(t *testing.T) {
	now := int64(1000)
	f := &fakeCLI{failAvatar: true}
	r := newTestResolver(t, f, &now)
	m := Message{Cid: "oc_g", Ctype: "group"}

	if got := r.Resolve([]Message{m}); got != "" {
		t.Fatalf("fetch 失败应返回空: %q", got)
	}
	f.calls = nil
	// 负缓存 TTL 内不重试
	now += avatarNegTTL - 1
	if got := r.Resolve([]Message{m}); got != "" || len(f.calls) != 0 {
		t.Fatalf("neg cache: got %q calls=%v", got, f.calls)
	}
	// 过负缓存 TTL 后重试成功
	now += 2
	f.failAvatar, f.chatAvatarURL = false, "https://cdn/g.jpg"
	if got := r.Resolve([]Message{m}); got != "https://cdn/g.jpg" {
		t.Fatalf("neg cache 过期应重试: %q", got)
	}
}
