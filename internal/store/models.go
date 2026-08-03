package store

import "time"

// Item is one ingested piece of work (a paper, post, or story) plus the
// LLM-derived enrichment layered on top of it.
type Item struct {
	ID          string    `json:"id"` // stable hash of the canonical URL
	Source      string    `json:"source"`
	SourceType  string    `json:"source_type"` // arxiv | hackernews | blog
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Authors     []string  `json:"authors"`
	PublishedAt time.Time `json:"published_at"`
	FetchedAt   time.Time `json:"fetched_at"`
	RawText     string    `json:"raw_text"` // abstract / excerpt as ingested

	// Signals carried from the source, used as ranking priors.
	Points int `json:"points"` // HN points, or 0

	// Enrichment (populated once the item is summarized).
	Enriched      bool     `json:"enriched"`
	OneLiner      string   `json:"one_liner"`
	Summary       string   `json:"summary"`
	KeyIdeas      []string `json:"key_ideas"`
	Claims        []string `json:"claims"`
	Evidence      []string `json:"evidence"`
	Limitations   []string `json:"limitations"`
	OpenQuestions []string `json:"open_questions"`
	Tags          []string `json:"tags"`

	Relevance  int     `json:"relevance"`  // 0-100, to reader interests
	Novelty    int     `json:"novelty"`    // 0-100
	Importance int     `json:"importance"` // 0-100
	Score      float64 `json:"score"`      // computed overall rank (recency-adjusted)

	// Reader state.
	State    string `json:"state"`    // unread | read | saved | dismissed
	Feedback int    `json:"feedback"` // -1, 0, +1

	ThemeID    *int64  `json:"theme_id,omitempty"`
	ThemeTitle *string `json:"theme_title,omitempty"`
}

// Theme is a cluster of related items with cross-item synthesis: what connects
// them, where they disagree, and how the idea is trending.
type Theme struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Connections  string    `json:"connections"`
	Disagreements string   `json:"disagreements"`
	Trajectory   string    `json:"trajectory"`
	ItemCount    int       `json:"item_count"`
	UpdatedAt    time.Time `json:"updated_at"`
	Items        []*Item   `json:"items,omitempty"`
}
