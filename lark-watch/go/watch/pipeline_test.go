package watch

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "重写 golden fixtures")

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func parseNDJSON(t *testing.T, data []byte) []Message {
	t.Helper()
	var out []Message
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("parse line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func encodeAll(msgs []Message) []byte {
	var buf bytes.Buffer
	for _, m := range msgs {
		buf.Write(EncodeLine(m))
	}
	return buf.Bytes()
}

// assertGolden 把 got 与 testdata/<name> 字节比较；`go test -update` 时改为重写基线。
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(filepath.Join("testdata", name), got, 0o644); err != nil {
			t.Fatalf("update %s: %v", name, err)
		}
		return
	}
	want := readTestdata(t, name)
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTrim(t *testing.T) {
	msgs, hasMore, err := Trim(readTestdata(t, "raw-search-response.json"))
	if err != nil {
		t.Fatal(err)
	}
	if hasMore {
		t.Error("hasMore should be false")
	}
	assertGolden(t, "trim.ndjson", encodeAll(msgs))
}

func TestTrimChatMessages(t *testing.T) {
	raw := readTestdata(t, "raw-chat-messages-response.json")
	msgs, hasMore, err := TrimChatMessages(raw, "测试群A", "group")
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore {
		t.Error("hasMore should be true")
	}
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d", len(msgs))
	}
	// fixture 按 create_time 降序，Trim 后应升序
	for i := 1; i < len(msgs); i++ {
		if msgs[i-1].T > msgs[i].T {
			t.Errorf("not ascending: %s > %s", msgs[i-1].T, msgs[i].T)
		}
	}
	for _, m := range msgs {
		if m.Ctype != "group" || m.Chat == nil || *m.Chat != "测试群A" {
			t.Errorf("%s: chat meta not injected: ctype=%q chat=%v", m.Mid, m.Ctype, m.Chat)
		}
		if !strings.HasPrefix(m.Link, "lark://applink.feishu.cn/") {
			t.Errorf("%s: link not lark scheme: %s", m.Mid, m.Link)
		}
	}
	// mentions 样本：赵六 @王五
	byMid := map[string]Message{}
	for _, m := range msgs {
		byMid[m.Mid] = m
	}
	at := byMid["om_mention1"]
	if len(at.AtIDs) != 1 || at.AtIDs[0] != "ou_dave" {
		t.Errorf("mentions not extracted: %v", at.AtIDs)
	}
	if plain := byMid["om_plain1"]; len(plain.AtIDs) != 0 {
		t.Errorf("no-mention message should have empty AtIDs: %v", plain.AtIDs)
	}

	// p2p：Chat 置 null，Ctype 注入 p2p
	p2p, _, err := TrimChatMessages(raw, "王五", "p2p")
	if err != nil {
		t.Fatal(err)
	}
	if p2p[0].Ctype != "p2p" || p2p[0].Chat != nil {
		t.Errorf("p2p: ctype=%q chat=%v", p2p[0].Ctype, p2p[0].Chat)
	}
}

func TestClassify(t *testing.T) {
	input := parseNDJSON(t, readTestdata(t, "classify-input.ndjson"))
	dir := t.TempDir()
	cases := []struct {
		name      string
		watchlist string
		keywords  string
		ignore    string
		expected  string
	}{
		{name: "base", expected: "classify.ndjson"},
		{name: "watch-user", watchlist: "ou_alice\n", expected: "classify-watch-user.ndjson"},
		{name: "watch-chat", watchlist: "oc_group1\n", expected: "classify-watch-chat.ndjson"},
		{name: "watch-name", watchlist: "# 注释行应被忽略\n测试群\n", expected: "classify-watch-chat.ndjson"},
		{name: "keywords", keywords: "开会\n", expected: "classify-keywords.ndjson"},
		// ignore 可压掉 P0；坏正则 `([` 跳过不崩溃
		{name: "ignore", ignore: "帮我看个问题\n([\n吃什么\n", expected: "classify-ignore.ndjson"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wl, kw, ig string
			if tc.watchlist != "" {
				wl = writeConfig(t, dir, tc.name+".watchlist", tc.watchlist)
			}
			if tc.keywords != "" {
				kw = writeConfig(t, dir, tc.name+".keywords", tc.keywords)
			}
			if tc.ignore != "" {
				ig = writeConfig(t, dir, tc.name+".ignore", tc.ignore)
			}
			rules := LoadRules("ou_SELF", wl, kw, ig)
			kept, _ := rules.ClassifyAll(input)
			assertGolden(t, tc.expected, encodeAll(kept))
		})
	}
}

