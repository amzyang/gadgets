package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// eventJSON 还原一次 emit 的完整事件 JSON（带预取内容的事件常超单行宽度，
// 被 emitLines 拆成多行 chunk 分片，按 seq 序拼接 data 还原）。
func eventJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var full strings.Builder
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var c Chunk
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatalf("parse emitted line %q: %v", line, err)
		}
		if c.P != "chunk" {
			return string(line)
		}
		full.WriteString(c.Data)
	}
	return full.String()
}

func TestDetectResources(t *testing.T) {
	wiki := "https://gaotuedu.feishu.cn/wiki/AbCdWikiTok1"
	docx := "https://x.feishu.cn/docx/DocxTokA123"
	cases := []struct {
		name, msgType, content string
		want                   []Resource // 只比 Kind/Ref/Name
	}{
		{"doc-bare", "text", "看下这个 " + wiki + " 有空评下",
			[]Resource{{Kind: "doc", Ref: wiki}}},
		{"doc-markdown-link", "post", "[方案](" + docx + ") 求评",
			[]Resource{{Kind: "doc", Ref: docx}}},
		{"doc-anchor-query", "text", docx + "?from=chat#share-XyZ9 看第二节",
			[]Resource{{Kind: "doc", Ref: docx + "?from=chat#share-XyZ9"}}},
		{"doc-in-card-json", "interactive", `{"title":"周报","url":"https://t.larksuite.com/wiki/IntlTok9"}`,
			[]Resource{{Kind: "doc", Ref: "https://t.larksuite.com/wiki/IntlTok9"}}},
		{"non-doc-url-ignored", "text", "https://gaotuedu.feishu.cn/minutes/obcn1234 和 https://example.com/docx/x",
			nil},
		{"image-bracket", "image", "[Image: img_v3_abc0g]",
			[]Resource{{Kind: "image", Ref: "img_v3_abc0g"}}},
		{"image-markdown-inline", "post", "看 ![Image](img_v3_xyz1g) 对吗",
			[]Resource{{Kind: "image", Ref: "img_v3_xyz1g"}}},
		{"file-with-name", "file", `<file key="file_v3_k1" name="报价.pdf"/>`,
			[]Resource{{Kind: "file", Ref: "file_v3_k1", Name: "报价.pdf"}}},
		{"file-without-name", "file", `<file key="file_v3_k2"/>`,
			[]Resource{{Kind: "file", Ref: "file_v3_k2"}}},
		{"image-bracket-traversal-rejected", "text", "[Image: ../../../../../../tmp/evil]", nil},
		{"image-markdown-traversal-rejected", "post", "看 ![Image](../../etc/passwd) 对吗", nil},
		{"file-traversal-rejected", "file", `<file key="../../x" name="报价.pdf"/>`, nil},
		{"image-non-key-shape-rejected", "text", "[Image: not a key]", nil},
		{"post-mixed", "post", "方案 " + docx + " 附图 ![Image](img_v3_m1g) 和 <file key=\"file_v3_m2\" name=\"数据.csv\"/>",
			[]Resource{{Kind: "doc", Ref: docx}, {Kind: "image", Ref: "img_v3_m1g"}, {Kind: "file", Ref: "file_v3_m2", Name: "数据.csv"}}},
		{"media-skipped", "media", `<file key="file_v3_v" name="演示.mp4"/>`, nil},
		{"audio-skipped", "audio", `<file key="file_v3_a"/>`, nil},
		{"sticker-skipped", "sticker", "[Sticker]", nil},
		{"dedup", "text", wiki + " 再看一遍 " + wiki,
			[]Resource{{Kind: "doc", Ref: wiki}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectResources("om_x", tc.msgType, tc.content)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d resources %+v, want %d", len(got), got, len(tc.want))
			}
			for i, w := range tc.want {
				if got[i].Kind != w.Kind || got[i].Ref != w.Ref || got[i].Name != w.Name {
					t.Errorf("[%d] got %+v, want %+v", i, got[i], w)
				}
				if got[i].Mid != "om_x" {
					t.Errorf("[%d] mid not filled: %+v", i, got[i])
				}
			}
		})
	}
}

