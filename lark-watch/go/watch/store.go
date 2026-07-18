package watch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// Store 是唯一的状态持久层（SQLite，WAL）。catchup/mark/send-card 独立进程与
// run daemon 并发读写靠 WAL + busy_timeout 保证安全。
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS seen (mid TEXT PRIMARY KEY, ts INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS handled (event_id TEXT PRIMARY KEY, ts INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS processed (cid TEXT PRIMARY KEY, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS fetched (cid TEXT PRIMARY KEY, ts INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS pending (mid TEXT PRIMARY KEY, draft TEXT NOT NULL, format TEXT NOT NULL DEFAULT 'text', card TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS digest_buf (id INTEGER PRIMARY KEY AUTOINCREMENT, msg TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS catchup_last (cid TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS restricted (cid TEXT PRIMARY KEY, name TEXT NOT NULL, ts INTEGER NOT NULL);
`

func OpenStore(stateDir string) (*Store, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(stateDir, "lark-watch.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 建表前探测全新库（尚无 pending 表）：全新库建表即最新结构，migrate 据此
	// 直落最新版本号，不跑迁移循环。
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'pending'`).Scan(&n); err != nil {
		db.Close()
		return nil, fmt.Errorf("probe fresh: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db, n == 0); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{db: db}
	s.migrateLegacy(stateDir)
	return s, nil
}

// migrations 按序演进已有表结构：migrations[i] 把 PRAGMA user_version 从 i 升到
// i+1，由 migrate 保证只执行一次。加列时在此追加一条 ALTER，并同步 schema 常量
// （全新库建表即最新结构，由 OpenStore 的 fresh 探测直落最新版本号，不经此循环）。
var migrations = []string{
	// v1: pending.format。曾以 try-ALTER 方式发布（未写版本号），存量库可能已有
	// 该列，靠 bootstrap 探测跳过。
	`ALTER TABLE pending ADD COLUMN format TEXT NOT NULL DEFAULT 'text'`,
}

// migrate 把落后的库补到最新版本；fresh（本次全新建库）只落版本号不跑迁移。
// 日常路径（版本已最新）纯读、不碰写锁；落后时进 IMMEDIATE 事务先占写锁再重读
// 版本（双检）——并发 OpenStore（run daemon 与 catchup/send-card 独立进程）的
// 后到者进锁即见新版本，自然空转。手动 BEGIN 走独占连接，不影响连接池上的其他事务。
func migrate(db *sql.DB, fresh bool) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v == len(migrations) {
		return nil
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `ROLLBACK`) // COMMIT 后无事务可回滚，空转即可
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v == len(migrations) {
		return nil // 并发者已完成迁移
	}
	if fresh {
		v = len(migrations) // 建表即最新结构，无迁移可跑
	} else if v == 0 {
		// bootstrap：未写过版本号的存量库按实际结构定位版本——try-ALTER 时期
		// 的库已有 format 列，即 v1 结构。
		var n int
		if err := conn.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info('pending') WHERE name = 'format'`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			v = 1
		}
	}
	for ; v < len(migrations); v++ {
		if _, err := conn.ExecContext(ctx, migrations[v]); err != nil {
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations))); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// ---------- meta ----------

func (s *Store) MetaGet(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v, err == nil
}

func (s *Store) MetaGetInt(key string) (int64, bool) {
	v, ok := s.MetaGet(key)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

func (s *Store) MetaSet(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

func (s *Store) MetaSetInt(key string, v int64) error {
	return s.MetaSet(key, strconv.FormatInt(v, 10))
}

// ---------- seen（消息去重，滚动保留最近 max 条）----------

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// trimToMax 滚动裁剪：仅保留表内 rowid 最新的 max 行（表名为编译期常量）。
func trimToMax(x execer, table string, max int) error {
	_, err := x.Exec(fmt.Sprintf(
		`DELETE FROM %s WHERE rowid NOT IN (SELECT rowid FROM %s ORDER BY rowid DESC LIMIT ?)`,
		table, table), max)
	return err
}

func (s *Store) FilterNewMessages(msgs []Message, now int64, max int) ([]Message, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var fresh []Message
	for _, m := range msgs {
		res, err := tx.Exec(`INSERT OR IGNORE INTO seen(mid, ts) VALUES(?, ?)`, m.Mid, now)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fresh = append(fresh, m)
		}
	}
	if len(fresh) > 0 { // 全为重复时无插入，免裁剪（回看窗口下的常态）
		if err := trimToMax(tx, "seen", max); err != nil {
			return nil, err
		}
	}
	return fresh, tx.Commit()
}

// ---------- handled（卡片事件去重，滚动 max 条）----------

// HandledSeen 返回该事件是否已处理过；未处理则记录。
func (s *Store) HandledSeen(eventID string, now int64, max int) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO handled(event_id, ts) VALUES(?, ?)`, eventID, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		err = trimToMax(s.db, "handled", max)
	}
	return n == 0, err
}

// ---------- processed（补课已处理游标）----------

func (s *Store) MarkProcessed(cids []string, at int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, cid := range cids {
		if _, err := tx.Exec(
			`INSERT INTO processed(cid, at) VALUES(?, ?) ON CONFLICT(cid) DO UPDATE SET at = excluded.at`,
			cid, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ProcessedCursors() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT cid, at FROM processed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var cid string
		var at int64
		if err := rows.Scan(&cid, &at); err != nil {
			return nil, err
		}
		out[cid] = at
	}
	return out, rows.Err()
}

// ---------- fetched（实时轮询 per-chat 拉取游标；与 processed 的「已处理」语义分离）----------

func (s *Store) FetchCursor(cid string) (int64, bool) {
	var ts int64
	err := s.db.QueryRow(`SELECT ts FROM fetched WHERE cid = ?`, cid).Scan(&ts)
	return ts, err == nil
}

func (s *Store) SetFetchCursor(cid string, ts int64) error {
	_, err := s.db.Exec(
		`INSERT INTO fetched(cid, ts) VALUES(?, ?) ON CONFLICT(cid) DO UPDATE SET ts = excluded.ts`,
		cid, ts)
	return err
}

// ClampFetchCursors 停机重启时把全部游标夹到指定时刻（积压交给 catchup，不洪泛实时链路）。
func (s *Store) ClampFetchCursors(ts int64) error {
	_, err := s.db.Exec(`UPDATE fetched SET ts = ?`, ts)
	return err
}

// ---------- restricted（防泄密模式群：API 禁止读取消息，跳过并按 TTL 重探）----------

func (s *Store) RestrictedGet(cid string) (int64, bool) {
	var ts int64
	err := s.db.QueryRow(`SELECT ts FROM restricted WHERE cid = ?`, cid).Scan(&ts)
	return ts, err == nil
}

func (s *Store) RestrictedSet(cid, name string, ts int64) error {
	_, err := s.db.Exec(
		`INSERT INTO restricted(cid, name, ts) VALUES(?, ?, ?)
		 ON CONFLICT(cid) DO UPDATE SET name = excluded.name, ts = excluded.ts`,
		cid, name, ts)
	return err
}

func (s *Store) RestrictedClear(cid string) error {
	_, err := s.db.Exec(`DELETE FROM restricted WHERE cid = ?`, cid)
	return err
}

func (s *Store) RestrictedList() ([]RestrictedChat, error) {
	rows, err := s.db.Query(`SELECT cid, name, ts FROM restricted ORDER BY cid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RestrictedChat
	for rows.Next() {
		var rc RestrictedChat
		if err := rows.Scan(&rc.Cid, &rc.Name, &rc.Since); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// ---------- pending（卡片草稿+卡片原稿）----------

func (s *Store) PendingPut(mid, draft, format, card string, now int64) error {
	_, err := s.db.Exec(
		`INSERT INTO pending(mid, draft, format, card, created) VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(mid) DO UPDATE SET draft = excluded.draft, format = excluded.format, card = excluded.card, created = excluded.created`,
		mid, draft, format, card, now)
	return err
}

func (s *Store) PendingGet(mid string) (draft, format, card string, ok bool) {
	err := s.db.QueryRow(`SELECT draft, format, card FROM pending WHERE mid = ?`, mid).Scan(&draft, &format, &card)
	return draft, format, card, err == nil
}

func (s *Store) PendingDelete(mid string) error {
	_, err := s.db.Exec(`DELETE FROM pending WHERE mid = ?`, mid)
	return err
}

func (s *Store) PendingCount() int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM pending`).Scan(&n)
	return n
}

// ---------- digest_buf（P1 摘要缓冲）----------

func (s *Store) DigestAppend(msgs []Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range msgs {
		b, _ := json.Marshal(m)
		if _, err := tx.Exec(`INSERT INTO digest_buf(msg) VALUES(?)`, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DigestCount() int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM digest_buf`).Scan(&n)
	return n
}

// DigestTake 取出全部缓冲并清空（同一事务）。
func (s *Store) DigestTake() ([]Message, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT msg FROM digest_buf ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []Message
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		var m Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM digest_buf`); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// ---------- catchup_last ----------

func (s *Store) CatchupLastSet(cids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM catchup_last`); err != nil {
		return err
	}
	for _, cid := range cids {
		if _, err := tx.Exec(`INSERT INTO catchup_last(cid) VALUES(?)`, cid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CatchupLastGet() ([]string, error) {
	rows, err := s.db.Query(`SELECT cid FROM catchup_last`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid string
		if err := rows.Scan(&cid); err != nil {
			return nil, err
		}
		out = append(out, cid)
	}
	return out, rows.Err()
}

// ---------- bash 版状态文件一次性迁移（导入后改名 *.imported 留档）----------

func (s *Store) migrateLegacy(stateDir string) {
	importFile := func(name string, fn func(content string)) {
		path := filepath.Join(stateDir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		fn(string(b))
		os.Rename(path, path+".imported")
		logf("migrated legacy %s into sqlite", name)
	}

	importFile("cursor", func(c string) {
		s.MetaSet("cursor", strings.TrimSpace(c))
	})
	importFile("last_flush", func(c string) {
		s.MetaSet("last_flush", strings.TrimSpace(c))
	})
	importFile("processed.tsv", func(c string) {
		for _, line := range strings.Split(c, "\n") {
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) != 2 {
				continue
			}
			if at, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil {
				s.MarkProcessed([]string{parts[0]}, at)
			}
		}
	})
	importFile("seen.ids", func(c string) {
		for _, mid := range strings.Fields(c) {
			s.db.Exec(`INSERT OR IGNORE INTO seen(mid, ts) VALUES(?, 0)`, mid)
		}
	})
	importFile("handled.ids", func(c string) {
		for _, id := range strings.Fields(c) {
			s.db.Exec(`INSERT OR IGNORE INTO handled(event_id, ts) VALUES(?, 0)`, id)
		}
	})
	importFile("catchup.last", func(c string) {
		var cids []string
		if json.Unmarshal([]byte(c), &cids) == nil {
			s.CatchupLastSet(cids)
		}
	})

	pendingDir := filepath.Join(stateDir, "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		mid := strings.TrimSuffix(e.Name(), ".md")
		draft, err := os.ReadFile(filepath.Join(pendingDir, e.Name()))
		if err != nil {
			continue
		}
		card, _ := os.ReadFile(filepath.Join(pendingDir, mid+".card.json"))
		s.PendingPut(mid, string(draft), "text", string(card), 0)
	}
	os.Rename(pendingDir, pendingDir+".imported")
	logf("migrated legacy pending/ into sqlite")
}
