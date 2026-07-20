package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listFake 可编程 list 链路 fake：ChatList 返回预置会话，ChatMessages 按 cid 返回预置响应。
type listFake struct {
	fakeCLI
	chats       []ChatMeta
	msgs        map[string]string // cid → raw JSON 响应
	errs        map[string]error  // cid → ChatMessages 注入错误
	chatCalls   []string
	searchCalls int
}

func (f *listFake) ChatList() ([]ChatMeta, error) { return f.chats, nil }

func (f *listFake) ChatMessages(cid, start string) ([]byte, error) {
	f.chatCalls = append(f.chatCalls, cid)
	if err, ok := f.errs[cid]; ok {
		return nil, err
	}
	if r, ok := f.msgs[cid]; ok {
		return []byte(r), nil
	}
	return []byte(emptyMessagesResp), nil
}

func (f *listFake) Search(start, end string) ([]byte, error) {
	f.searchCalls++
	return []byte(emptyMessagesResp), nil
}

func rawMsgJSON(mid, fid, from, text, t string, mentions ...string) string {
	var mts []string
	for _, id := range mentions {
		mts = append(mts, fmt.Sprintf(`{"id":%q,"key":"@_user_1","name":"某人"}`, id))
	}
	return fmt.Sprintf(`{"message_id":%q,"chat_id":"oc_a","msg_type":"text","content":%q,
		"create_time":%q,"message_app_link":"https://applink.feishu.cn/client/chat/open?openChatId=oc_a",
		"mentions":[%s],
		"sender":{"id":%q,"id_type":"open_id","name":%q,"sender_type":"user"}}`,
		mid, text, t, strings.Join(mts, ","), fid, from)
}

func chatMsgsResp(hasMore bool, msgs ...string) string {
	return fmt.Sprintf(`{"ok":true,"data":{"has_more":%v,"messages":[%s]}}`,
		hasMore, strings.Join(msgs, ","))
}

func newTestPoller(t *testing.T, cli LarkCLI, now int64) (*Poller, *[][]byte) {
	t.Helper()
	s := openTestStore(t)
	var events [][]byte
	p := &Poller{
		Store: s, CLI: cli, Paths: Paths{ConfigDir: t.TempDir()},
		Interval: time.Second, DigestWindow: 600, DigestMax: 20,
		Out: func(line []byte) { events = append(events, append([]byte(nil), line...)) },
		Now: func() int64 { return now },
	}
	return p, &events
}

// 首 tick：所有会话懒初始化游标为 now，不拉取（不重放历史）。
func TestTickLazyInit(t *testing.T) {
	f := &listFake{chats: []ChatMeta{
		{Cid: "oc_a", Name: "群A", Mode: "group"},
		{Cid: "oc_p", Name: "张三", Mode: "p2p"},
	}}
	p, events := newTestPoller(t, f, 1000)
	if err := p.tick(context.Background(), 1000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(f.chatCalls) != 0 {
		t.Errorf("lazy init should not fetch, got calls: %v", f.chatCalls)
	}
	for _, cid := range []string{"oc_a", "oc_p"} {
		if ts, ok := p.Store.FetchCursor(cid); !ok || ts != 1000 {
			t.Errorf("%s cursor: %d %v, want 1000", cid, ts, ok)
		}
	}
	if len(*events) != 0 {
		t.Errorf("no events expected, got %d", len(*events))
	}
}

// 增量 tick：@我升 P0 即时 emit，普通群消息进 digest 缓冲；游标推进到 now。
func TestTickIncremental(t *testing.T) {
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "群A", Mode: "group"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "早", "2026-07-17 12:00"),
			rawMsgJSON("om_2", "ou_alice", "张三", "@邹洋 看下", "2026-07-17 12:01", "ou_SELF"),
		)},
	}
	p, events := newTestPoller(t, f, 2000)
	p.Store.SetFetchCursor("oc_a", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 || !strings.Contains(string((*events)[0]), `"p":"P0"`) ||
		!strings.Contains(string((*events)[0]), "om_2") {
		t.Errorf("want 1 P0 event for om_2, got: %q", *events)
	}
	if n := p.Store.DigestCount(); n != 1 {
		t.Errorf("digest buffer: want 1 (om_1), got %d", n)
	}
	if ts, _ := p.Store.FetchCursor("oc_a"); ts != 2000 {
		t.Errorf("cursor should advance to now, got %d", ts)
	}
}

