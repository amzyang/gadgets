package watch

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func msg(mid string) Message { return Message{Mid: mid} }

func TestSeenFilterAndCap(t *testing.T) {
	s := openTestStore(t)
	batch := []Message{msg("m1"), msg("m2")}
	fresh, err := s.FilterNewMessages(batch, 100, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 2 {
		t.Fatalf("first pass: want 2 fresh, got %d", len(fresh))
	}
	fresh, _ = s.FilterNewMessages(batch, 101, 5)
	if len(fresh) != 0 {
		t.Fatalf("second pass (重叠窗重复): want 0 fresh, got %d", len(fresh))
	}
	// 滚动上限：max=3，共 5 条，仅最新 3 条保留 → m1/m2 再来应视为新
	s.FilterNewMessages([]Message{msg("m3"), msg("m4"), msg("m5")}, 102, 3)
	fresh, _ = s.FilterNewMessages([]Message{msg("m1"), msg("m5")}, 103, 3)
	if len(fresh) != 1 || fresh[0].Mid != "m1" {
		t.Fatalf("after cap: want [m1] fresh, got %v", fresh)
	}
}

func TestHandledDedup(t *testing.T) {
	s := openTestStore(t)
	dup, err := s.HandledSeen("e1", 100, 1000)
	if err != nil || dup {
		t.Fatalf("first: dup=%v err=%v", dup, err)
	}
	dup, _ = s.HandledSeen("e1", 101, 1000)
	if !dup {
		t.Fatal("second occurrence should be dup")
	}
}

func TestProcessedUpsertAndCursors(t *testing.T) {
	s := openTestStore(t)
	s.MarkProcessed([]string{"oc_a"}, 1000)
	s.MarkProcessed([]string{"oc_a", "oc_b"}, 2000)
	cur, err := s.ProcessedCursors()
	if err != nil {
		t.Fatal(err)
	}
	if cur["oc_a"] != 2000 || cur["oc_b"] != 2000 || len(cur) != 2 {
		t.Fatalf("cursors: %v", cur)
	}
}

func TestFetchedCursors(t *testing.T) {
	s := openTestStore(t)
	if _, ok := s.FetchCursor("oc_a"); ok {
		t.Fatal("empty store should have no cursor")
	}
	s.SetFetchCursor("oc_a", 1000)
	s.SetFetchCursor("oc_a", 2000) // upsert 覆盖
	s.SetFetchCursor("oc_b", 3000)
	if ts, ok := s.FetchCursor("oc_a"); !ok || ts != 2000 {
		t.Fatalf("oc_a: %d %v", ts, ok)
	}
	if ts, ok := s.FetchCursor("oc_b"); !ok || ts != 3000 {
		t.Fatalf("oc_b: %d %v", ts, ok)
	}
}

func TestPendingLifecycle(t *testing.T) {
	s := openTestStore(t)
	s.PendingPut("om_1", []string{"草稿"}, "markdown", `{"schema":"2.0"}`, 100)
	drafts, format, card, ok := s.PendingGet("om_1")
	if !ok || len(drafts) != 1 || drafts[0] != "草稿" || format != "markdown" || card != `{"schema":"2.0"}` {
		t.Fatalf("get: %q %q %q %v", drafts, format, card, ok)
	}
	// 多候选往返（同 mid upsert 覆盖）
	s.PendingPut("om_1", []string{"候选A", "候选B", "候选C"}, "text", `{}`, 200)
	drafts, format, _, ok = s.PendingGet("om_1")
	if !ok || format != "text" || len(drafts) != 3 || drafts[0] != "候选A" || drafts[2] != "候选C" {
		t.Fatalf("multi get: %q %q %v", drafts, format, ok)
	}
	if s.PendingCount() != 1 {
		t.Fatal("count != 1")
	}
	s.PendingDelete("om_1")
	if _, _, _, ok := s.PendingGet("om_1"); ok {
		t.Fatal("should be deleted")
	}
}

// card_mid（发卡后回填的卡片自身 message_id）读写往返；未回填读出空串，
// 无记录 !ok。
func TestPendingCardMid(t *testing.T) {
	s := openTestStore(t)
	s.PendingPut("om_cm", []string{"草稿"}, "text", `{"schema":"2.0"}`, 1)

	card, cardMid, ok := s.PendingCard("om_cm")
	if !ok || card != `{"schema":"2.0"}` || cardMid != "" {
		t.Fatalf("before set: card=%q cardMid=%q ok=%v", card, cardMid, ok)
	}
	if err := s.PendingSetCardMid("om_cm", "om_card_x"); err != nil {
		t.Fatal(err)
	}
	if _, cardMid, ok = s.PendingCard("om_cm"); !ok || cardMid != "om_card_x" {
		t.Fatalf("after set: cardMid=%q ok=%v", cardMid, ok)
	}
	if _, _, ok := s.PendingCard("om_none"); ok {
		t.Fatal("missing mid should be !ok")
	}
}

// v2 库（有 format/extras、user_version=2）补 card_mid 列升 v3，存量行 card_mid
// 读出空串（改卡自然跳过）。
func TestPendingCardMidMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "lark-watch.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE pending (mid TEXT PRIMARY KEY, draft TEXT NOT NULL, format TEXT NOT NULL DEFAULT 'text', extras TEXT NOT NULL DEFAULT '[]', card TEXT NOT NULL, created INTEGER NOT NULL);
		INSERT INTO pending VALUES('om_old', '旧草稿', 'text', '[]', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	card, cardMid, ok := s.PendingCard("om_old")
	if !ok || card != "{}" || cardMid != "" {
		t.Fatalf("migrated row: card=%q cardMid=%q ok=%v", card, cardMid, ok)
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version = %d (err=%v), want %d", v, err, len(migrations))
	}
}

// v0 旧库（pending 无 format/extras 列）打开时连跳两级补列并落 user_version，
// 存量行按 text/单候选读出；二次打开走「版本已最新」快速路径。
func TestPendingFormatMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "lark-watch.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE pending (mid TEXT PRIMARY KEY, draft TEXT NOT NULL, card TEXT NOT NULL, created INTEGER NOT NULL);
		INSERT INTO pending VALUES('om_old', '旧草稿', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ {
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		drafts, format, _, ok := s.PendingGet("om_old")
		if !ok || len(drafts) != 1 || drafts[0] != "旧草稿" || format != "text" {
			t.Fatalf("migrated row: %q %q %v", drafts, format, ok)
		}
		var v int
		if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
			t.Fatalf("user_version = %d (err=%v), want %d", v, err, len(migrations))
		}
		s.Close()
	}
}

