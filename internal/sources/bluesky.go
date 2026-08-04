package sources

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"news/internal/store"
)

// Bluesky is fetched via the public AppView API — no auth or API key required
// for public posts. Handles are configured by the user in settings.
const bskyAPI = "https://public.api.bsky.app/xrpc/app.bsky.feed.getAuthorFeed"

type bskyFeed struct {
	Feed []struct {
		Post   bskyPost       `json:"post"`
		Reason map[string]any `json:"reason"` // present on reposts
	} `json:"feed"`
}

type bskyPost struct {
	URI    string `json:"uri"`
	Author struct {
		Handle      string `json:"handle"`
		DisplayName string `json:"displayName"`
	} `json:"author"`
	Record struct {
		Text      string         `json:"text"`
		CreatedAt string         `json:"createdAt"`
		Reply     map[string]any `json:"reply"` // present on replies
		Embed     struct {
			External struct {
				URI   string `json:"uri"`
				Title string `json:"title"`
			} `json:"external"`
		} `json:"embed"`
	} `json:"record"`
}

// FetchBluesky pulls recent top-level posts (no replies, no reposts) for each
// configured handle.
func FetchBluesky(handles []string, perHandle int) ([]*store.Item, error) {
	if perHandle <= 0 {
		perHandle = 20
	}
	if perHandle > 40 {
		perHandle = 40
	}
	var out []*store.Item
	var firstErr error

	for _, raw := range handles {
		handle := normalizeHandle(raw)
		if handle == "" {
			continue
		}
		endpoint := fmt.Sprintf("%s?actor=%s&limit=%d&filter=posts_no_replies",
			bskyAPI, url.QueryEscape(handle), perHandle)
		body, err := get(endpoint)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("bluesky %s: %w", handle, err)
			}
			continue
		}
		var feed bskyFeed
		if err := json.Unmarshal(body, &feed); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("bluesky parse %s: %w", handle, err)
			}
			continue
		}
		for _, fi := range feed.Feed {
			if fi.Reason != nil || fi.Post.Record.Reply != nil {
				continue // skip reposts and replies
			}
			p := fi.Post
			text := strings.TrimSpace(p.Record.Text)
			if text == "" {
				continue
			}
			link := postURL(handle, p.URI)
			raw := text
			if ext := p.Record.Embed.External.URI; ext != "" {
				raw = text + "\n\nLinked: " + ext
			}
			author := p.Author.DisplayName
			if author == "" {
				author = handle
			}
			out = append(out, &store.Item{
				ID:          itemID(link),
				Source:      "Bluesky @" + handle,
				SourceType:  "bluesky",
				Title:       firstLine(text, 110),
				URL:         link,
				Authors:     []string{author},
				PublishedAt: parseTime(p.Record.CreatedAt),
				FetchedAt:   time.Now().UTC(),
				RawText:     raw,
			})
		}
	}
	return out, firstErr
}

func normalizeHandle(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "@")
	// Accept a pasted profile URL too.
	if i := strings.Index(h, "bsky.app/profile/"); i >= 0 {
		h = h[i+len("bsky.app/profile/"):]
		h = strings.SplitN(h, "/", 2)[0]
	}
	return strings.ToLower(h)
}

func postURL(handle, uri string) string {
	rkey := uri
	if i := strings.LastIndex(uri, "/"); i >= 0 {
		rkey = uri[i+1:]
	}
	return fmt.Sprintf("https://bsky.app/profile/%s/post/%s", handle, rkey)
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
