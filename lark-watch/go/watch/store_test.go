package watch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func msg(mid string) Message { return Message{Mid: mid} }

func TestSeenFilterAndCap(t *testing.T) {
	s, _ := openTestStore(t)
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
	s, _ := openTestStore(t)
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
	s, _ := openTestStore(t)
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
	s, _ := openTestStore(t)
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
	s, _ := openTestStore(t)
	s.PendingPut("om_1", "草稿", `{"schema":"2.0"}`, 100)
	draft, card, ok := s.PendingGet("om_1")
	if !ok || draft != "草稿" || card != `{"schema":"2.0"}` {
		t.Fatalf("get: %q %q %v", draft, card, ok)
	}
	if s.PendingCount() != 1 {
		t.Fatal("count != 1")
	}
	s.PendingDelete("om_1")
	if _, _, ok := s.PendingGet("om_1"); ok {
		t.Fatal("should be deleted")
	}
}

func TestDigestBuffer(t *testing.T) {
	s, _ := openTestStore(t)
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
	s, _ := openTestStore(t)
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
	if draft, _, ok := s.PendingGet("om_1"); !ok || draft != "旧草稿\n" {
		t.Fatalf("pending: %q %v", draft, ok)
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
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := s2.MarkProcessed([]string{"oc_x"}, int64(j)); err != nil {
					t.Errorf("mark write: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
}
