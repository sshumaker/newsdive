package sources

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"news/internal/store"
)

// TypeManual marks items the reader added by hand (or via the chat agent's
// add_article tool) rather than pulled from a configured source.
const TypeManual = "manual"

var (
	titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	// og:description / <meta name="description">, attribute order either way.
	descRe = regexp.MustCompile(`(?is)<meta[^>]+(?:name|property)=["'](?:og:)?description["'][^>]+content=["']([^"']*)["']`)
	descRe2 = regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']*)["'][^>]+(?:name|property)=["'](?:og:)?description["']`)
	// Script/style bodies would otherwise dominate the extracted text. Listed
	// per-tag rather than with a backreference: Go's RE2 has no backreferences.
	dropRes = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</\s*script\s*>`),
		regexp.MustCompile(`(?is)<style\b[^>]*>.*?</\s*style\s*>`),
		regexp.MustCompile(`(?is)<nav\b[^>]*>.*?</\s*nav\s*>`),
		regexp.MustCompile(`(?is)<footer\b[^>]*>.*?</\s*footer\s*>`),
		regexp.MustCompile(`(?is)<svg\b[^>]*>.*?</\s*svg\s*>`),
	}
)

// FetchURL retrieves a single page and builds an unenriched item from it, for
// adding an article on demand. Extraction is deliberately crude — title, meta
// description, and a text excerpt — because the enrichment pass is what actually
// reads the content, and it tolerates a thin excerpt (falling back to reasoning
// from the title).
//
// arXiv abstract pages get their /abs/ form preferred so the id matches anything
// the arXiv source would later ingest, rather than creating a duplicate item.
func FetchURL(raw string) (*store.Item, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty url")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("not a valid url: %s", raw)
	}
	// Normalize arXiv PDF links to the abstract page so the derived id lines up
	// with the arXiv fetcher's.
	if strings.Contains(u.Host, "arxiv.org") {
		u.Path = strings.TrimSuffix(strings.Replace(u.Path, "/pdf/", "/abs/", 1), ".pdf")
	}
	page := u.String()

	body, err := get(page)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", page, err)
	}
	html := string(body)

	title := ""
	if m := titleRe.FindStringSubmatch(html); len(m) > 1 {
		title = strings.TrimSpace(stripHTML(m[1]))
	}
	if title == "" {
		title = u.Host + u.Path
	}

	desc := ""
	for _, re := range []*regexp.Regexp{descRe, descRe2} {
		if m := re.FindStringSubmatch(html); len(m) > 1 {
			desc = strings.TrimSpace(stripHTML(m[1]))
			break
		}
	}
	stripped := html
	for _, re := range dropRes {
		stripped = re.ReplaceAllString(stripped, " ")
	}
	text := strings.TrimSpace(stripHTML(stripped))
	if desc != "" {
		text = desc + "\n\n" + text
	}

	srcType := TypeManual
	if strings.Contains(u.Host, "arxiv.org") {
		srcType = TypeArxiv
	}

	now := time.Now()
	return &store.Item{
		ID:          itemID(page),
		Source:      u.Host,
		SourceType:  srcType,
		Title:       title,
		URL:         page,
		PublishedAt: now, // the page rarely states one reliably; treat as "now"
		FetchedAt:   now,
		RawText:     excerpt(text, 6000),
	}, nil
}