func TestDetectResourcesCap(t *testing.T) {
	var b strings.Builder
	for _, tok := range []string{"Tok1", "Tok2", "Tok3", "Tok4", "Tok5", "Tok6", "Tok7"} {
		b.WriteString("https://x.feishu.cn/docx/" + tok + " ")
	}
	if got := DetectResources("om_x", "text", b.String()); len(got) != detectMaxPerMessage {
		t.Errorf("got %d, want cap %d", len(got), detectMaxPerMessage)
	}
}

// 检测挂在截断前的全量 content 上：URL 在 500 码点之后仍能检出。
func TestToMessageDetectsBeyondTruncation(t *testing.T) {
	url := "https://x.feishu.cn/wiki/TailTok9"
	m := toMessage(rawMessage{
		MessageID: "om_long", ChatID: "oc_a", MsgType: "text",
		Content: strings.Repeat("长", 600) + " " + url,
		Sender:  rawSender{ID: "ou_alice", Name: "张三", SenderType: "user"},
	})
	if len([]rune(m.Text)) != 500 {
		t.Fatalf("text should be truncated to 500 runes, got %d", len([]rune(m.Text)))
	}
	if len(m.Resources) != 1 || m.Resources[0].Ref != url || m.Resources[0].Mid != "om_long" {
		t.Errorf("resources: %+v, want doc %s", m.Resources, url)
	}
}

func TestMergeResources(t *testing.T) {
	groups := [][]Resource{
		{{Kind: "doc", Ref: "u1"}, {Kind: "image", Ref: "k1"}},
		{{Kind: "doc", Ref: "u2"}},
		{{Kind: "doc", Ref: "u1"}, {Kind: "file", Ref: "k2"}, {Kind: "image", Ref: "k3"}},
	}
	got := mergeResources(groups, 3)
	// 最新消息优先（倒序），(kind,ref) 去重，截 3
	want := []string{"doc:u1", "file:k2", "image:k3"}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %v", got, want)
	}
	for i, w := range want {
		if got[i].Kind+":"+got[i].Ref != w {
			t.Errorf("[%d] got %s:%s, want %s", i, got[i].Kind, got[i].Ref, w)
		}
	}
	if r := mergeResources(nil, 3); r != nil {
		t.Errorf("empty input should stay nil, got %+v", r)
	}
}

// 聚合组的事件顶层资源 = 组内各条消息资源合并（最新优先、去重、截 3）；
// 「链接一条 + @我一条」两连发时链接条的文档不能丢。
func TestGroupP0MergeResources(t *testing.T) {
	doc := Resource{Kind: "doc", Ref: "https://x.feishu.cn/wiki/GTok1", Mid: "om_1"}
	img := Resource{Kind: "image", Ref: "img_v3_g2", Mid: "om_2"}
	events := GroupP0([]Message{
		{Mid: "om_1", Cid: "oc_a", T: "2026-07-24 10:00", Resources: []Resource{doc}},
		{Mid: "om_2", Cid: "oc_a", T: "2026-07-24 10:01", Resources: []Resource{img}},
	})
	if len(events) != 1 || events[0].N != 2 {
		t.Fatalf("want 1 aggregated event, got %+v", events)
	}
	rs := events[0].Resources
	if len(rs) != 2 || rs[0].Ref != img.Ref || rs[1].Ref != doc.Ref {
		t.Errorf("resources: %+v, want [image img_v3_g2, doc GTok1]", rs)
	}
	// 单条保持原形状，资源截 p0MaxResources
	single := GroupP0([]Message{{Mid: "om_s", Cid: "oc_b", T: "2026-07-24 10:02",
		Resources: []Resource{doc, img, {Kind: "file", Ref: "k3"}, {Kind: "file", Ref: "k4"}}}})
	if len(single) != 1 || len(single[0].Resources) != 3 {
		t.Errorf("single event resources should cap at 3, got %+v", single[0].Resources)
	}
}

