package sources

import (
	"fmt"
	"net/url"
	"strconv"
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
	// Publication date, most authoritative first. arXiv abstract pages carry
	// citation_date; most blogs/news carry the OpenGraph or JSON-LD variants.
	// citation_online_date is deliberately last: on arXiv it is the date of the
	// latest revision, not of the paper.
	dateRes = []*regexp.Regexp{
		metaRe("citation_date"),
		metaRe("citation_publication_date"),
		metaRe("article:published_time"),
		metaRe("datePublished"),
		metaRe("date"),
		metaRe("citation_online_date"),
		regexp.MustCompile(`(?is)"datePublished"\s*:\s*"([^"]+)"`),         // JSON-LD
		regexp.MustCompile(`(?is)<time[^>]+datetime=["']([^"']+)["']`),     // <time datetime>
	}

	// Last resort, after the URL-path date: these describe the last revision,
	// not publication, so they only run when nothing better exists.
	staleDateRes = []*regexp.Regexp{
		metaRe("updated_time"),
		metaRe("dateModified"),
	}

	// Dated URL paths — /2025/Jan/20/, /2025/01/20/, /posts/2023-06-23-agent/.
	// Very common on blogs and often the only date in the markup.
	urlDateRes = []*regexp.Regexp{
		regexp.MustCompile(`/(\d{4})[/-](\d{1,2})[/-](\d{1,2})(?:[/-]|$)`),
		regexp.MustCompile(`/(\d{4})/([A-Za-z]{3,9})/(\d{1,2})(?:/|$)`),
	}

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

// metaRe matches <meta name|property="<key>" content="..."> with the attributes
// in either order, which real pages emit both ways.
func metaRe(key string) *regexp.Regexp {
	k := regexp.QuoteMeta(key)
	return regexp.MustCompile(`(?is)<meta[^>]+(?:name|property)=["'](?:og:|article:)?` + k +
		`["'][^>]+content=["']([^"']+)["']|<meta[^>]+content=["']([^"']+)["'][^>]+(?:name|property)=["'](?:og:|article:)?` + k + `["']`)
}

// parseArticleDate accepts the handful of formats publishers actually use.
// Returns the zero time when nothing parses, which callers must treat as
// "unknown" rather than "now" — see FetchURL.
func parseArticleDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	// Some pages (og:updated_time) give a Unix timestamp rather than a date.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 946_684_800 && n < 4_102_444_800 {
		return time.Unix(n, 0).UTC()
	}
	layouts := []string{
		time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05",
		"2006/01/02", "2006-01-02", "02 Jan 2006", "2 January 2006",
		"January 2, 2006", "Jan 2, 2006",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// urlDate reads a date out of a dated URL path.
func urlDate(path string) time.Time {
	for _, re := range urlDateRes {
		m := re.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		for _, layout := range []string{"2006-1-2", "2006-Jan-2", "2006-January-2"} {
			if t, err := time.Parse(layout, m[1]+"-"+m[2]+"-"+m[3]); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// articleDate pulls the publication date out of a page, preferring explicit
// publication metadata, then a dated URL path, then last-modified signals.
func articleDate(html, path string) time.Time {
	if t := scanDates(html, dateRes); !t.IsZero() {
		return t
	}
	if t := urlDate(path); !t.IsZero() {
		return t
	}
	return scanDates(html, staleDateRes)
}

func scanDates(html string, res []*regexp.Regexp) time.Time {
	for _, re := range res {
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			for _, g := range m[1:] {
				if t := parseArticleDate(g); !t.IsZero() {
					return t
				}
			}
		}
	}
	return time.Time{}
}

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

	// The article's own publication date, never the fetch time. Stamping "now"
	// makes a year-old paper read as fresh and — because ranking is
	// recency-weighted and clustering takes the newest N — pushes it to the top
	// of the feed and displaces genuinely recent work. Left zero when the page
	// says nothing; Rank treats unknown as old rather than brand new.
	published := articleDate(html, u.Path)
	if published.After(time.Now()) {
		published = time.Time{} // a future date is metadata junk, not news
	}
	return &store.Item{
		ID:          itemID(page),
		Source:      u.Host,
		SourceType:  srcType,
		Title:       title,
		URL:         page,
		PublishedAt: published,
		FetchedAt:   time.Now(),
		RawText:     excerpt(text, 6000),
	}, nil
}