// early-stop：连续 K 个「无新消息」的会话后停止遍历。
func TestTickEarlyStop(t *testing.T) {
	var chats []ChatMeta
	for i := 0; i < 20; i++ {
		chats = append(chats, ChatMeta{Cid: fmt.Sprintf("oc_%02d", i), Name: "群", Mode: "group"})
	}
	f := &listFake{chats: chats}
	p, _ := newTestPoller(t, f, 2000)
	for _, ch := range chats {
		p.Store.SetFetchCursor(ch.Cid, 1000)
	}
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if k := earlyStopK(); len(f.chatCalls) != k {
		t.Errorf("want %d fetches (early-stop), got %d: %v", k, len(f.chatCalls), f.chatCalls)
	}
}

// search 兜底：首 tick 跑一次，之后每 searchEveryN 个 tick 一次。
func TestTickSearchFallback(t *testing.T) {
	f := &listFake{}
	p, _ := newTestPoller(t, f, 2000)
	for i := 0; i < searchEveryN()+1; i++ {
		if err := p.tick(context.Background(), 2000+int64(i), "ou_SELF"); err != nil {
			t.Fatal(err)
		}
	}
	if f.searchCalls != 2 {
		t.Errorf("want 2 search calls (tick 0 and tick %d), got %d", searchEveryN(), f.searchCalls)
	}
}

// has_more：游标只推进到本批最后一条消息时间，下 tick 续拉。
func TestTickHasMore(t *testing.T) {
	lastT := "2026-07-17 12:05"
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "群A", Mode: "group"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(true,
			rawMsgJSON("om_1", "ou_alice", "张三", "刷屏1", "2026-07-17 12:04"),
			rawMsgJSON("om_2", "ou_alice", "张三", "刷屏2", lastT),
		)},
	}
	p, _ := newTestPoller(t, f, 9000000000)
	p.Store.SetFetchCursor("oc_a", 8999999000)
	if err := p.tick(context.Background(), 9000000000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	want := parseMinute(lastT)
	if ts, _ := p.Store.FetchCursor("oc_a"); ts != want {
		t.Errorf("has_more cursor: got %d, want %d (last msg time)", ts, want)
	}
}

func restrictedErr() error {
	return &ExecError{Args: []string{"im", "+chat-messages-list"},
		Stderr: `{"ok":false,"code":231203,"msg":"The chat type is not supported, ext=Chat open Restricted Mode, don't allow copying or forwarding messages"}`,
		Err:    fmt.Errorf("ok=false")}
}

// 防泄密群：首次检测发一次 alert 并持久标记，标记未过期的后续 tick 不再拉取。
func TestTickRestrictedMode(t *testing.T) {
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_r", Name: "产品技术部", Mode: "group"}},
		errs:  map[string]error{"oc_r": restrictedErr()},
	}
	p, events := newTestPoller(t, f, 2000)
	p.Store.SetFetchCursor("oc_r", 1000)

	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 || !strings.Contains(string((*events)[0]), `"kind":"restricted"`) ||
		!strings.Contains(string((*events)[0]), "产品技术部") {
		t.Fatalf("want 1 restricted alert, got %q", *events)
	}
	if ts, _ := p.Store.FetchCursor("oc_r"); ts != 1000 {
		t.Errorf("cursor should be kept, got %d", ts)
	}
	if _, ok := p.Store.RestrictedGet("oc_r"); !ok {
		t.Error("want restricted marker persisted")
	}

	// 标记未过期：跳过拉取，不重复告警
	if err := p.tick(context.Background(), 2100, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(f.chatCalls) != 1 {
		t.Errorf("marked chat should be skipped, calls: %v", f.chatCalls)
	}
	if len(*events) != 1 {
		t.Errorf("want no duplicate alert, got %d events", len(*events))
	}
}