// digest 会话条目携带 peek（最新一条）消息的资源，截 digestResPerChat。
func TestBuildDigestResources(t *testing.T) {
	chat := "踏踏实实的 AIGC"
	from := "严萍"
	old := Message{Cid: "oc_x", Chat: &chat, From: &from, T: "2026-07-24 09:51", Type: "text",
		Text: "先看这个", Resources: []Resource{{Kind: "doc", Ref: "u_old"}}}
	latest := Message{Cid: "oc_x", Chat: &chat, From: &from, T: "2026-07-24 09:53", Type: "text",
		Text: "https://gaotuedu.feishu.cn/wiki/WkTok1",
		Resources: []Resource{
			{Kind: "doc", Ref: "https://gaotuedu.feishu.cn/wiki/WkTok1"},
			{Kind: "image", Ref: "img_v3_d1"},
			{Kind: "file", Ref: "file_v3_d2"},
		}}
	d := BuildDigest([]Message{old, latest})
	if len(d.Chats) != 1 {
		t.Fatalf("want 1 chat, got %+v", d.Chats)
	}
	rs := d.Chats[0].Resources
	if len(rs) != 2 || rs[0].Ref != "https://gaotuedu.feishu.cn/wiki/WkTok1" || rs[1].Ref != "img_v3_d1" {
		t.Errorf("digest resources should be latest's first 2, got %+v", rs)
	}
	// 无资源会话不带字段（omitempty，golden 字节稳定）
	plain := BuildDigest([]Message{{Cid: "oc_y", Chat: &chat, From: &from, T: "2026-07-24 10:00", Text: "早"}})
	if plain.Chats[0].Resources != nil {
		t.Errorf("plain chat should have nil resources, got %+v", plain.Chats[0].Resources)
	}
}

func TestParseDocsFetch(t *testing.T) {
	out := []byte(`{"ok":true,"identity":"user","data":{"document":{"document_id":"dox1","revision_id":12,"content":"# 技术方案\n\n## 背景\n正文"}}}`)
	title, content, err := parseDocsFetch(out)
	if err != nil || title != "技术方案" || !strings.Contains(content, "## 背景") {
		t.Errorf("got title=%q content=%q err=%v", title, content, err)
	}
	if _, _, err := parseDocsFetch([]byte("not json")); err == nil {
		t.Error("bad json should error")
	}
	// 首行非标题：title 为空，content 原样
	title, content, err = parseDocsFetch([]byte(`{"ok":true,"data":{"document":{"content":"纯文本开头"}}}`))
	if err != nil || title != "" || content != "纯文本开头" {
		t.Errorf("got title=%q content=%q err=%v", title, content, err)
	}
}

func TestResCache(t *testing.T) {
	s := openTestStore(t)
	if _, ok := s.ResCacheGet("doc:tok1"); ok {
		t.Fatal("empty cache should miss")
	}
	e := ResCacheEntry{Ref: "doc:tok1", Kind: "doc", Name: "方案", Path: "/tmp/x.md", FetchedAt: 100}
	if err := s.ResCachePut(e); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.ResCacheGet("doc:tok1"); !ok || got != e {
		t.Errorf("got %+v ok=%v, want %+v", got, ok, e)
	}
	e.FetchedAt = 200
	s.ResCachePut(e)
	if got, _ := s.ResCacheGet("doc:tok1"); got.FetchedAt != 200 {
		t.Errorf("upsert failed: %+v", got)
	}
	s.ResCachePut(ResCacheEntry{Ref: "image:k1", Kind: "image", Path: "/tmp/i.png", FetchedAt: 500})
	if err := s.ResCacheSweep(300); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ResCacheGet("doc:tok1"); ok {
		t.Error("swept entry should miss")
	}
	if _, ok := s.ResCacheGet("image:k1"); !ok {
		t.Error("fresh entry should survive")
	}
}

func newTestPrefetcher(t *testing.T, cli LarkCLI) *Prefetcher {
	t.Helper()
	return &Prefetcher{CLI: cli, Store: openTestStore(t), SpoolDir: t.TempDir(),
		Now: func() int64 { return 1000 }}
}

func TestPrefetcherFetchDoc(t *testing.T) {
	f := &fakeCLI{}
	pf := newTestPrefetcher(t, f)
	rs := []Resource{{Kind: "doc", Ref: "https://x.feishu.cn/wiki/WTok1?from=x#share-abc", Mid: "om_1"}}
	pf.Fetch(context.Background(), rs, 2000)
	r := rs[0]
	if r.Err != "" || r.Name != "测试文档" || !strings.Contains(r.Content, "正文内容") || r.Path == "" {
		t.Fatalf("doc not filled: %+v", r)
	}
	if b, err := os.ReadFile(r.Path); err != nil || !strings.Contains(string(b), "正文内容") {
		t.Errorf("spool file: %v %q", err, b)
	}
	// 锚点/查询串归一：fetch 用干净 URL（全文获取，缓存对锚点不敏感）
	if !f.hasCall("docs-fetch https://x.feishu.cn/wiki/WTok1") || f.hasCall("#share") {
		t.Errorf("fetch ref not normalized: %v", f.calls)
	}
	// 缓存命中：同 token 再来（不同锚点）不调 CLI
	rs2 := []Resource{{Kind: "doc", Ref: "https://x.feishu.cn/wiki/WTok1#share-zzz", Mid: "om_2"}}
	calls := len(f.calls)
	pf.Fetch(context.Background(), rs2, 2000)
	if len(f.calls) != calls {
		t.Errorf("cache hit should not call CLI: %v", f.calls[calls:])
	}
	if rs2[0].Name != "测试文档" || !strings.Contains(rs2[0].Content, "正文内容") || rs2[0].Path != r.Path {
		t.Errorf("cache hit not filled: %+v", rs2[0])
	}
}

