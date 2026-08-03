// Package store is the SQLite persistence layer. It uses the pure-Go
// modernc.org/sqlite driver (no cgo) so the binary cross-compiles cleanly for
// Docker/Railway, and an FTS5 virtual table for keyword search and the ask
// agent's corpus lookups.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // WAL + single writer keeps SQLite simple and safe here
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS items (
  seq            INTEGER PRIMARY KEY AUTOINCREMENT,
  id             TEXT UNIQUE NOT NULL,
  source         TEXT NOT NULL,
  source_type    TEXT NOT NULL,
  title          TEXT NOT NULL,
  url            TEXT NOT NULL,
  authors        TEXT NOT NULL DEFAULT '[]',
  published_at   INTEGER NOT NULL DEFAULT 0,
  fetched_at     INTEGER NOT NULL DEFAULT 0,
  raw_text       TEXT NOT NULL DEFAULT '',
  points         INTEGER NOT NULL DEFAULT 0,
  enriched       INTEGER NOT NULL DEFAULT 0,
  one_liner      TEXT NOT NULL DEFAULT '',
  summary        TEXT NOT NULL DEFAULT '',
  key_ideas      TEXT NOT NULL DEFAULT '[]',
  claims         TEXT NOT NULL DEFAULT '[]',
  evidence       TEXT NOT NULL DEFAULT '[]',
  limitations    TEXT NOT NULL DEFAULT '[]',
  open_questions TEXT NOT NULL DEFAULT '[]',
  tags           TEXT NOT NULL DEFAULT '[]',
  relevance      INTEGER NOT NULL DEFAULT 0,
  novelty        INTEGER NOT NULL DEFAULT 0,
  importance     INTEGER NOT NULL DEFAULT 0,
  score          REAL NOT NULL DEFAULT 0,
  state          TEXT NOT NULL DEFAULT 'unread',
  feedback       INTEGER NOT NULL DEFAULT 0,
  theme_id       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_items_published ON items(published_at);
CREATE INDEX IF NOT EXISTS idx_items_enriched  ON items(enriched);
CREATE INDEX IF NOT EXISTS idx_items_state     ON items(state);

CREATE TABLE IF NOT EXISTS themes (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  title         TEXT NOT NULL,
  description   TEXT NOT NULL DEFAULT '',
  connections   TEXT NOT NULL DEFAULT '',
  disagreements TEXT NOT NULL DEFAULT '',
  trajectory    TEXT NOT NULL DEFAULT '',
  updated_at    INTEGER NOT NULL DEFAULT 0
);

CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(title, body, tags);
`)
	return err
}

// ---------- helpers ----------

func jsonArr(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseArr(s string) []string {
	var v []string
	if s == "" {
		return nil
	}
	_ = json.Unmarshal([]byte(s), &v)
	return v
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

const selectCols = `i.seq,i.id,i.source,i.source_type,i.title,i.url,i.authors,i.published_at,i.fetched_at,i.raw_text,i.points,i.enriched,i.one_liner,i.summary,i.key_ideas,i.claims,i.evidence,i.limitations,i.open_questions,i.tags,i.relevance,i.novelty,i.importance,i.score,i.state,i.feedback,i.theme_id,t.title`

const selectBase = `SELECT ` + selectCols + ` FROM items i LEFT JOIN themes t ON t.id=i.theme_id`

func scanItem(rows *sql.Rows) (*Item, error) {
	var (
		it                        Item
		seq                       int64
		authors, ki, cl, ev, lim  string
		oq, tags                  string
		pub, fetched              int64
		enriched                  int64
		themeID                   sql.NullInt64
		themeTitle                sql.NullString
	)
	err := rows.Scan(&seq, &it.ID, &it.Source, &it.SourceType, &it.Title, &it.URL,
		&authors, &pub, &fetched, &it.RawText, &it.Points, &enriched,
		&it.OneLiner, &it.Summary, &ki, &cl, &ev, &lim, &oq, &tags,
		&it.Relevance, &it.Novelty, &it.Importance, &it.Score, &it.State, &it.Feedback,
		&themeID, &themeTitle)
	if err != nil {
		return nil, err
	}
	it.Authors = parseArr(authors)
	it.KeyIdeas = parseArr(ki)
	it.Claims = parseArr(cl)
	it.Evidence = parseArr(ev)
	it.Limitations = parseArr(lim)
	it.OpenQuestions = parseArr(oq)
	it.Tags = parseArr(tags)
	it.PublishedAt = fromUnix(pub)
	it.FetchedAt = fromUnix(fetched)
	it.Enriched = enriched != 0
	if themeID.Valid {
		it.ThemeID = &themeID.Int64
	}
	if themeTitle.Valid {
		t := themeTitle.String
		it.ThemeTitle = &t
	}
	return &it, nil
}

// ---------- ingest ----------

// Upsert inserts a new item, ignoring items already stored (matched by ID).
// Returns true if the item was newly inserted.
func (s *Store) Upsert(it *Item) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO items
		(id,source,source_type,title,url,authors,published_at,fetched_at,raw_text,points)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		it.ID, it.Source, it.SourceType, it.Title, it.URL, jsonArr(it.Authors),
		unix(it.PublishedAt), unix(it.FetchedAt), it.RawText, it.Points)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	// Seed FTS with title + raw text so search works pre-enrichment.
	var seq int64
	if err := s.db.QueryRow(`SELECT seq FROM items WHERE id=?`, it.ID).Scan(&seq); err == nil {
		s.db.Exec(`INSERT INTO items_fts(rowid,title,body,tags) VALUES(?,?,?,?)`,
			seq, it.Title, it.RawText, "")
	}
	return true, nil
}

// ---------- enrichment ----------

// PendingEnrichment returns unenriched, non-dismissed items, newest first.
func (s *Store) PendingEnrichment(limit int) ([]*Item, error) {
	rows, err := s.db.Query(selectBase+` WHERE i.enriched=0 AND i.state!='dismissed' ORDER BY i.published_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// SaveEnrichment persists the LLM-derived fields and refreshes the FTS row.
func (s *Store) SaveEnrichment(it *Item) error {
	_, err := s.db.Exec(`UPDATE items SET
		enriched=1, one_liner=?, summary=?, key_ideas=?, claims=?, evidence=?,
		limitations=?, open_questions=?, tags=?, relevance=?, novelty=?, importance=?, score=?
		WHERE id=?`,
		it.OneLiner, it.Summary, jsonArr(it.KeyIdeas), jsonArr(it.Claims), jsonArr(it.Evidence),
		jsonArr(it.Limitations), jsonArr(it.OpenQuestions), jsonArr(it.Tags),
		it.Relevance, it.Novelty, it.Importance, it.Score, it.ID)
	if err != nil {
		return err
	}
	var seq int64
	if err := s.db.QueryRow(`SELECT seq FROM items WHERE id=?`, it.ID).Scan(&seq); err != nil {
		return err
	}
	body := it.Summary + "\n" + strings.Join(it.KeyIdeas, "\n") + "\n" + strings.Join(it.Claims, "\n")
	s.db.Exec(`DELETE FROM items_fts WHERE rowid=?`, seq)
	_, err = s.db.Exec(`INSERT INTO items_fts(rowid,title,body,tags) VALUES(?,?,?,?)`,
		seq, it.Title, body, strings.Join(it.Tags, " "))
	return err
}

// ---------- feed / reads ----------

type FeedOpts struct {
	State      string // "" = all non-dismissed; or explicit unread/read/saved
	SavedOnly  bool
	SourceType string
	ThemeID    int64 // 0 = any
	Query      string
	Limit      int
	Offset     int
}

// Feed returns ranked items. Ranking (recency-adjusted score) is computed in Go
// so the logic lives in one place; candidate set is capped for a personal-scale DB.
func (s *Store) Feed(o FeedOpts) ([]*Item, error) {
	if o.Query != "" {
		return s.Search(o.Query, orDefault(o.Limit, 50))
	}
	var (
		where []string
		args  []any
	)
	if o.SavedOnly {
		where = append(where, "i.state='saved'")
	} else if o.State != "" {
		where = append(where, "i.state=?")
		args = append(args, o.State)
	} else {
		where = append(where, "i.state!='dismissed'")
	}
	if o.SourceType != "" {
		where = append(where, "i.source_type=?")
		args = append(args, o.SourceType)
	}
	if o.ThemeID != 0 {
		where = append(where, "i.theme_id=?")
		args = append(args, o.ThemeID)
	}
	q := selectBase + " WHERE " + strings.Join(where, " AND ") + " ORDER BY i.published_at DESC LIMIT 800"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := collect(rows)
	if err != nil {
		return nil, err
	}
	Rank(items)
	// Apply offset/limit in Go after ranking.
	lo := o.Offset
	if lo > len(items) {
		lo = len(items)
	}
	hi := len(items)
	if lim := orDefault(o.Limit, 50); lo+lim < hi {
		hi = lo + lim
	}
	return items[lo:hi], nil
}

func (s *Store) Get(id string) (*Item, error) {
	rows, err := s.db.Query(selectBase+` WHERE i.id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := collect(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, sql.ErrNoRows
	}
	return items[0], nil
}

// Search runs an FTS5 query and returns ranked matches.
func (s *Store) Search(query string, limit int) ([]*Item, error) {
	match := ftsQuery(query)
	if match == "" {
		return []*Item{}, nil
	}
	rows, err := s.db.Query(selectBase+
		` JOIN items_fts f ON f.rowid=i.seq WHERE items_fts MATCH ? AND i.state!='dismissed' ORDER BY bm25(items_fts) LIMIT ?`,
		match, orDefault(limit, 50))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

func (s *Store) SetState(id, state string) error {
	_, err := s.db.Exec(`UPDATE items SET state=? WHERE id=?`, state, id)
	return err
}

func (s *Store) SetFeedback(id string, fb int) error {
	_, err := s.db.Exec(`UPDATE items SET feedback=? WHERE id=?`, fb, id)
	return err
}

// ---------- themes ----------

func (s *Store) Themes() ([]*Theme, error) {
	rows, err := s.db.Query(`
		SELECT t.id,t.title,t.description,t.connections,t.disagreements,t.trajectory,t.updated_at,
		       (SELECT COUNT(*) FROM items i WHERE i.theme_id=t.id) AS cnt
		FROM themes t ORDER BY cnt DESC, t.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Theme
	for rows.Next() {
		var t Theme
		var updated int64
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &t.Connections, &t.Disagreements, &t.Trajectory, &updated, &t.ItemCount); err != nil {
			return nil, err
		}
		t.UpdatedAt = fromUnix(updated)
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *Store) ThemeWithItems(id int64) (*Theme, error) {
	var t Theme
	var updated int64
	err := s.db.QueryRow(`SELECT id,title,description,connections,disagreements,trajectory,updated_at FROM themes WHERE id=?`, id).
		Scan(&t.ID, &t.Title, &t.Description, &t.Connections, &t.Disagreements, &t.Trajectory, &updated)
	if err != nil {
		return nil, err
	}
	t.UpdatedAt = fromUnix(updated)
	items, err := s.Feed(FeedOpts{ThemeID: id, Limit: 200})
	if err != nil {
		return nil, err
	}
	t.Items = items
	t.ItemCount = len(items)
	return &t, nil
}

// ThemeInput is one cluster produced by the theme-synthesis LLM pass.
type ThemeInput struct {
	Title         string
	Description   string
	Connections   string
	Disagreements string
	Trajectory    string
	ItemIDs       []string
}

// ReplaceThemes atomically clears existing themes and installs a fresh set,
// assigning each item to its first-listed (primary) theme.
func (s *Store) ReplaceThemes(themes []ThemeInput) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE items SET theme_id=NULL`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM themes`); err != nil {
		return err
	}
	now := time.Now().Unix()
	assigned := map[string]bool{}
	for _, th := range themes {
		res, err := tx.Exec(`INSERT INTO themes(title,description,connections,disagreements,trajectory,updated_at) VALUES(?,?,?,?,?,?)`,
			th.Title, th.Description, th.Connections, th.Disagreements, th.Trajectory, now)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		for _, itemID := range th.ItemIDs {
			if assigned[itemID] {
				continue // primary theme wins
			}
			if _, err := tx.Exec(`UPDATE items SET theme_id=? WHERE id=?`, id, itemID); err != nil {
				return err
			}
			assigned[itemID] = true
		}
	}
	return tx.Commit()
}

