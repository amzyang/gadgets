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
CREATE TABLE IF NOT EXISTS pending (mid TEXT PRIMARY KEY, draft TEXT NOT NULL, format TEXT NOT NULL DEFAULT 'text', extras TEXT NOT NULL DEFAULT '[]', card TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS digest_buf (id INTEGER PRIMARY KEY AUTOINCREMENT, msg TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS notify_wait (mid TEXT PRIMARY KEY, cid TEXT NOT NULL, msg TEXT NOT NULL, due INTEGER NOT NULL);
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
// col 是该迁移给 pending 新增的列名，作为「已完成」的结构探针供版本校准使用。
var migrations = []struct {
	sql string
	col string
}{
	// v1: pending.format。曾以 try-ALTER 方式发布（未写版本号），存量库可能已有
	// 该列，靠版本校准跳过。
	{`ALTER TABLE pending ADD COLUMN format TEXT NOT NULL DEFAULT 'text'`, "format"},
	// v2: 多候选草稿。draft 保留候选 0 原文（版本偏斜期旧二进制读到的仍是合法
	// 草稿），extras 存候选 1..n-1 的 JSON 数组。
	{`ALTER TABLE pending ADD COLUMN extras TEXT NOT NULL DEFAULT '[]'`, "extras"},
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
	if v >= len(migrations) {
		return nil // 版本超前 = 更新的二进制迁移过，绝不能回写降级
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
	if v >= len(migrations) {
		return nil // 并发者已完成迁移
	}
	if fresh {
		v = len(migrations) // 建表即最新结构，无迁移可跑
	} else {
		// 版本校准：按实际列结构定位版本下界——覆盖 v1 try-ALTER 发布期（未写
		// 版本号），以及无 >= 守卫的历史二进制打开新库时把版本号回写降级的情形；
		// 列已存在即视为对应迁移已完成，避免重跑 ALTER 报 duplicate column。
		for i, m := range migrations {
			if v > i {
				continue
			}
			var n int
			if err := conn.QueryRowContext(ctx,
				`SELECT count(*) FROM pragma_table_info('pending') WHERE name = ?`, m.col).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				v = i + 1
			}
		}
	}
	for ; v < len(migrations); v++ {
		if _, err := conn.ExecContext(ctx, migrations[v].sql); err != nil {
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

// ---------- pending（卡片草稿候选+卡片原稿）----------

// PendingPut 落盘草稿候选（len(drafts) >= 1 由调用方保证）：候选 0 进 draft 列，
// 其余进 extras（JSON 数组）。
func (s *Store) PendingPut(mid string, drafts []string, format, card string, now int64) error {
	extras, _ := json.Marshal(drafts[1:])
	_, err := s.db.Exec(
		`INSERT INTO pending(mid, draft, format, extras, card, created) VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(mid) DO UPDATE SET draft = excluded.draft, format = excluded.format, extras = excluded.extras, card = excluded.card, created = excluded.created`,
		mid, drafts[0], format, string(extras), card, now)
	return err
}

func (s *Store) PendingGet(mid string) (drafts []string, format, card string, ok bool) {
	var draft, extras string
	if err := s.db.QueryRow(`SELECT draft, format, extras, card FROM pending WHERE mid = ?`, mid).
		Scan(&draft, &format, &extras, &card); err != nil {
		return nil, "", "", false
	}
	var rest []string
	json.Unmarshal([]byte(extras), &rest) // DEFAULT '[]' 保证可解码
	return append([]string{draft}, rest...), format, card, true
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

// ---------- notify_wait（P0 延迟通知：等草稿卡片发出后展示，超时兜底）----------

// NotifyDeferPut 把通知批次写入延迟队列（mid 冲突忽略——seen 去重后不应重复）。
func (s *Store) NotifyDeferPut(msgs []Message, due int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range msgs {
		b, _ := json.Marshal(m)
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO notify_wait(mid, cid, msg, due) VALUES(?, ?, ?, ?)`,
			m.Mid, m.Cid, string(b), due); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NotifyDeferTakeDue 取出并删除全部到期条目（同一事务；与 NotifyDeferClaimChat
// 靠事务互斥，poller 兜底与 send-card 认领不会重复通知同一条）。
func (s *Store) NotifyDeferTakeDue(now int64) ([]Message, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := notifyWaitScan(tx.Query(
		`SELECT msg FROM notify_wait WHERE due <= ? ORDER BY rowid`, now))
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(`DELETE FROM notify_wait WHERE due <= ?`, now); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// NotifyDeferClaimChat 按 mid 认领并删除同会话的全部延迟条目——草稿针对整个
// 会话的诉求，同 cid 的积压一并释放。查无此 mid（未延迟 / 已超时弹出 / 补课
// 路径）返回 ok=false，任何一步失败也按未认领处理（条目留待超时兜底，不丢通知）。
func (s *Store) NotifyDeferClaimChat(mid string) ([]Message, bool) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false
	}
	defer tx.Rollback()
	var cid string
	if err := tx.QueryRow(`SELECT cid FROM notify_wait WHERE mid = ?`, mid).Scan(&cid); err != nil {
		return nil, false
	}
	out, err := notifyWaitScan(tx.Query(
		`SELECT msg FROM notify_wait WHERE cid = ? ORDER BY rowid`, cid))
	if err != nil {
		return nil, false
	}
	if _, err := tx.Exec(`DELETE FROM notify_wait WHERE cid = ?`, cid); err != nil {
		return nil, false
	}
	return out, tx.Commit() == nil
}

// NotifyDeferPurge 丢弃 due 早于 before 的陈旧条目，返回删除数（启动清理用：
// 停机太久后重启不弹旧消息，对齐游标夹紧哲学）。
func (s *Store) NotifyDeferPurge(before int64) int {
	res, err := s.db.Exec(`DELETE FROM notify_wait WHERE due < ?`, before)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// notifyWaitScan 把 notify_wait 查询结果解码为消息列表（插入序即时间序）。
func notifyWaitScan(rows *sql.Rows, err error) ([]Message, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
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
		s.PendingPut(mid, []string{string(draft)}, "text", string(card), 0)
	}
	os.Rename(pendingDir, pendingDir+".imported")
	logf("migrated legacy pending/ into sqlite")
}
