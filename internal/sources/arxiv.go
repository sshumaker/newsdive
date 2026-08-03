package sources

import (
	"encoding/xml"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"news/internal/store"
)

// arxivQueries are topical searches over abstracts/titles. Keeping them
// keyword-targeted (rather than dumping whole categories) means the ingested
// set is already on-topic, so enrichment spend goes to relevant work.
var arxivQueries = []string{
	`abs:"language model" AND abs:agent`,
	`abs:"LLM agent" OR abs:"agentic"`,
	`abs:"tool use" AND abs:"language model"`,
	`abs:"inference-time" OR abs:"test-time compute"`,
	`abs:"program synthesis" OR abs:"code generation"`,
	`abs:"coding agent" OR abs:"software engineering agent"`,
	`abs:"chain-of-thought" OR abs:"reasoning" AND abs:"large language model"`,
	`abs:"multi-agent" AND abs:"language model"`,
}

var arxivVersionRe = regexp.MustCompile(`v\d+$`)

type arxivFeed struct {
	Entries []arxivEntry `xml:"entry"`
}

type arxivEntry struct {
	ID        string        `xml:"id"`
	Title     string        `xml:"title"`
	Summary   string        `xml:"summary"`
	Published string        `xml:"published"`
	Updated   string        `xml:"updated"`
	Authors   []arxivAuthor `xml:"author"`
	Links     []arxivLink   `xml:"link"`
}

type arxivAuthor struct {
	Name string `xml:"name"`
}

type arxivLink struct {
	Href string `xml:"href"`
	Rel  string `xml:"rel"`
	Type string `xml:"type"`
}

// FetchArxiv runs each topical query and merges results (deduped downstream by ID).
func FetchArxiv(perQuery int) ([]*store.Item, error) {
	if perQuery <= 0 {
		perQuery = 20
	}
	// Cap per query so a broad ingest doesn't pull hundreds of papers to enrich.
	if perQuery > 25 {
		perQuery = 25
	}
	var out []*store.Item
	var firstErr error
	seen := map[string]bool{}

	for i, q := range arxivQueries {
		if i > 0 {
			time.Sleep(3 * time.Second) // arXiv asks for ~3s between requests
		}
		endpoint := fmt.Sprintf(
			"http://export.arxiv.org/api/query?search_query=%s&sortBy=submittedDate&sortOrder=descending&max_results=%d",
			url.QueryEscape(q), perQuery)
		body, err := get(endpoint)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("arxiv %q: %w", q, err)
			}
			continue
		}
		var feed arxivFeed
		if err := xml.Unmarshal(sanitizeXML(body), &feed); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("arxiv parse %q: %w", q, err)
			}
			continue
		}
		for _, e := range feed.Entries {
			absURL := absURL(e)
			if absURL == "" || seen[absURL] {
				continue
			}
			seen[absURL] = true
			var authors []string
			for _, a := range e.Authors {
				if n := strings.TrimSpace(a.Name); n != "" {
					authors = append(authors, n)
				}
			}
			out = append(out, &store.Item{
				ID:          itemID(absURL),
				Source:      "arXiv",
				SourceType:  TypeArxiv,
				Title:       clean(e.Title),
				URL:         absURL,
				Authors:     authors,
				PublishedAt: parseTime(e.Published, e.Updated),
				FetchedAt:   time.Now().UTC(),
				RawText:     clean(e.Summary),
			})
		}
	}
	return out, firstErr
}

// absURL returns the canonical version-stripped abstract URL for an entry.
func absURL(e arxivEntry) string {
	raw := strings.TrimSpace(e.ID)
	for _, l := range e.Links {
		if l.Rel == "alternate" && strings.Contains(l.Href, "/abs/") {
			raw = l.Href
			break
		}
	}
	raw = strings.Replace(raw, "http://", "https://", 1)
	if idx := strings.Index(raw, "/abs/"); idx >= 0 {
		id := raw[idx+len("/abs/"):]
		id = arxivVersionRe.ReplaceAllString(id, "")
		return "https://arxiv.org/abs/" + id
	}
	return raw
}

func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func parseTime(candidates ...string) time.Time {
	layouts := []string{time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05Z"}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		for _, l := range layouts {
			if t, err := time.Parse(l, c); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}