// TTL 重探：失败仅刷新标记不重复告警；成功清除标记并把游标夹到当下（积压不涌实时链路）。
func TestTickRestrictedReprobe(t *testing.T) {
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_r", Name: "产品技术部", Mode: "group"}},
		errs:  map[string]error{"oc_r": restrictedErr()},
	}
	p, events := newTestPoller(t, f, 2000)
	p.Store.SetFetchCursor("oc_r", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}

	ttl := restrictedReprobe()

	// 重探失败：发生一次拉取，无新告警；标记 ts 刷新使下一 tick 继续跳过
	if err := p.tick(context.Background(), 2000+ttl+1, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(f.chatCalls) != 2 {
		t.Errorf("want reprobe fetch, calls: %v", f.chatCalls)
	}
	if len(*events) != 1 {
		t.Errorf("want no duplicate alert, got %d events", len(*events))
	}
	if err := p.tick(context.Background(), 2000+ttl+100, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(f.chatCalls) != 2 {
		t.Errorf("refreshed marker should skip, calls: %v", f.chatCalls)
	}

	// 重探成功：清除标记、游标夹到当下，随后恢复常规拉取
	delete(f.errs, "oc_r")
	now2 := 2000 + 2*ttl + 2
	if err := p.tick(context.Background(), now2, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(f.chatCalls) != 3 {
		t.Errorf("want reprobe fetch after ttl, calls: %v", f.chatCalls)
	}
	if _, ok := p.Store.RestrictedGet("oc_r"); ok {
		t.Error("marker should be cleared after successful reprobe")
	}
	if ts, _ := p.Store.FetchCursor("oc_r"); ts != now2 {
		t.Errorf("cursor should clamp to now, got %d, want %d", ts, now2)
	}
	if err := p.tick(context.Background(), now2+1, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(f.chatCalls) != 4 {
		t.Errorf("want normal fetch resumed, calls: %v", f.chatCalls)
	}
}

// rawVCJSON 构造音视频会议消息（content 为空，msg_type=video_chat）。
func rawVCJSON(mid, fid, from, t string) string {
	return fmt.Sprintf(`{"message_id":%q,"chat_id":"oc_a","msg_type":"video_chat","content":"",
		"create_time":%q,"message_app_link":"https://applink.feishu.cn/client/chat/open?openChatId=oc_a",
		"sender":{"id":%q,"id_type":"open_id","name":%q,"sender_type":"user"}}`,
		mid, t, fid, from)
}

// GroupP0：同 cid 合并（代表取最后一条、Msgs 时间升序），单条原样透传，会话首见序稳定。
func TestGroupP0(t *testing.T) {
	from := "张三"
	mk := func(cid, mid, text, ts string) Message {
		return Message{P: "P0", Text: text, From: &from, T: ts, Cid: cid, Mid: mid}
	}
	a1 := mk("oc_a", "om_1", "在吗", "2026-07-17 12:00")
	a2 := mk("oc_a", "om_2", "帮我看个问题", "2026-07-17 12:01")
	b1 := mk("oc_b", "om_3", "另一个会话", "2026-07-17 12:00")

	out := GroupP0([]Message{a2, b1, a1}) // 乱序输入：组内按时间归位
	if len(out) != 2 {
		t.Fatalf("want 2 events, got %d", len(out))
	}
	g := out[0]
	if g.N != 2 || g.Mid != "om_2" || g.Text != "帮我看个问题" {
		t.Errorf("representative should be last by time: %+v", g)
	}
	if len(g.Msgs) != 2 || g.Msgs[0].Mid != "om_1" || g.Msgs[1].Mid != "om_2" {
		t.Errorf("msgs should be time-ascending: %+v", g.Msgs)
	}
	if out[1].N != 0 || out[1].Msgs != nil || out[1].Mid != "om_3" {
		t.Errorf("single message should pass through unchanged: %+v", out[1])
	}
}

// SelfLastTimes：每会话取本人最新时间，非本人不计。
func TestSelfLastTimes(t *testing.T) {
	msgs := []Message{
		{Fid: "ou_SELF", Cid: "oc_a", T: "2026-07-17 12:00"},
		{Fid: "ou_SELF", Cid: "oc_a", T: "2026-07-17 12:05"},
		{Fid: "ou_alice", Cid: "oc_a", T: "2026-07-17 12:06"},
		{Fid: "ou_SELF", Cid: "oc_b", T: "2026-07-17 11:00"},
	}
	got := SelfLastTimes(msgs, "ou_SELF")
	if len(got) != 2 || got["oc_a"] != "2026-07-17 12:05" || got["oc_b"] != "2026-07-17 11:00" {
		t.Errorf("SelfLastTimes = %v", got)
	}
	if got := SelfLastTimes(msgs, "ou_nobody"); len(got) != 0 {
		t.Errorf("want empty map, got %v", got)
	}
}

// notifyBatch：本人已回复的剔除；同分钟保留（不误抑制）；音视频会议豁免。
func TestNotifyBatch(t *testing.T) {
	selfLast := map[string]string{"oc_a": "2026-07-17 12:01"}
	batch := notifyBatch([]Message{
		{Type: "text", Cid: "oc_a", T: "2026-07-17 12:00", Mid: "om_replied"},
		{Type: "text", Cid: "oc_a", T: "2026-07-17 12:01", Mid: "om_same_minute"},
		{Type: "video_chat", Cid: "oc_a", T: "2026-07-17 12:00", Mid: "om_vc"},
		{Type: "text", Cid: "oc_b", T: "2026-07-17 12:00", Mid: "om_other"},
	}, selfLast)
	if len(batch) != 3 {
		t.Fatalf("want 3 kept, got %d: %+v", len(batch), batch)
	}
	for _, m := range batch {
		if m.Mid == "om_replied" {
			t.Error("replied message should be filtered from notify batch")
		}
	}
}

// 聚合 tick：同会话两条 P0 合并为一个事件，键序与代表字段做字节断言。
func TestTickAggregatesSameChat(t *testing.T) {
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "在吗", "2026-07-17 12:00"),
			rawMsgJSON("om_2", "ou_alice", "张三", "帮我看个问题", "2026-07-17 12:01"),
		)},
	}
	p, events := newTestPoller(t, f, 2000)
	p.Store.SetFetchCursor("oc_a", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 {
		t.Fatalf("want 1 aggregated event, got %d: %q", len(*events), *events)
	}
	want := `{"p":"P0","n":2,"msgs":[` +
		`{"text":"在吗","from":"张三","t":"2026-07-17 12:00","type":"text","mid":"om_1","fid":"ou_alice"},` +
		`{"text":"帮我看个问题","from":"张三","t":"2026-07-17 12:01","type":"text","mid":"om_2","fid":"ou_alice"}],` +
		`"text":"帮我看个问题","from":"张三","chat":null,"t":"2026-07-17 12:01","ctype":"p2p","type":"text",` +
		`"mid":"om_2","cid":"oc_a","fid":"ou_alice","ftype":"user",` +
		`"link":"lark://applink.feishu.cn/client/chat/open?openChatId=oc_a"}` + "\n"
	if got := string((*events)[0]); got != want {
		t.Errorf("aggregated event mismatch:\n got %s\nwant %s", got, want)
	}
}

