// Package sources fetches candidate items from arXiv, Hacker News, and a
// curated set of research blogs (RSS/Atom). Each fetcher returns store.Item
// values with a stable ID; the ingest layer dedupes and persists them.
package sources

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"

	"news/internal/store"
)

const (
	TypeArxiv = "arxiv"
	TypeHN    = "hackernews"
	TypeBlog  = "blog"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// get performs a GET with a descriptive User-Agent (arXiv/HN expect one).
func get(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "news-reader/1.0 (personal research aggregator)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// sanitizeXML strips control characters that are illegal in XML 1.0 (some feeds
// emit raw U+0008 etc.), which would otherwise abort the whole parse.
func sanitizeXML(b []byte) []byte {
	out := b[:0]
	for _, r := range string(b) {
		if r == '\t' || r == '\n' || r == '\r' || r >= 0x20 {
			out = append(out, []byte(string(r))...)
		}
	}
	return out
}

// itemID is a stable identifier derived from the canonical URL.
func itemID(url string) string {
	sum := sha256.Sum256([]byte(canonicalURL(url)))
	return hex.EncodeToString(sum[:])[:16]
}

func canonicalURL(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimSuffix(u, "/")
	return strings.ToLower(u)
}

// Result bundles fetched items with any per-source errors (so one failing
// source never aborts the whole ingest).
type Result struct {
	Items  []*store.Item
	Errors []error
}

// FetchAll pulls from every source. perSource caps how many each query returns.
// cfg adds user-configured Bluesky handles and extra RSS/Atom feeds on top of
// the built-in arXiv/HN/blog sources.
func FetchAll(perSource int, cfg store.SourcesConfig) Result {
	var r Result
	add := func(items []*store.Item, err error) {
		if err != nil {
			r.Errors = append(r.Errors, err)
		}
		r.Items = append(r.Items, items...)
	}
	add(FetchArxiv(perSource))
	add(FetchHackerNews(perSource))
	add(FetchBlogs(perSource, cfg.ExtraFeeds))
	if len(cfg.BlueskyHandles) > 0 {
		add(FetchBluesky(cfg.BlueskyHandles, perSource))
	}
	return r
}