// 注释只认整行：行内 # 属内容（写入侧 ignore-add 按完整行校验，两侧解析一致），
// 行首 #（含前导空白）整行跳过。
func TestReadConfigLines(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "ignore", "# 整行注释\n  # 前导空白注释\n工单#\\d+ 自动通知\n\n收到\n")
	got := readConfigLines(path)
	want := []string{`工单#\d+ 自动通知`, "收到"}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func strPtr(s string) *string { return &s }

// Classify 判定理由全词表覆盖（诊断日志 msg.keep/msg.drop 的 reason 字段来源）。
func TestClassifyReason(t *testing.T) {
	dir := t.TempDir()
	wl := writeConfig(t, dir, "watchlist", "ou_vip\noc_vip\n王总\n")
	kw := writeConfig(t, dir, "keywords", "开会\n")
	ig := writeConfig(t, dir, "ignore", "吃什么\n")
	rules := LoadRules("ou_SELF", wl, kw, ig)

	base := Message{Mid: "om_r", Cid: "oc_x", Ctype: "group", Chat: strPtr("普通群"),
		From: strPtr("张三"), Fid: "ou_alice", Ftype: "user", Type: "text",
		Text: "随便聊聊", T: "2026-07-17 12:00"}
	mk := func(mut func(*Message)) Message { m := base; mut(&m); return m }

	cases := []struct {
		name   string
		msg    Message
		keep   bool
		reason string
	}{
		{"self", mk(func(m *Message) { m.Fid = "ou_SELF" }), false, "self"},
		{"non-user", mk(func(m *Message) { m.Ftype = "app" }), false, "non-user"},
		{"empty", mk(func(m *Message) { m.Text = "" }), false, "empty"},
		{"ignore", mk(func(m *Message) { m.Text = "中午吃什么" }), false, "ignore:吃什么"},
		{"vc", mk(func(m *Message) { m.Type = "video_chat"; m.Text = "" }), true, "vc"},
		{"p2p", mk(func(m *Message) { m.Ctype = "p2p"; m.Chat = nil }), true, "p2p"},
		{"at-me-mentions", mk(func(m *Message) { m.AtIDs = []string{"ou_bob", "ou_SELF"} }), true, "at-me"},
		{"at-me-content", mk(func(m *Message) { m.Text = `<at user_id="ou_SELF">周八</at> 看下` }), true, "at-me"},
		{"watch-user", mk(func(m *Message) { m.Fid = "ou_vip" }), true, "watch-user"},
		{"watch-chat", mk(func(m *Message) { m.Cid = "oc_vip" }), true, "watch-chat"},
		{"watch-name", mk(func(m *Message) { m.From = strPtr("王总") }), true, "watch-name"},
		{"keyword", mk(func(m *Message) { m.Text = "明天开会" }), true, "keyword:开会"},
		{"p1", base, true, "p1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, keep := rules.Classify(tc.msg)
			if keep != tc.keep || got.Reason != tc.reason {
				t.Errorf("keep=%v reason=%q, want keep=%v reason=%q",
					keep, got.Reason, tc.keep, tc.reason)
			}
		})
	}
}

// ClassifyAll 返回 dropped 切片（带丢弃 Reason），kept 打好 p 标签。
func TestClassifyAllDropped(t *testing.T) {
	rules := LoadRules("ou_SELF", "", "", "")
	kept, dropped := rules.ClassifyAll([]Message{
		{Mid: "om_1", Fid: "ou_SELF", Ftype: "user", Ctype: "p2p", Type: "text", Text: "自己发的"},
		{Mid: "om_2", Fid: "ou_alice", Ftype: "user", Ctype: "p2p", Type: "text", Text: "在吗"},
		{Mid: "om_3", Fid: "ou_bot", Ftype: "app", Ctype: "p2p", Type: "text", Text: "bot 消息"},
	})
	if len(kept) != 1 || kept[0].Mid != "om_2" || kept[0].P != "P0" || kept[0].Reason != "p2p" {
		t.Fatalf("kept: %+v", kept)
	}
	if len(dropped) != 2 || dropped[0].Reason != "self" || dropped[1].Reason != "non-user" {
		t.Fatalf("dropped: %+v", dropped)
	}
}

// @我判定：真实 API 的 content 是渲染文本（无 <at> 标记），@ 信息在 mentions 数组。
func TestClassifyAtIDs(t *testing.T) {
	rules := LoadRules("ou_SELF", "", "", "")
	base := Message{Mid: "om_at", Cid: "oc_group1", Ctype: "group", Chat: strPtr("测试群"),
		From: strPtr("张三"), Fid: "ou_alice", Ftype: "user", Type: "text",
		Text: "@周八 这个方案你看下", T: "2026-07-17 12:03"}

	atMe := base
	atMe.AtIDs = []string{"ou_bob", "ou_SELF"}
	if got, keep := rules.Classify(atMe); !keep || got.P != "P0" {
		t.Errorf("mentions 含 self 应升 P0: keep=%v p=%q", keep, got.P)
	}

	atOther := base
	atOther.AtIDs = []string{"ou_bob"}
	if got, keep := rules.Classify(atOther); !keep || got.P != "P1" {
		t.Errorf("mentions 不含 self 应为 P1: keep=%v p=%q", keep, got.P)
	}
}