// v1 库（有 format 列、user_version=1）补 extras 列升 v2，存量行读出为单候选。
func TestPendingExtrasMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "lark-watch.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE pending (mid TEXT PRIMARY KEY, draft TEXT NOT NULL, format TEXT NOT NULL DEFAULT 'text', card TEXT NOT NULL, created INTEGER NOT NULL);
		INSERT INTO pending VALUES('om_old', '旧草稿', 'markdown', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drafts, format, _, ok := s.PendingGet("om_old")
	if !ok || len(drafts) != 1 || drafts[0] != "旧草稿" || format != "markdown" {
		t.Fatalf("migrated row: %q %q %v", drafts, format, ok)
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version = %d (err=%v), want %d", v, err, len(migrations))
	}
}

// 版本号被无守卫的历史二进制回写降级后（列结构其实已最新），按列结构校准版本，
// 不重跑 ALTER（否则 duplicate column 永久打不开库）。
func TestMigrateRecalibratesFromColumns(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.PendingPut("om_1", []string{"候选A", "候选B"}, "text", "{}", 1)
	if _, err := s.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen after downgraded version: %v", err)
	}
	defer s.Close()
	if drafts, _, _, ok := s.PendingGet("om_1"); !ok || len(drafts) != 2 {
		t.Fatalf("row lost after recalibration: %q %v", drafts, ok)
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version = %d (err=%v), want %d", v, err, len(migrations))
	}
}