func TestPrefetcherDocTTLAndLostFile(t *testing.T) {
	f := &fakeCLI{}
	pf := newTestPrefetcher(t, f)
	now := int64(1000)
	pf.Now = func() int64 { return now }
	ref := "https://x.feishu.cn/docx/DTok1"
	fetch := func() Resource {
		rs := []Resource{{Kind: "doc", Ref: ref, Mid: "om_1"}}
		pf.Fetch(context.Background(), rs, 2000)
		return rs[0]
	}
	first := fetch()
	calls := len(f.calls)
	// TTL 内命中
	now += docCacheTTL - 10
	fetch()
	if len(f.calls) != calls {
		t.Errorf("within TTL should hit cache: %v", f.calls[calls:])
	}
	// TTL 过期重取
	now += 20
	fetch()
	if len(f.calls) != calls+1 {
		t.Errorf("expired TTL should refetch: %v", f.calls[calls:])
	}
	// 产物文件丢失视为 miss 重取
	calls = len(f.calls)
	os.Remove(first.Path)
	fetch()
	if len(f.calls) != calls+1 {
		t.Errorf("lost spool file should refetch: %v", f.calls[calls:])
	}
}

func TestPrefetcherInlineTruncation(t *testing.T) {
	long := "# 长文\n" + strings.Repeat("字", 3000)
	f := &fakeCLI{docsFetch: func(ctx context.Context, ref string) ([]byte, error) {
		return EncodeLine(map[string]any{"ok": true, "data": map[string]any{"document": map[string]any{"content": long}}}), nil
	}}
	pf := newTestPrefetcher(t, f)
	rs := []Resource{{Kind: "doc", Ref: "https://x.feishu.cn/docx/LTok1", Mid: "om_1"}}
	pf.Fetch(context.Background(), rs, 100)
	if n := len([]rune(rs[0].Content)); n != 100 {
		t.Errorf("inline should truncate to 100 runes, got %d", n)
	}
	if b, _ := os.ReadFile(rs[0].Path); len([]rune(string(b))) != len([]rune(long)) {
		t.Errorf("spool file should keep full content, got %d runes", len([]rune(string(b))))
	}
}

func TestPrefetcherDownload(t *testing.T) {
	f := &fakeCLI{downloadName: "截图.png"}
	pf := newTestPrefetcher(t, f)
	rs := []Resource{{Kind: "image", Ref: "img_v3_k1", Mid: "om_1"}}
	pf.Fetch(context.Background(), rs, 2000)
	if rs[0].Err != "" || rs[0].Path == "" || rs[0].Name != "截图.png" {
		t.Fatalf("image not filled: %+v", rs[0])
	}
	if _, err := os.Stat(rs[0].Path); err != nil {
		t.Fatal(err)
	}
	// 缓存命中（file_key 内容不变，产物存在即命中）：不再下载
	rs2 := []Resource{{Kind: "image", Ref: "img_v3_k1", Mid: "om_9"}}
	calls := len(f.calls)
	pf.Fetch(context.Background(), rs2, 2000)
	if len(f.calls) != calls || rs2[0].Path != rs[0].Path {
		t.Errorf("cache hit: calls=%v path=%q", f.calls[calls:], rs2[0].Path)
	}
}

