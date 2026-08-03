package sources

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"news/internal/store"
)

// hnQueries target the reader's topics on HN front-page-quality stories.
var hnQueries = []string{
	"LLM agent",
	"coding agent",
	"language model",
	"AI agents",
	"inference",
	"prompt engineering",
}

const hnMinPoints = 40

type hnResponse struct {
	Hits []hnHit `json:"hits"`
}

type hnHit struct {
	ObjectID  string `json:"objectID"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Author    string `json:"author"`
	Points    int    `json:"points"`
	CreatedAtI int64 `json:"created_at_i"`
	StoryText string `json:"story_text"`
}

// FetchHackerNews queries the Algolia HN API for recent, sufficiently-upvoted
// stories matching each topic.
func FetchHackerNews(perQuery int) ([]*store.Item, error) {
	if perQuery <= 0 {
		perQuery = 20
	}
	if perQuery > 30 {
		perQuery = 30
	}
	var out []*store.Item
	var firstErr error
	seen := map[string]bool{}

	for _, q := range hnQueries {
		endpoint := fmt.Sprintf(
			"https://hn.algolia.com/api/v1/search_by_date?query=%s&tags=story&numericFilters=points%%3E%d&hitsPerPage=%d",
			url.QueryEscape(q), hnMinPoints, perQuery)
		body, err := get(endpoint)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("hn %q: %w", q, err)
			}
			continue
		}
		var resp hnResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("hn parse %q: %w", q, err)
			}
			continue
		}
		for _, h := range resp.Hits {
			link := h.URL
			if link == "" {
				link = "https://news.ycombinator.com/item?id=" + h.ObjectID
			}
			if seen[link] {
				continue
			}
			seen[link] = true
			raw := h.StoryText
			if raw == "" {
				raw = h.Title
			}
			var authors []string
			if h.Author != "" {
				authors = []string{h.Author}
			}
			out = append(out, &store.Item{
				ID:          itemID(link),
				Source:      "Hacker News",
				SourceType:  TypeHN,
				Title:       clean(h.Title),
				URL:         link,
				Authors:     authors,
				PublishedAt: time.Unix(h.CreatedAtI, 0).UTC(),
				FetchedAt:   time.Now().UTC(),
				RawText:     clean(raw),
				Points:      h.Points,
			})
		}
	}
	return out, firstErr
}
