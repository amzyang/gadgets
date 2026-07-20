package watch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// tick 把本人发言时间持久化到 chat_state：延迟通知释放前重判 replied 的信号源。
func TestTickPersistsSelfLast(t *testing.T) {
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "在吗", "2026-07-17 12:00"),
			rawMsgJSON("om_2", "ou_SELF", "我", "在的", "2026-07-17 12:01"),
		)},
	}
	p, _ := newTestPoller(t, f, 2000)
	p.Store.SetFetchCursor("oc_a", 1000)
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if got := p.Store.SelfLast([]string{"oc_a"}); got["oc_a"] != "2026-07-17 12:01" {
		t.Errorf("self_last should persist after tick, got %v", got)
	}
}

// 跨 tick replied：上一 tick 落库的本人回复也算数——晚浮现的旧消息（search 兜底、
// has_more 续拉）同样注记 replied 并抑制通知，不再只有 90s 回看窗口的记忆。
func TestTickRepliedAcrossTicks(t *testing.T) {
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "必须是这个", "2026-07-17 12:00"),
		)},
	}
	p, events := newTestPoller(t, f, 2000)
	writeConfig(t, p.Paths.ConfigDir, "notify", `true`)
	p.Store.SetFetchCursor("oc_a", 1000)
	// 模拟上一 tick 观察到的本人回复（本 tick 消息流里没有本人发言）
	p.Store.SelfLastUpsert(map[string]string{"oc_a": "2026-07-17 12:05"})
	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 || !strings.Contains(string((*events)[0]), `"replied":true`) {
		t.Fatalf("event should carry replied from persisted self_last: %q", *events)
	}
	if msgs, _ := p.Store.NotifyDeferTakeDue(1 << 40); len(msgs) != 0 {
		t.Errorf("replied message must not enter defer queue: %v", msgs)
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

// 释放前重判：等草稿/到期期间本人已亲自回复的，兜底释放时不再弹（条目消费掉，
// 不留队列）。入队时的 replied 判断会过期，弹窗前必须用持久化 self_last 复核。
func TestFlushDueNotifySkipsReplied(t *testing.T) {
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	p, _ := newTestPoller(t, &listFake{}, 2000)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, p.Paths.ConfigDir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)
	p.Store.NotifyDeferPut([]Message{
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_1", Type: "text", Text: "在吗", T: "2026-07-17 12:00"},
	}, 2180)
	p.Store.SelfLastUpsert(map[string]string{"oc_a": "2026-07-17 12:01"})

	p.flushDueNotify(context.Background(), 2180)
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("notify ran for replied message, want dropped")
	}
	if rang.Load() != 0 {
		t.Errorf("bell rang %d times, want 0", rang.Load())
	}
	if msgs, _ := p.Store.NotifyDeferTakeDue(1 << 40); len(msgs) != 0 {
		t.Errorf("entry should be consumed, not left behind: %v", msgs)
	}
}

// 混批释放：已回复的丢弃，未回复的照常弹（内容只含未回复那条）。
func TestFlushDueNotifyDropsRepliedKeepsOthers(t *testing.T) {
	rang := stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	p, _ := newTestPoller(t, &listFake{}, 2000)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, p.Paths.ConfigDir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)
	p.Store.NotifyDeferPut([]Message{
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_1", Type: "text", Text: "必须是这个", T: "2026-07-17 12:00"},
		{From: strPtr("李四"), Cid: "oc_b", Mid: "om_2", Type: "text", Text: "帮我看下", T: "2026-07-17 12:00"},
	}, 2180)
	p.Store.SelfLastUpsert(map[string]string{"oc_a": "2026-07-17 12:03"})

	p.flushDueNotify(context.Background(), 2180)
	if got := string(waitForFile(t, out)); got != "李四（私聊）: 帮我看下" {
		t.Errorf("notify message: got %q", got)
	}
	if rang.Load() != 1 {
		t.Errorf("bell rang %d times, want 1", rang.Load())
	}
}