func TestPrefetcherFileWhitelist(t *testing.T) {
	f := &fakeCLI{}
	pf := newTestPrefetcher(t, f)
	rs := []Resource{{Kind: "file", Ref: "file_v3_z", Name: "安装包.dmg", Mid: "om_1"}}
	pf.Fetch(context.Background(), rs, 2000)
	if rs[0].Err == "" || rs[0].Path != "" || f.hasCall("download") {
		t.Errorf("non-consumable file should skip download: %+v calls=%v", rs[0], f.calls)
	}
	// 白名单内正常下载
	f2 := &fakeCLI{downloadName: "报价.pdf"}
	pf2 := newTestPrefetcher(t, f2)
	rs2 := []Resource{{Kind: "file", Ref: "file_v3_p", Name: "报价.pdf", Mid: "om_2"}}
	pf2.Fetch(context.Background(), rs2, 2000)
	if rs2[0].Err != "" || rs2[0].Path == "" {
		t.Errorf("pdf should download: %+v", rs2[0])
	}
}

// 一切预取失败（含 auth 类）吞入 Resource.Err，绝不让 IsAuthError 冒泡杀 poller。
func TestPrefetcherErrSwallowed(t *testing.T) {
	f := &fakeCLI{failDownload: true,
		docsFetch: func(ctx context.Context, ref string) ([]byte, error) {
			return nil, fmt.Errorf("NeedUserAuthorization: docs scope missing")
		}}
	pf := newTestPrefetcher(t, f)
	rs := []Resource{
		{Kind: "doc", Ref: "https://x.feishu.cn/docx/ETok", Mid: "om_1"},
		{Kind: "file", Ref: "file_v3_e", Name: "a.pdf", Mid: "om_1"},
	}
	pf.Fetch(context.Background(), rs, 2000)
	for i, r := range rs {
		if r.Err == "" || r.Content != "" || r.Path != "" {
			t.Errorf("[%d] err should be set without content: %+v", i, r)
		}
	}
}

// 预算 ctx 已取消/耗尽：剩余资源短路标 err，不调 CLI。
func TestPrefetcherBudgetCancelled(t *testing.T) {
	f := &fakeCLI{}
	pf := newTestPrefetcher(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rs := []Resource{{Kind: "doc", Ref: "https://x.feishu.cn/docx/BTok", Mid: "om_1"}}
	pf.Fetch(ctx, rs, 2000)
	if rs[0].Err == "" || f.hasCall("docs-fetch") {
		t.Errorf("cancelled ctx should short-circuit: %+v calls=%v", rs[0], f.calls)
	}
}

func TestPrefetcherSweep(t *testing.T) {
	pf := newTestPrefetcher(t, &fakeCLI{})
	docs := filepath.Join(pf.SpoolDir, "docs")
	os.MkdirAll(docs, 0o755)
	oldFile := filepath.Join(docs, "old.md")
	os.WriteFile(oldFile, []byte("x"), 0o644)
	oldTime := time.Unix(1000-spoolMaxAge-10, 0)
	os.Chtimes(oldFile, oldTime, oldTime)
	newFile := filepath.Join(docs, "new.md")
	os.WriteFile(newFile, []byte("y"), 0o644)
	oldDir := filepath.Join(pf.SpoolDir, "res", "om_old")
	os.MkdirAll(oldDir, 0o755)
	os.Chtimes(oldDir, oldTime, oldTime)
	pf.Store.ResCachePut(ResCacheEntry{Ref: "doc:old", Kind: "doc", Path: oldFile, FetchedAt: 1000 - spoolMaxAge - 10})
	pf.Store.ResCachePut(ResCacheEntry{Ref: "doc:new", Kind: "doc", Path: newFile, FetchedAt: 999})

	pf.Sweep(1000)

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("old doc file should be removed")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Error("new doc file should survive")
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Error("old res dir should be removed")
	}
	if _, ok := pf.Store.ResCacheGet("doc:old"); ok {
		t.Error("old cache row should be swept")
	}
	if _, ok := pf.Store.ResCacheGet("doc:new"); !ok {
		t.Error("new cache row should survive")
	}
}

// P0 事件发射前资源已预取（listFake 内嵌 fakeCLI 的 DocsFetch 默认成功响应）。
func TestTickPrefetch(t *testing.T) {
	url := "https://x.feishu.cn/wiki/PTok1"
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "群A", Mode: "group"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "@周八 看下 "+url, "2026-07-17 12:01", "ou_SELF"),
		)},
	}
	p, events := newTestPoller(t, f, 2000)
	p.Prefetch = &Prefetcher{CLI: f, Store: p.Store, SpoolDir: t.TempDir(), Now: func() int64 { return 2000 }}
	p.Store.SetFetchCursor("oc_a", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 {
		t.Fatalf("want 1 event, got %d", len(*events))
	}
	line := eventJSON(t, (*events)[0])
	if !strings.Contains(line, `"resources":[`) || !strings.Contains(line, "# 测试文档") {
		t.Errorf("event should carry prefetched doc content: %s", line)
	}
}

