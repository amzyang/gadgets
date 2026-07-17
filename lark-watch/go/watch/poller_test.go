package watch

import (
	"context"
	"fmt"
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
	s, _ := openTestStore(t)
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

// 停机夹紧：ClampFetchCursors 把全部游标夹到指定时刻。
func TestClampFetchCursors(t *testing.T) {
	s, _ := openTestStore(t)
	s.SetFetchCursor("oc_a", 1000)
	s.SetFetchCursor("oc_b", 2000)
	s.ClampFetchCursors(5000)
	for _, cid := range []string{"oc_a", "oc_b"} {
		if ts, ok := s.FetchCursor(cid); !ok || ts != 5000 {
			t.Fatalf("clamp %s: %d %v", cid, ts, ok)
		}
	}
}