// 同分钟不算已回复：释放侧保持 selfRepliedAfter 的严格大于语义，宁可多提醒。
func TestFlushDueNotifySameMinuteStillNotifies(t *testing.T) {
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	p, _ := newTestPoller(t, &listFake{}, 2000)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)
	writeConfig(t, p.Paths.ConfigDir, "notify", `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`)
	p.Store.NotifyDeferPut([]Message{
		{From: strPtr("张三"), Cid: "oc_a", Mid: "om_1", Type: "text", Text: "在吗", T: "2026-07-17 12:00"},
	}, 2180)
	p.Store.SelfLastUpsert(map[string]string{"oc_a": "2026-07-17 12:00"})

	p.flushDueNotify(context.Background(), 2180)
	if got := string(waitForFile(t, out)); got != "张三（私聊）: 在吗" {
		t.Errorf("same-minute reply must still notify: got %q", got)
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

// notify 总开关 off 时 notify-vc 不生效：VC 不弹任何通知。
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
	writeConfig(t, p.Paths.ConfigDir, "notify", "off")
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

// 事件日志：keep 在去重后记 Info；drop 按理由分流（self 降 DEBUG）；重复拉取记
// msg.dup；tick 摘要与 emit/notify.skip 留痕。
func TestTickLogsKeepDropDup(t *testing.T) {
	logs := captureEvlog(t)
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_self", "ou_SELF", "我", "自己发的", "2026-07-17 12:00"),
			rawMsgJSON("om_ig", "ou_alice", "张三", "中午吃什么", "2026-07-17 12:01"),
			rawMsgJSON("om_new", "ou_alice", "张三", "帮我看个问题", "2026-07-17 12:02"),
		)},
	}
	p, _ := newTestPoller(t, f, 2000)
	writeConfig(t, p.Paths.ConfigDir, "ignore", "吃什么\n")
	writeConfig(t, p.Paths.ConfigDir, "notify", "off\n")
	p.Store.SetFetchCursor("oc_a", 1000)

	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	recs := logs()
	keeps := findLogs(recs, "msg.keep")
	if len(keeps) != 1 || keeps[0]["mid"] != "om_new" || keeps[0]["reason"] != "p2p" ||
		keeps[0]["p"] != "P0" || keeps[0]["level"] != "INFO" {
		t.Errorf("msg.keep: %v", keeps)
	}
	drops := findLogs(recs, "msg.drop")
	if len(drops) != 2 {
		t.Fatalf("want 2 msg.drop, got %v", drops)
	}
	byMid := map[string]map[string]any{}
	for _, d := range drops {
		byMid[d["mid"].(string)] = d
	}
	if d := byMid["om_self"]; d["reason"] != "self" || d["level"] != "DEBUG" {
		t.Errorf("self drop should be DEBUG: %v", d)
	}
	if d := byMid["om_ig"]; d["reason"] != "ignore:吃什么" || d["level"] != "INFO" {
		t.Errorf("ignore drop should be INFO: %v", d)
	}
	if ticks := findLogs(recs, "tick"); len(ticks) != 1 || ticks[0]["new"] != float64(1) ||
		ticks[0]["p0"] != float64(1) || ticks[0]["level"] != "INFO" {
		t.Errorf("tick summary: %v", ticks)
	}
	if emits := findLogs(recs, "emit"); len(emits) != 1 || emits[0]["kind"] != "p0" ||
		emits[0]["mid"] != "om_new" {
		t.Errorf("emit: %v", emits)
	}
	// notify 配置为 off 但有 P0 批次：跳过留痕
	if r := findLogs(recs, "notify.skip"); len(r) != 1 || r[0]["reason"] != "off" {
		t.Errorf("notify.skip: %v", r)
	}

	// 同一批消息再 tick：无新 keep，回看窗口的重复降为 msg.dup（debug）
	if err := p.tick(context.Background(), 2001, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	recs = logs()
	if keeps := findLogs(recs, "msg.keep"); len(keeps) != 1 {
		t.Errorf("second tick must not re-keep, got %v", keeps)
	}
	dups := findLogs(recs, "msg.dup")
	if len(dups) != 1 || dups[0]["mid"] != "om_new" || dups[0]["level"] != "DEBUG" {
		t.Errorf("msg.dup: %v", dups)
	}
}

// 安静 tick 摘要降为 Debug（info 级不落盘）；search 兜底跑过的 tick 保持 Info。
func TestTickSummaryLevels(t *testing.T) {
	logs := captureEvlogAt(t, slog.LevelInfo)
	p, _ := newTestPoller(t, &listFake{}, 2000)
	for i := int64(0); i < 3; i++ {
		if err := p.tick(context.Background(), 2000+i, "ou_SELF"); err != nil {
			t.Fatal(err)
		}
	}
	ticks := findLogs(logs(), "tick")
	if len(ticks) != 1 {
		t.Fatalf("want only the search tick at info level, got %v", ticks)
	}
	if ticks[0]["search"] != true || ticks[0]["new"] != float64(0) {
		t.Errorf("tick attrs: %v", ticks[0])
	}
}