// replied 事件不预取（模型安静跳过，预取纯浪费），事件带 refs 无内容。
func TestTickPrefetchSkipsReplied(t *testing.T) {
	url := "https://x.feishu.cn/wiki/RTok1"
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "群A", Mode: "group"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "@周八 看下 "+url, "2026-07-17 12:01", "ou_SELF"),
			rawMsgJSON("om_2", "ou_SELF", "周八", "收到", "2026-07-17 12:02"),
		)},
	}
	p, events := newTestPoller(t, f, 2000)
	p.Prefetch = &Prefetcher{CLI: f, Store: p.Store, SpoolDir: t.TempDir(), Now: func() int64 { return 2000 }}
	p.Store.SetFetchCursor("oc_a", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if f.hasCall("docs-fetch") {
		t.Errorf("replied event should not prefetch: %v", f.calls)
	}
	if len(*events) != 1 || !strings.Contains(eventJSON(t, (*events)[0]), `"replied":true`) {
		t.Fatalf("want replied event, got %q", *events)
	}
}

// 预取 auth 失败不杀 tick：事件带 err 照常发射，tick 返回 nil。
func TestTickPrefetchAuthErrorNotFatal(t *testing.T) {
	url := "https://x.feishu.cn/wiki/ATok1"
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "群A", Mode: "group"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "@周八 "+url, "2026-07-17 12:01", "ou_SELF"),
		)},
	}
	f.docsFetch = func(ctx context.Context, ref string) ([]byte, error) {
		return nil, fmt.Errorf("NeedUserAuthorization: run auth login")
	}
	p, events := newTestPoller(t, f, 2000)
	p.Prefetch = &Prefetcher{CLI: f, Store: p.Store, SpoolDir: t.TempDir(), Now: func() int64 { return 2000 }}
	p.Store.SetFetchCursor("oc_a", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatalf("prefetch failure must not fail tick: %v", err)
	}
	if len(*events) != 1 || !strings.Contains(eventJSON(t, (*events)[0]), `"err":"NeedUserAuthorization`) {
		t.Errorf("event should carry err and still emit: %q", *events)
	}
}

// digest flush 时对 peek 资源预取；关停路径（已取消 ctx）短路预取照常发射。
func TestFlushDigestPrefetch(t *testing.T) {
	url := "https://gaotuedu.feishu.cn/wiki/DTok9"
	chat, from := "踏踏实实的 AIGC", "严萍"
	mk := func(mid string) Message {
		return Message{Cid: "oc_x", Chat: &chat, From: &from, T: "2026-07-24 09:53", Type: "text",
			Text: url, Mid: mid, Resources: []Resource{{Kind: "doc", Ref: url, Mid: mid}}}
	}
	f := &listFake{}
	p, events := newTestPoller(t, f, 2000)
	p.Prefetch = &Prefetcher{CLI: f, Store: p.Store, SpoolDir: t.TempDir(), Now: func() int64 { return 2000 }}

	p.Store.DigestAppend([]Message{mk("om_d1")})
	p.flushDigest(context.Background())
	if len(*events) != 1 {
		t.Fatalf("want 1 digest, got %d", len(*events))
	}
	line := eventJSON(t, (*events)[0])
	if !strings.Contains(line, `"p":"digest"`) || !strings.Contains(line, "# 测试文档") {
		t.Errorf("digest should carry prefetched content: %s", line)
	}

	// 关停路径：预取短路（标 err）但摘要照常发出
	f.docsFetch = func(ctx context.Context, ref string) ([]byte, error) {
		t.Error("cancelled ctx must not reach CLI")
		return nil, nil
	}
	p.Store.DigestAppend([]Message{mk("om_d2")})
	p.flushDigest(closedCtx())
	if len(*events) != 2 || !strings.Contains(eventJSON(t, (*events)[1]), `"err":`) {
		t.Fatalf("shutdown flush should emit with err: %q", *events)
	}
}