// 旧二进制打开更新版本的库（user_version 超前）：不跑迁移、不回写版本号——
// 回写降级会让新二进制重跑已完成的 ALTER 而永久打不开库。
func TestMigrateNoDowngrade(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	future := len(migrations) + 1
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, future)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != future {
		t.Fatalf("user_version = %d (err=%v), want %d（超前版本号不得回写降级）", v, err, future)
	}
}

// 全新库建表即最新结构，migrate 应直落 len(migrations) 而不执行迁移循环——
// 追加伪 migration（非法 SQL）后全新 OpenStore 仍成功，即证循环未跑。
func TestFreshStoreSkipsMigrations(t *testing.T) {
	migrations = append(migrations, struct{ sql, col string }{`THIS IS NOT SQL`, "no_such_column"})
	defer func() { migrations = migrations[:len(migrations)-1] }()

	s := openTestStore(t)
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version = %d (err=%v), want %d", v, err, len(migrations))
	}
}

// 并发 OpenStore 一个旧库（run daemon 与 catchup/send-card 独立进程场景）：
// 迁移在写锁内互斥 + 锁内重读版本，两边都成功且读到迁移后的数据。
// 旧库表齐全、已是 WAL，仅 pending 缺 format 列——全新库的首次并发创建不在
// 保证范围（实际由 run 单独首建）。
func TestOpenStoreConcurrent(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "lark-watch.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema + `
		DROP TABLE pending;
		CREATE TABLE pending (mid TEXT PRIMARY KEY, draft TEXT NOT NULL, card TEXT NOT NULL, created INTEGER NOT NULL);
		INSERT INTO pending VALUES('om_old', '旧草稿', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := OpenStore(dir)
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			if _, format, _, ok := s.PendingGet("om_old"); !ok || format != "text" {
				t.Errorf("migrated row: format=%q ok=%v", format, ok)
			}
		}()
	}
	wg.Wait()
}

func TestDigestBuffer(t *testing.T) {
	s := openTestStore(t)
	chat := "群"
	s.DigestAppend([]Message{{P: "P1", Mid: "m1", Cid: "oc_x", Chat: &chat, Text: "hi", T: "2026-07-17 12:00"}})
	if s.DigestCount() != 1 {
		t.Fatal("count != 1")
	}
	msgs, err := s.DigestTake()
	if err != nil || len(msgs) != 1 || msgs[0].Mid != "m1" || *msgs[0].Chat != "群" {
		t.Fatalf("take: %v %v", msgs, err)
	}
	if s.DigestCount() != 0 {
		t.Fatal("buffer not cleared")
	}
}

func TestCatchupLast(t *testing.T) {
	s := openTestStore(t)
	s.CatchupLastSet([]string{"oc_x", "oc_y"})
	s.CatchupLastSet([]string{"oc_z"})
	cids, _ := s.CatchupLastGet()
	if len(cids) != 1 || cids[0] != "oc_z" {
		t.Fatalf("catchup_last: %v", cids)
	}
}

func TestLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cursor"), []byte("1784262101\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "processed.tsv"), []byte("oc_a\t1000\noc_b\t2000\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "seen.ids"), []byte("m1\nm2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "handled.ids"), []byte("e1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "catchup.last"), []byte(`["oc_a"]`), 0o644)
	os.MkdirAll(filepath.Join(dir, "pending"), 0o755)
	os.WriteFile(filepath.Join(dir, "pending", "om_1.md"), []byte("旧草稿\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pending", "om_1.card.json"), []byte(`{"schema":"2.0"}`), 0o644)

	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if v, ok := s.MetaGet("cursor"); !ok || v != "1784262101" {
		t.Fatalf("cursor: %q %v", v, ok)
	}
	cur, _ := s.ProcessedCursors()
	if cur["oc_a"] != 1000 || cur["oc_b"] != 2000 {
		t.Fatalf("processed: %v", cur)
	}
	if fresh, _ := s.FilterNewMessages([]Message{msg("m1")}, 1, 100); len(fresh) != 0 {
		t.Fatal("seen m1 should be imported")
	}
	if dup, _ := s.HandledSeen("e1", 1, 100); !dup {
		t.Fatal("handled e1 should be imported")
	}
	if drafts, format, _, ok := s.PendingGet("om_1"); !ok || len(drafts) != 1 || drafts[0] != "旧草稿\n" || format != "text" {
		t.Fatalf("pending: %q %q %v", drafts, format, ok)
	}
	if cids, _ := s.CatchupLastGet(); len(cids) != 1 || cids[0] != "oc_a" {
		t.Fatalf("catchup_last: %v", cids)
	}
	// 原文件改名 *.imported
	for _, name := range []string{"cursor", "processed.tsv", "seen.ids", "handled.ids", "catchup.last", "pending"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be renamed", name)
		}
		if _, err := os.Stat(filepath.Join(dir, name+".imported")); err != nil {
			t.Errorf("%s.imported missing", name)
		}
	}
}

