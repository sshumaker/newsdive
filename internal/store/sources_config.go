package store

import "encoding/json"

// SourcesConfig is the user-editable source configuration (managed from the
// settings UI). Built-in arXiv/HN queries and the default blog list are always
// active; this adds Bluesky accounts and extra RSS/Atom feeds on top.
type SourcesConfig struct {
	BlueskyHandles []string    `json:"bluesky_handles"`
	ExtraFeeds     []FeedEntry `json:"extra_feeds"`
}

type FeedEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

const sourcesKey = "sources_config"

// GetSourcesConfig returns the stored config (empty lists if unset).
func (s *Store) GetSourcesConfig() SourcesConfig {
	c := SourcesConfig{BlueskyHandles: []string{}, ExtraFeeds: []FeedEntry{}}
	if v, ok := s.GetSetting(sourcesKey); ok {
		_ = json.Unmarshal([]byte(v), &c)
	}
	if c.BlueskyHandles == nil {
		c.BlueskyHandles = []string{}
	}
	if c.ExtraFeeds == nil {
		c.ExtraFeeds = []FeedEntry{}
	}
	return c
}

// SetSourcesConfig persists the config after light normalization.
func (s *Store) SetSourcesConfig(c SourcesConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.SetSetting(sourcesKey, string(b))
}