func TestClassifyVC(t *testing.T) {
	vc := Message{Mid: "om_vc1", Cid: "oc_group1", Ctype: "group", Chat: strPtr("测试群"),
		From: strPtr("张三"), Fid: "ou_alice", Ftype: "user", Type: "video_chat",
		Link: "lark://applink.feishu.cn/client/chat/open?openChatId=oc_group1&position=20",
		T:    "2026-07-17 12:09"}
	meeting := vc
	meeting.Mid = "om_vc2"
	meeting.Type = "vc_meeting"

	rules := LoadRules("ou_SELF", "", "", "")
	for _, m := range []Message{vc, meeting} {
		got, keep := rules.Classify(m)
		if !keep || got.P != "P0" {
			t.Errorf("%s(%s): got keep=%v p=%q, want P0", m.Mid, m.Type, keep, got.P)
		}
	}

	// ignore 仍可压掉音视频会议
	dir := t.TempDir()
	ig := writeConfig(t, dir, "vc.ignore", "测试群\n")
	if _, keep := LoadRules("ou_SELF", "", "", ig).Classify(vc); keep {
		t.Error("ignore should drop vc message")
	}

	// 空文本豁免仅限音视频类型，普通空文本仍丢弃
	empty := vc
	empty.Type = "text"
	if _, keep := rules.Classify(empty); keep {
		t.Error("empty text message should be dropped")
	}
}

func TestBuildDigest(t *testing.T) {
	msgs := parseNDJSON(t, readTestdata(t, "digest-buf.ndjson"))
	assertGolden(t, "digest.json", EncodeLine(BuildDigest(msgs)))
}

func TestNormalizeMedia(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"image-bracket", "[Image: img_v3_0213q_1e9231e8-6665-4c0c-968b-ffd4ea45419g]", "[图片]"},
		{"image-markdown-inline", "看下这个 ![Image](img_v3_0213q_679c070a) 对吗", "看下这个 [图片] 对吗"},
		{"file-with-name", `<file key="file_v3_0013q_98b6" name="10726需手动发奖.csv"/>`, "[文件:10726需手动发奖.csv]"},
		{"file-without-name", `<file key="file_v3_x"/>`, "[文件]"},
		{"card-with-title", "<card title=\"督察合规讲堂\">\n🖼️ image", "[卡片:督察合规讲堂]"},
		{"card-without-title", "<card>\n伙伴好！欢迎来到你的专属服务台。", "[卡片]"},
		{"forwarded", "<forwarded_messages>\n[2026-07-21] 张三: 你好", "[合并转发]"},
		{"plain-text", "是添来", "是添来"},
	}
	for _, tc := range cases {
		if got := normalizeMedia(tc.in); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestBuildDigestMediaPeek(t *testing.T) {
	chat := "轻舟平台-唯一沟通群"
	from := "蔡雨顺"
	d := BuildDigest([]Message{{
		Cid: "oc_x", Chat: &chat, From: &from, T: "2026-07-21 14:59",
		Type: "image", Text: "[Image: img_v3_0213q_679c070a-67cb-4699-830f-0b8b46a2ag]",
	}})
	if got, want := d.Chats[0].Peek, "蔡雨顺: [图片]"; got != want {
		t.Errorf("peek: got %q want %q", got, want)
	}
}

func TestShouldFlush(t *testing.T) {
	cases := []struct {
		name       string
		count, max int
		last, now  int64
		window     int64
		want       bool
	}{
		{"empty-buffer", 0, 20, 0, 9999, 600, false},
		{"count-reached", 20, 20, 9990, 9999, 600, true},
		{"window-elapsed", 3, 20, 9000, 9600, 600, true},
		{"not-yet", 3, 20, 9500, 9600, 600, false},
	}
	for _, tc := range cases {
		if got := ShouldFlush(tc.count, tc.max, tc.last, tc.now, tc.window); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestCatchupGroup(t *testing.T) {
	msgs := parseNDJSON(t, readTestdata(t, "catchup-input.ndjson"))
	cursors := map[string]string{"oc_g1": "2026-07-17 12:04"}

	assertGolden(t, "catchup-group.json",
		EncodeLine(CatchupGroup(msgs, cursors, "2026-07-17 12:00", 5, false)))
	assertGolden(t, "catchup-group-peek1.json",
		EncodeLine(CatchupGroup(msgs, cursors, "2026-07-17 12:00", 1, true)))
}