// RecentEnriched returns compact enriched items for the theme-clustering pass.
func (s *Store) RecentEnriched(limit int) ([]*Item, error) {
	rows, err := s.db.Query(selectBase+` WHERE i.enriched=1 AND i.state!='dismissed' ORDER BY i.published_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// ---------- stats ----------

type Stats struct {
	Total    int `json:"total"`
	Enriched int `json:"enriched"`
	Unread   int `json:"unread"`
	Saved    int `json:"saved"`
	Themes   int `json:"themes"`
}

func (s *Store) Stats() (Stats, error) {
	var st Stats
	q := func(dst *int, sql string) error { return s.db.QueryRow(sql).Scan(dst) }
	if err := q(&st.Total, `SELECT COUNT(*) FROM items`); err != nil {
		return st, err
	}
	q(&st.Enriched, `SELECT COUNT(*) FROM items WHERE enriched=1`)
	q(&st.Unread, `SELECT COUNT(*) FROM items WHERE state='unread' AND enriched=1`)
	q(&st.Saved, `SELECT COUNT(*) FROM items WHERE state='saved'`)
	q(&st.Themes, `SELECT COUNT(*) FROM themes`)
	return st, nil
}

// ---------- internal ----------

func collect(rows *sql.Rows) ([]*Item, error) {
	out := []*Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func orDefault(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

// ftsQuery turns free text into a safe FTS5 MATCH expression: each alphanumeric
// token is quoted and OR-joined with a prefix match, so partial words hit.
func ftsQuery(q string) string {
	fields := strings.FieldsFunc(q, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	var terms []string
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		terms = append(terms, fmt.Sprintf(`"%s"*`, f))
	}
	return strings.Join(terms, " OR ")
}