// 并发冒烟：模拟 run daemon 与 mark/catchup 独立进程同库并发（两个连接）
func TestConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	s1, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := s1.FilterNewMessages([]Message{msg(string(rune('a'+i)) + string(rune('0'+j%10)))}, int64(j), 100); err != nil {
					t.Errorf("seen write: %v", err)
				}
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := s2.MarkProcessed([]string{"oc_x"}, int64(j)); err != nil {
					t.Errorf("mark write: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}

// restricted 标记：Set/Get/Clear/List 往返，Set 幂等刷新时间戳。
func TestRestrictedMarker(t *testing.T) {
	s := openTestStore(t)
	if _, ok := s.RestrictedGet("oc_a"); ok {
		t.Fatal("empty store: want no marker")
	}
	s.RestrictedSet("oc_a", "产品技术部", 1000)
	s.RestrictedSet("oc_b", "群B", 2000)
	if ts, ok := s.RestrictedGet("oc_a"); !ok || ts != 1000 {
		t.Fatalf("get oc_a: %d %v, want 1000 true", ts, ok)
	}
	s.RestrictedSet("oc_a", "产品技术部", 3000)
	if ts, _ := s.RestrictedGet("oc_a"); ts != 3000 {
		t.Fatalf("refresh ts: got %d, want 3000", ts)
	}
	list, err := s.RestrictedList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Cid != "oc_a" || list[0].Name != "产品技术部" || list[0].Since != 3000 {
		t.Fatalf("list: %+v", list)
	}
	s.RestrictedClear("oc_a")
	if _, ok := s.RestrictedGet("oc_a"); ok {
		t.Fatal("clear: want no marker")
	}
	if list, _ := s.RestrictedList(); len(list) != 1 {
		t.Fatalf("list after clear: %+v", list)
	}
}

// notify_wait：到期取走、按 mid 认领同会话、陈旧清理；两侧互斥不重复取。
func TestNotifyDeferStore(t *testing.T) {
	s := openTestStore(t)
	err := s.NotifyDeferPut([]Message{
		{Mid: "om_a1", Cid: "oc_a", Text: "在吗"},
		{Mid: "om_a2", Cid: "oc_a", Text: "帮我看个问题"},
		{Mid: "om_b1", Cid: "oc_b", Text: "另一个会话"},
	}, 1000)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := s.NotifyDeferTakeDue(999); err != nil || len(got) != 0 {
		t.Fatalf("before due: want empty, got %v (%v)", got, err)
	}

	msgs, ok := s.NotifyDeferClaimChat("om_a2")
	if !ok || len(msgs) != 2 || msgs[0].Mid != "om_a1" || msgs[1].Mid != "om_a2" {
		t.Fatalf("claim by mid should return whole chat in order: %v %v", msgs, ok)
	}
	if _, ok := s.NotifyDeferClaimChat("om_a1"); ok {
		t.Error("second claim must find nothing")
	}

	got, err := s.NotifyDeferTakeDue(1000)
	if err != nil || len(got) != 1 || got[0].Mid != "om_b1" {
		t.Fatalf("take due: want om_b1 only, got %v (%v)", got, err)
	}
	if got, _ := s.NotifyDeferTakeDue(1000); len(got) != 0 {
		t.Errorf("second take must be empty, got %v", got)
	}
}

func TestNotifyDeferPurge(t *testing.T) {
	s := openTestStore(t)
	s.NotifyDeferPut([]Message{{Mid: "om_old", Cid: "oc_a"}}, 100)
	s.NotifyDeferPut([]Message{{Mid: "om_new", Cid: "oc_b"}}, 2000)
	if n := s.NotifyDeferPurge(1000); n != 1 {
		t.Fatalf("want 1 purged, got %d", n)
	}
	got, _ := s.NotifyDeferTakeDue(9999)
	if len(got) != 1 || got[0].Mid != "om_new" {
		t.Errorf("fresh entry should survive purge: %v", got)
	}
}

// 到期释放按会话为单位：同 cid 未到期条目随到期条目一并带走（跨 tick 积压
// 一次合并弹出，不再几十秒后二次弹旧内容），他会话不受影响。
func TestNotifyDeferTakeDueChatLevel(t *testing.T) {
	s := openTestStore(t)
	s.NotifyDeferPut([]Message{{Mid: "om_a1", Cid: "oc_a", Text: "第一条"}}, 1000)
	s.NotifyDeferPut([]Message{{Mid: "om_a2", Cid: "oc_a", Text: "第二条"}}, 2000)
	s.NotifyDeferPut([]Message{{Mid: "om_b1", Cid: "oc_b", Text: "另一会话"}}, 3000)

	got, err := s.NotifyDeferTakeDue(1000)
	if err != nil || len(got) != 2 || got[0].Mid != "om_a1" || got[1].Mid != "om_a2" {
		t.Fatalf("due 应带走 oc_a 全部（含未到期 om_a2）: %v (%v)", got, err)
	}
	if got, _ := s.NotifyDeferTakeDue(1000); len(got) != 0 {
		t.Errorf("二次取应为空: %v", got)
	}
	got, _ = s.NotifyDeferTakeDue(3000)
	if len(got) != 1 || got[0].Mid != "om_b1" {
		t.Errorf("oc_b 应保留到自己的 due: %v", got)
	}
}

// chat_state：本人每会话最新发言时间只增不减，按 cid 子集查询，未知 cid 缺席。
func TestChatStateSelfLast(t *testing.T) {
	s := openTestStore(t)
	if err := s.SelfLastUpsert(map[string]string{
		"oc_a": "2026-07-17 12:01",
		"oc_b": "2026-07-17 09:30",
	}); err != nil {
		t.Fatal(err)
	}
	// 更旧的时间不回退（回看窗口重放），更新的时间覆盖
	if err := s.SelfLastUpsert(map[string]string{
		"oc_a": "2026-07-17 11:00",
		"oc_b": "2026-07-17 10:00",
	}); err != nil {
		t.Fatal(err)
	}
	got := s.SelfLast([]string{"oc_a", "oc_b", "oc_unknown"})
	if got["oc_a"] != "2026-07-17 12:01" {
		t.Errorf("oc_a should keep newer value, got %q", got["oc_a"])
	}
	if got["oc_b"] != "2026-07-17 10:00" {
		t.Errorf("oc_b should advance, got %q", got["oc_b"])
	}
	if _, ok := got["oc_unknown"]; ok {
		t.Error("unknown cid must be absent")
	}
	// 子集查询：只要 oc_b
	if got := s.SelfLast([]string{"oc_b"}); len(got) != 1 || got["oc_b"] != "2026-07-17 10:00" {
		t.Errorf("subset query: %v", got)
	}
	if err := s.SelfLastUpsert(nil); err != nil {
		t.Errorf("empty upsert should be no-op: %v", err)
	}
}