// 音视频会议不聚合：同会话同 tick 里 vc 即时单发，其余文本照常聚合。
func TestTickVCNotAggregated(t *testing.T) {
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawVCJSON("om_v", "ou_alice", "张三", "2026-07-17 12:00"),
			rawMsgJSON("om_1", "ou_alice", "张三", "进来聊", "2026-07-17 12:01"),
			rawMsgJSON("om_2", "ou_alice", "张三", "说个事", "2026-07-17 12:02"),
		)},
	}
	p, events := newTestPoller(t, f, 2000)
	p.Store.SetFetchCursor("oc_a", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 2 {
		t.Fatalf("want vc single + aggregated text, got %d: %q", len(*events), *events)
	}
	vc, agg := string((*events)[0]), string((*events)[1])
	if !strings.Contains(vc, `"type":"video_chat"`) || strings.Contains(vc, `"n":`) {
		t.Errorf("first event should be un-aggregated vc: %s", vc)
	}
	if !strings.Contains(agg, `"n":2`) || !strings.Contains(agg, `"mid":"om_2"`) {
		t.Errorf("texts should aggregate to n=2 with last as representative: %s", agg)
	}
}

// replied 注记：本人发言严格晚于 P0（分钟精度）时事件带 replied:true；本人消息本身不成事件。
func TestTickRepliedAnnotation(t *testing.T) {
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "在吗", "2026-07-17 12:00"),
			rawMsgJSON("om_2", "ou_SELF", "我", "在的，你说", "2026-07-17 12:01"),
		)},
	}
	p, events := newTestPoller(t, f, 2000)
	p.Store.SetFetchCursor("oc_a", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 {
		t.Fatalf("want 1 event, got %d: %q", len(*events), *events)
	}
	got := string((*events)[0])
	if !strings.Contains(got, `"p":"P0","replied":true,"text":"在吗"`) {
		t.Errorf("want replied annotation right after p, got: %s", got)
	}
}

