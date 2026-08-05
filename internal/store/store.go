// Package store is the SQLite persistence layer. It uses the pure-Go
// modernc.org/sqlite driver (no cgo) so the binary cross-compiles cleanly for
// Docker/Railway, and an FTS5 virtual table for keyword search and the ask
// agent's corpus lookups.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
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
  skeptics_take  TEXT NOT NULL DEFAULT '',
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

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- One chat thread per topic (an item, a theme, or the feed). Keyed by topic
-- rather than by session so a server restart resumes the Managed Agents session
-- instead of silently orphaning it and starting over.
CREATE TABLE IF NOT EXISTS conversations (
  topic         TEXT PRIMARY KEY,      -- "item:<id>" | "theme:<key>" | "feed"
  item_id       TEXT NOT NULL DEFAULT '',
  title         TEXT NOT NULL DEFAULT '',
  session_id    TEXT NOT NULL,
  turns         INTEGER NOT NULL DEFAULT 0,
  last_activity INTEGER NOT NULL DEFAULT 0,
  summary       TEXT NOT NULL DEFAULT '',
  summarized_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_conv_item ON conversations(item_id);
CREATE INDEX IF NOT EXISTS idx_conv_idle ON conversations(last_activity);
`)
	if err != nil {
		return err
	}
	return s.migrateThemeKeys()
}

// migrateThemeKeys adds the stable-key and starred columns to `themes`. Themes
// were previously rebuilt wholesale on every clustering run with AUTOINCREMENT
// ids, so a starred flag had nothing durable to hang on and bookmarked theme
// URLs pointed at a different theme after each run. The key is derived from the
// title, which is what actually identifies a theme across re-clusterings.
//
// CREATE TABLE IF NOT EXISTS can't add columns to an existing table, so these
// run as explicit idempotent ALTERs (duplicate-column errors are the expected
// no-op on an already-migrated database).
func (s *Store) migrateThemeKeys() error {
	for _, stmt := range []string{
		`ALTER TABLE themes ADD COLUMN key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE themes ADD COLUMN starred INTEGER NOT NULL DEFAULT 0`,
		// Items enriched before this column existed keep '' and simply render no
		// skeptic's take until they are re-enriched.
		`ALTER TABLE items ADD COLUMN skeptics_take TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate themes: %w", err)
		}
	}
	// Backfill keys for themes that predate the column, then enforce uniqueness.
	rows, err := s.db.Query(`SELECT id, title FROM themes WHERE key=''`)
	if err != nil {
		return err
	}
	type row struct {
		id    int64
		title string
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.title); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, r)
	}
	rows.Close()
	for _, r := range pending {
		if _, err := s.db.Exec(`UPDATE themes SET key=? WHERE id=?`, ThemeKey(r.title), r.id); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_themes_key ON themes(key)`)
	return err
}

// ThemeKey derives a theme's stable identity from its title. Re-clusterings
// reword titles slightly, so the key is aggressively normalized: lowercased,
// non-alphanumerics collapsed to single hyphens.
func ThemeKey(title string) string {
	var b strings.Builder
	lastHyphen := true // suppresses a leading hyphen
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// GetSetting returns a stored value and whether it was present.
func (s *Store) GetSetting(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

// SetSetting upserts a key/value setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
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

const selectCols = `i.seq,i.id,i.source,i.source_type,i.title,i.url,i.authors,i.published_at,i.fetched_at,i.raw_text,i.points,i.enriched,i.one_liner,i.summary,i.key_ideas,i.claims,i.evidence,i.limitations,i.skeptics_take,i.open_questions,i.tags,i.relevance,i.novelty,i.importance,i.score,i.state,i.feedback,i.theme_id,t.title`

const selectBase = `SELECT ` + selectCols + ` FROM items i LEFT JOIN themes t ON t.id=i.theme_id`

func scanItem(rows *sql.Rows) (*Item, error) {
	var (
		it                       Item
		seq                      int64
		authors, ki, cl, ev, lim string
		oq, tags                 string
		pub, fetched             int64
		enriched                 int64
		themeID                  sql.NullInt64
		themeTitle               sql.NullString
	)
	err := rows.Scan(&seq, &it.ID, &it.Source, &it.SourceType, &it.Title, &it.URL,
		&authors, &pub, &fetched, &it.RawText, &it.Points, &enriched,
		&it.OneLiner, &it.Summary, &ki, &cl, &ev, &lim, &it.SkepticsTake, &oq, &tags,
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
		limitations=?, skeptics_take=?, open_questions=?, tags=?, relevance=?, novelty=?, importance=?, score=?
		WHERE id=?`,
		it.OneLiner, it.Summary, jsonArr(it.KeyIdeas), jsonArr(it.Claims), jsonArr(it.Evidence),
		jsonArr(it.Limitations), it.SkepticsTake, jsonArr(it.OpenQuestions), jsonArr(it.Tags),
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
	// Rank here too: the stored score column is the enrichment-time base, with
	// no recency term. Without this the detail page shows a different (and for
	// older items much higher) number than the same item's row in the feed.
	Rank(items)
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
		SELECT t.id,t.key,t.starred,t.title,t.description,t.connections,t.disagreements,t.trajectory,t.updated_at,
		       (SELECT COUNT(*) FROM items i WHERE i.theme_id=t.id) AS cnt
		FROM themes t ORDER BY t.starred DESC, cnt DESC, t.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Theme
	for rows.Next() {
		var t Theme
		var updated int64
		if err := rows.Scan(&t.ID, &t.Key, &t.Starred, &t.Title, &t.Description, &t.Connections, &t.Disagreements, &t.Trajectory, &updated, &t.ItemCount); err != nil {
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
	err := s.db.QueryRow(`SELECT id,key,starred,title,description,connections,disagreements,trajectory,updated_at FROM themes WHERE id=?`, id).
		Scan(&t.ID, &t.Key, &t.Starred, &t.Title, &t.Description, &t.Connections, &t.Disagreements, &t.Trajectory, &updated)
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

// SuspectDated returns items whose publication date looks like it was stamped
// at fetch time rather than read from the source — the signature of an
// on-demand add that couldn't find a real date. Also catches missing and
// future dates.
func (s *Store) SuspectDated(limit int) ([]*Item, error) {
	rows, err := s.db.Query(selectBase+`
		WHERE i.published_at<=0
		   OR i.published_at > strftime('%s','now')+3600
		   OR abs(i.published_at - i.fetched_at) < 120
		ORDER BY i.fetched_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// SetPublishedAt corrects an item's publication date. Score is recomputed from
// it on every read, so nothing else needs updating.
func (s *Store) SetPublishedAt(id string, t time.Time) error {
	_, err := s.db.Exec(`UPDATE items SET published_at=? WHERE id=?`, t.Unix(), id)
	return err
}

// SetItemTheme files a single item under a theme without re-clustering.
func (s *Store) SetItemTheme(itemID string, themeID int64) error {
	_, err := s.db.Exec(`UPDATE items SET theme_id=? WHERE id=?`, themeID, itemID)
	return err
}

// StarredThemes returns the themes the reader has pinned, which survive
// re-clustering. Used to tell the clusterer which themes it must keep.
func (s *Store) StarredThemes() ([]*Theme, error) {
	rows, err := s.db.Query(`SELECT id, key, title, description, connections, disagreements, trajectory, updated_at
		FROM themes WHERE starred=1 ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Theme{}
	for rows.Next() {
		var t Theme
		var updated int64
		if err := rows.Scan(&t.ID, &t.Key, &t.Title, &t.Description, &t.Connections,
			&t.Disagreements, &t.Trajectory, &updated); err != nil {
			return nil, err
		}
		t.UpdatedAt = time.Unix(updated, 0)
		t.Starred = true
		out = append(out, t2p(t))
	}
	return out, rows.Err()
}

func t2p(t Theme) *Theme { return &t }

// SetThemeStarred pins or unpins a theme. A pinned theme keeps its identity
// across clustering runs; see SyncThemes.
func (s *Store) SetThemeStarred(id int64, starred bool) error {
	v := 0
	if starred {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE themes SET starred=? WHERE id=?`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SyncThemes installs a freshly clustered set of themes, preserving the identity
// of starred ones.
//
// This replaces a wholesale DELETE + re-INSERT. Themes are matched by their
// stable key (derived from the title), so a starred theme keeps its row id —
// bookmarked URLs and the star itself survive — while absorbing whatever items
// the new clustering assigned to it and taking the regenerated synthesis text.
// Unstarred themes are still rebuilt from scratch each run.
func (s *Store) SyncThemes(themes []ThemeInput) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Item→theme assignment is recomputed from scratch every run, including for
	// starred themes: "keep the theme" means keep its identity and let current
	// items join it, not freeze last run's membership.
	if _, err := tx.Exec(`UPDATE items SET theme_id=NULL`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM themes WHERE starred=0`); err != nil {
		return err
	}

	now := time.Now().Unix()
	assigned := map[string]bool{}
	seen := map[string]bool{}
	for _, th := range themes {
		key := ThemeKey(th.Title)
		if key == "" || seen[key] {
			continue // a title that normalizes to nothing, or a duplicate this run
		}
		seen[key] = true

		// Upsert on the stable key: updates a surviving starred row in place,
		// inserts otherwise. Starred is never written here, so pinning sticks.
		if _, err := tx.Exec(`
			INSERT INTO themes(key,title,description,connections,disagreements,trajectory,updated_at)
			VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET
				title=excluded.title, description=excluded.description,
				connections=excluded.connections, disagreements=excluded.disagreements,
				trajectory=excluded.trajectory, updated_at=excluded.updated_at`,
			key, th.Title, th.Description, th.Connections, th.Disagreements, th.Trajectory, now); err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRow(`SELECT id FROM themes WHERE key=?`, key).Scan(&id); err != nil {
			return err
		}
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

	// A starred theme the clusterer didn't return this run keeps its row (the
	// reader asked for it) but ends up with no items until one matches again.
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

// ---------- conversations ----------

// Conversation is one chat thread about a topic. The transcript itself lives in
// the Managed Agents session (retrievable via its event list); this row is the
// durable pointer to it plus the rolled-up summary.
type Conversation struct {
	Topic        string    `json:"topic"`
	ItemID       string    `json:"item_id,omitempty"`
	Title        string    `json:"title,omitempty"`
	SessionID    string    `json:"session_id"`
	Turns        int       `json:"turns"`
	LastActivity time.Time `json:"last_activity"`
	Summary      string    `json:"summary,omitempty"`
	SummarizedAt time.Time `json:"summarized_at,omitempty"`
}

// UpsertConversation records (or refreshes) the session backing a topic and
// bumps its activity clock. Writing the summary to '' on a new turn is
// deliberate: the existing digest is stale the moment the reader says more.
func (s *Store) UpsertConversation(topic, itemID, title, sessionID string) error {
	_, err := s.db.Exec(`
		INSERT INTO conversations(topic,item_id,title,session_id,turns,last_activity)
		VALUES(?,?,?,?,1,?)
		ON CONFLICT(topic) DO UPDATE SET
			session_id=excluded.session_id,
			item_id=excluded.item_id,
			title=excluded.title,
			turns=conversations.turns+1,
			last_activity=excluded.last_activity,
			summary='', summarized_at=0`,
		topic, itemID, title, sessionID, time.Now().Unix())
	return err
}

// ConversationSession returns the session id previously used for a topic, so a
// restarted server resumes the thread instead of orphaning it.
func (s *Store) ConversationSession(topic string) (string, bool) {
	var id string
	if err := s.db.QueryRow(`SELECT session_id FROM conversations WHERE topic=?`, topic).Scan(&id); err != nil {
		return "", false
	}
	return id, id != ""
}

// ConversationForItem returns the chat digest attached to an item, if any.
func (s *Store) ConversationForItem(itemID string) (*Conversation, error) {
	c, err := s.scanConversation(s.db.QueryRow(`
		SELECT topic,item_id,title,session_id,turns,last_activity,summary,summarized_at
		FROM conversations WHERE item_id=? AND summary!=''`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// ConversationForTopic returns the digest for any topic key (an item, theme, or
// the feed), if one has been written.
func (s *Store) ConversationForTopic(topic string) (*Conversation, error) {
	c, err := s.scanConversation(s.db.QueryRow(`
		SELECT topic,item_id,title,session_id,turns,last_activity,summary,summarized_at
		FROM conversations WHERE topic=? AND summary!=''`, topic))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// StaleConversations returns conversations that have been idle at least
// idleFor and have no current summary — the work list for the digest sweep.
func (s *Store) StaleConversations(idleFor time.Duration, limit int) ([]*Conversation, error) {
	cutoff := time.Now().Add(-idleFor).Unix()
	rows, err := s.db.Query(`
		SELECT topic,item_id,title,session_id,turns,last_activity,summary,summarized_at
		FROM conversations
		WHERE summary='' AND last_activity>0 AND last_activity<=? AND turns>0
		ORDER BY last_activity LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Conversation{}
	for rows.Next() {
		c, err := s.scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveConversationSummary stores a digest. It is a no-op if the conversation has
// moved on since the sweep read it (last_activity changed), so a slow summary
// can't overwrite a fresher exchange with a stale digest.
func (s *Store) SaveConversationSummary(topic, summary string, readAt time.Time) error {
	_, err := s.db.Exec(`UPDATE conversations SET summary=?, summarized_at=? WHERE topic=? AND last_activity=?`,
		summary, time.Now().Unix(), topic, readAt.Unix())
	return err
}

type scanner interface{ Scan(...any) error }

func (s *Store) scanConversation(row scanner) (*Conversation, error) {
	var c Conversation
	var last, summed int64
	if err := row.Scan(&c.Topic, &c.ItemID, &c.Title, &c.SessionID, &c.Turns, &last, &c.Summary, &summed); err != nil {
		return nil, err
	}
	c.LastActivity = fromUnix(last)
	c.SummarizedAt = fromUnix(summed)
	return &c, nil
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