// emit 钩子：每条 stdout 事件按类型记 kind 与关键 id。
func TestEmitLogged(t *testing.T) {
	logs := captureEvlog(t)
	p, _ := newTestPoller(t, &listFake{}, 2000)
	p.emit(Message{P: "P0", Mid: "om_e", Cid: "oc_a", N: 2, Replied: true})
	p.emit(NewAlert("api", "连续失败"))
	p.emit(Backlog{P: "backlog", OfflineSecs: 42})
	p.emit(BuildDigest([]Message{{P: "P1", Cid: "oc_a", Chat: strPtr("群A"), Text: "hi"}}))

	recs := findLogs(logs(), "emit")
	if len(recs) != 4 {
		t.Fatalf("want 4 emit records, got %v", recs)
	}
	if r := recs[0]; r["kind"] != "p0" || r["mid"] != "om_e" || r["n"] != float64(2) || r["replied"] != true {
		t.Errorf("p0 emit: %v", r)
	}
	if r := recs[1]; r["kind"] != "alert" || r["alert_kind"] != "api" || r["text"] != "连续失败" {
		t.Errorf("alert emit: %v", r)
	}
	if r := recs[2]; r["kind"] != "backlog" || r["offline_secs"] != float64(42) {
		t.Errorf("backlog emit: %v", r)
	}
	if r := recs[3]; r["kind"] != "digest" || r["n"] != float64(1) {
		t.Errorf("digest emit: %v", r)
	}
}

// 通知链路：defer 入队与到期 flush 都留痕（mids 可与 msg.keep 对上）。
func TestTickNotifyDeferFlushLogged(t *testing.T) {
	logs := captureEvlog(t)
	stubBell(t)
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
	defers := findLogs(logs(), "notify.defer")
	if len(defers) != 1 || defers[0]["n"] != float64(1) ||
		defers[0]["due"] != float64(2000+notifyGraceSecs()) {
		t.Fatalf("notify.defer: %v", defers)
	}
	if ms, ok := defers[0]["mids"].([]any); !ok || len(ms) != 1 || ms[0] != "om_1" {
		t.Errorf("defer mids: %v", defers[0]["mids"])
	}

	p.flushDueNotify(context.Background(), 2000+notifyGraceSecs())
	waitForFile(t, out) // 等脚本落盘，避免通知 goroutine 逸出测试
	flushes := findLogs(logs(), "notify.flush")
	if len(flushes) != 1 || flushes[0]["n"] != float64(1) || flushes[0]["script"] != true {
		t.Errorf("notify.flush: %v", flushes)
	}
}

// 本人已回复的通知抑制留痕（批次清空后不 defer）。
func TestTickNotifyRepliedLogged(t *testing.T) {
	logs := captureEvlog(t)
	f := &listFake{
		chats: []ChatMeta{{Cid: "oc_a", Name: "张三", Mode: "p2p"}},
		msgs: map[string]string{"oc_a": chatMsgsResp(false,
			rawMsgJSON("om_1", "ou_alice", "张三", "在吗", "2026-07-17 12:00"),
			rawMsgJSON("om_2", "ou_SELF", "我", "在的", "2026-07-17 12:01"),
		)},
	}
	p, _ := newTestPoller(t, f, 2000)
	writeConfig(t, p.Paths.ConfigDir, "notify", "true")
	p.Store.SetFetchCursor("oc_a", 1000)

	if err := p.tick(context.Background(), 2000, "ou_SELF"); err != nil {
		t.Fatal(err)
	}
	recs := logs()
	if r := findLogs(recs, "notify.replied"); len(r) != 1 || r[0]["n"] != float64(1) {
		t.Errorf("notify.replied: %v", r)
	}
	if r := findLogs(recs, "notify.defer"); len(r) != 0 {
		t.Errorf("empty batch must not defer: %v", r)
	}
}

// grace=0 即时路径与 vc 专用路径的分流留痕。
func TestDispatchNotifyVCNowLogged(t *testing.T) {
	logs := captureEvlog(t)
	stubBell(t)
	stubProbes(t, "net.kovidgoyal.kitty", 0)
	calls := stubVCDialog(t)
	t.Setenv("LW_NOTIFY_GRACE", "0")
	p, _ := newTestPoller(t, &listFake{}, 2000)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("LW_TEST_OUT", out)

	p.dispatchNotify(context.Background(), `printf '%s' "$LW_MESSAGE" > "$LW_TEST_OUT"`, []Message{
		{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat", Link: "lark://vc", Mid: "om_v"},
		{From: strPtr("张三"), Ctype: "p2p", Type: "text", Text: "在吗", Mid: "om_t"},
	}, 2000)
	waitForDialog(t, calls)
	waitForFile(t, out)

	if r := findLogs(logs(), "notify.vc"); len(r) != 1 || r[0]["n"] != float64(1) {
		t.Errorf("notify.vc: %v", r)
	}
	if r := findLogs(logs(), "notify.now"); len(r) != 1 || r[0]["n"] != float64(1) {
		t.Errorf("notify.now: %v", r)
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