// tick 中途 auth 失败不得吞掉已缓冲的 P0：collect 已把 mid 写入 seen，
// 提前返回若不先发射，重启后被当重复过滤，消息永久丢失。
func TestTickAuthErrorFlushesBufferedP0(t *testing.T) {
	f := &listFake{
		chats: []ChatMeta{
			{Cid: "oc_a", Name: "张三", Mode: "p2p"},
			{Cid: "oc_b", Name: "李四", Mode: "p2p"},
		},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "在吗", "2026-07-17 12:00"),
		)},
		errs: map[string]error{"oc_b": errors.New("NeedUserAuthorization")},
	}
	p, events := newTestPoller(t, f, 2000)
	p.Store.SetFetchCursor("oc_a", 1000)
	p.Store.SetFetchCursor("oc_b", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); !IsAuthError(err) {
		t.Fatalf("want auth error, got %v", err)
	}
	if len(*events) != 1 || !strings.Contains(string((*events)[0]), `"mid":"om_1"`) {
		t.Fatalf("buffered P0 must flush before auth return, got %q", *events)
	}
}

// replied 同分钟不标记：宁可多提醒，不可误标。
func TestMarkRepliedSameMinute(t *testing.T) {
	events := MarkReplied([]Message{{Cid: "oc_a", T: "2026-07-17 12:00"}},
		map[string]string{"oc_a": "2026-07-17 12:00"})
	if events[0].Replied {
		t.Error("same-minute self reply must not mark replied")
	}
}

// 停机夹紧：ClampFetchCursors 把全部游标夹到指定时刻。
func TestClampFetchCursors(t *testing.T) {
	s := openTestStore(t)
	s.SetFetchCursor("oc_a", 1000)
	s.SetFetchCursor("oc_b", 2000)
	s.ClampFetchCursors(5000)
	for _, cid := range []string{"oc_a", "oc_b"} {
		if ts, ok := s.FetchCursor(cid); !ok || ts != 5000 {
			t.Fatalf("clamp %s: %d %v", cid, ts, ok)
		}
	}
}

// 通知延迟：配置 notify 脚本时，文本 P0 不即时执行脚本（入延迟队列等草稿），
// 音视频会议仍即时弹出；到期后由 flushDueNotify 兜底释放。
func TestTickDefersNotifyUntilDraftOrTimeout(t *testing.T) {
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "帮我看个问题", "2026-07-17 12:00"),
		)},
	}
	p, _ := newTestPoller(t, f, 2000)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, p.Paths.ConfigDir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)
	p.Store.SetFetchCursor("oc_a", 1000)

	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("notify script ran at tick time, want deferred")
	}
	if rang.Load() != 0 {
		t.Errorf("bell rang %d times at tick time, want 0", rang.Load())
	}

	// 未到期不释放；到期释放且内容与即时通知一致
	p.flushDueNotify(context.Background(), 2000+notifyGraceSecs()-1)
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("notify released before grace expired")
	}
	p.flushDueNotify(context.Background(), 2000+notifyGraceSecs())
	if got := string(waitForFile(t, out)); got != "张三（私聊）: 帮我看个问题" {
		t.Errorf("deferred notify message: got %q", got)
	}
	if rang.Load() != 1 {
		t.Errorf("bell rang %d times after flush, want 1", rang.Load())
	}
}

// 音视频会议不延迟：即时走专用内置弹窗，不经通用 notify 脚本、不入延迟队列。
func TestTickNotifiesVCImmediately(t *testing.T) {
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	calls := stubVCDialog(t)
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawVCJSON("om_v3", "ou_alice", "张三", "2026-07-17 12:00"),
		)},
	}
	p, _ := newTestPoller(t, f, 2000)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, p.Paths.ConfigDir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)
	p.Store.SetFetchCursor("oc_a", 1000)

	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if got := waitForDialog(t, calls); got[1] != "张三（私聊）: 发起了音视频会议" {
		t.Errorf("vc dialog message: got %q", got[1])
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Error("generic notify script ran for VC, want dedicated dialog only")
	}
	if msgs, _ := p.Store.NotifyDeferTakeDue(1 << 40); len(msgs) != 0 {
		t.Errorf("vc must not enter defer queue: %v", msgs)
	}
}

// notify-vc 配置存在时覆盖内置弹窗；VC 批次仍不经通用 notify 脚本。
func TestTickVCPrefersNotifyVCScript(t *testing.T) {
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	calls := stubVCDialog(t)
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "李四", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawVCJSON("om_v4", "ou_bob", "李四", "2026-07-17 12:01"),
		)},
	}
	p, _ := newTestPoller(t, f, 2000)
	outDir := t.TempDir()
	t.Setenv("LW_TEST_OUT", outDir)
	writeConfig(t, p.Paths.ConfigDir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT/generic"`)
	writeConfig(t, p.Paths.ConfigDir, "notify-vc",
		`printf '%s|%s' "$LW_TITLE" "$LW_MESSAGE" > "$LW_TEST_OUT/vc"`)
	p.Store.SetFetchCursor("oc_a", 1000)

	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if got := string(waitForFile(t, filepath.Join(outDir, "vc"))); got != "📞 音视频会议|李四（私聊）: 发起了音视频会议" {
		t.Errorf("notify-vc output: got %q", got)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(outDir, "generic")); err == nil {
		t.Error("generic notify script ran for VC")
	}
	if len(calls) != 0 {
		t.Error("builtin dialog called despite notify-vc script")
	}
}

// notify 总开关缺失时 notify-vc 不生效：VC 不弹任何通知。
func TestTickVCNeedsNotifyMasterSwitch(t *testing.T) {
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	calls := stubVCDialog(t)
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "王五", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawVCJSON("om_v5", "ou_carol", "王五", "2026-07-17 12:02"),
		)},
	}
	p, _ := newTestPoller(t, f, 2000)
	outDir := t.TempDir()
	t.Setenv("LW_TEST_OUT", outDir)
	writeConfig(t, p.Paths.ConfigDir, "notify-vc", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT/vc"`)
	p.Store.SetFetchCursor("oc_a", 1000)

	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(outDir, "vc")); err == nil {
		t.Error("notify-vc ran without notify master switch")
	}
	if len(calls) != 0 {
		t.Error("builtin dialog called without notify master switch")
	}
}

// grace=0 的即时回退不混批：VC 走专用弹窗、文本走通用脚本，内容互不混入。
func TestDispatchNotifyGraceZeroSplits(t *testing.T) {
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	calls := stubVCDialog(t)
	t.Setenv("LW_NOTIFY_GRACE", "0")
	p, _ := newTestPoller(t, &listFake{}, 2000)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)

	p.dispatchNotify(context.Background(), `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`, []Message{
		{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat", Link: "lark://vc"},
		{From: strPtr("张三"), Ctype: "p2p", Type: "text", Text: "在吗"},
	}, 2000)

	if got := waitForDialog(t, calls); got[1] != "李四（私聊）: 发起了音视频会议" {
		t.Errorf("vc dialog message: got %q", got[1])
	}
	if got := string(waitForFile(t, out)); got != "张三（私聊）: 在吗" {
		t.Errorf("generic notify message: got %q", got)
	}
}

// 延迟入库失败退回即时通知，同样不混批。
func TestDispatchNotifyDeferFailFallback(t *testing.T) {
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	calls := stubVCDialog(t)
	p, _ := newTestPoller(t, &listFake{}, 2000)
	p.Store.Close() // NotifyDeferPut 必然失败
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)

	p.dispatchNotify(context.Background(), `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`, []Message{
		{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat", Link: "lark://vc"},
		{From: strPtr("张三"), Ctype: "p2p", Type: "text", Text: "在吗"},
	}, 2000)

	if got := waitForDialog(t, calls); got[1] != "李四（私聊）: 发起了音视频会议" {
		t.Errorf("vc dialog message: got %q", got[1])
	}
	if got := string(waitForFile(t, out)); got != "张三（私聊）: 在吗" {
		t.Errorf("generic notify message: got %q", got)
	}
}

// LW_NOTIFY_GRACE=0 恢复全部即时通知。
func TestTickNotifyGraceZeroImmediate(t *testing.T) {
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	t.Setenv("LW_NOTIFY_GRACE", "0")
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "在吗", "2026-07-17 12:00"),
		)},
	}
	p, _ := newTestPoller(t, f, 2000)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, p.Paths.ConfigDir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)
	p.Store.SetFetchCursor("oc_a", 1000)

	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if got := string(waitForFile(t, out)); got != "张三（私聊）: 在吗" {
		t.Errorf("immediate notify message: got %q", got)
	}
	if msgs, _ := p.Store.NotifyDeferTakeDue(1 << 40); len(msgs) != 0 {
		t.Errorf("grace=0 must not defer: %v", msgs)
	}
}
