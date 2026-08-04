package sources

import (
	"encoding/xml"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"news/internal/store"
)

// blogFeeds is a curated set of research blogs (RSS or Atom). These are
// generally on-topic, so items are ingested wholesale and left for the
// relevance scorer to rank. Edit to taste.
var blogFeeds = []struct{ Name, URL string }{
	{"Simon Willison", "https://simonwillison.net/atom/everything/"},
	{"Lil'Log", "https://lilianweng.github.io/index.xml"},
	{"BAIR Blog", "https://bair.berkeley.edu/blog/feed.xml"},
	{"Hugging Face", "https://huggingface.co/blog/feed.xml"},
	{"The Gradient", "https://thegradient.pub/rss/"},
	{"Answer.AI", "https://www.answer.ai/index.xml"},
	{"Eugene Yan", "https://eugeneyan.com/rss/"},
	{"Interconnects", "https://www.interconnects.ai/feed"},
	{"Sebastian Raschka", "https://magazine.sebastianraschka.com/feed"},
}

type feedDoc struct {
	RSSItems   []rssItem   `xml:"channel>item"`
	AtomEnturs []atomEntry `xml:"entry"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	Creator     string `xml:"creator"` // dc:creator
}

type atomEntry struct {
	Title     string     `xml:"title"`
	Links     []atomLink `xml:"link"`
	Summary   string     `xml:"summary"`
	Content   string     `xml:"content"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
	Author    struct {
		Name string `xml:"name"`
	} `xml:"author"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

var tagRe = regexp.MustCompile(`<[^>]+>`)

// FetchBlogs pulls the newest entries from the built-in feeds plus any
// user-configured extra feeds.
func FetchBlogs(perFeed int, extra []store.FeedEntry) ([]*store.Item, error) {
	if perFeed <= 0 {
		perFeed = 15
	}
	if perFeed > 20 {
		perFeed = 20
	}
	var out []*store.Item
	var firstErr error

	feeds := make([]struct{ Name, URL string }, 0, len(blogFeeds)+len(extra))
	feeds = append(feeds, blogFeeds...)
	for _, e := range extra {
		if strings.TrimSpace(e.URL) != "" {
			feeds = append(feeds, struct{ Name, URL string }{Name: orName(e.Name, e.URL), URL: e.URL})
		}
	}

	for _, f := range feeds {
		body, err := get(f.URL)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("blog %s: %w", f.Name, err)
			}
			continue
		}
		var doc feedDoc
		if err := xml.Unmarshal(sanitizeXML(body), &doc); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("blog parse %s: %w", f.Name, err)
			}
			continue
		}

		count := 0
		emit := func(title, link, text, author string, published time.Time) {
			if count >= perFeed || link == "" || title == "" {
				return
			}
			count++
			var authors []string
			if author != "" {
				authors = []string{author}
			}
			out = append(out, &store.Item{
				ID:          itemID(link),
				Source:      f.Name,
				SourceType:  TypeBlog,
				Title:       clean(title),
				URL:         strings.TrimSpace(link),
				Authors:     authors,
				PublishedAt: published,
				FetchedAt:   time.Now().UTC(),
				RawText:     excerpt(text, 1200),
			})
		}

		for _, it := range doc.RSSItems {
			emit(it.Title, it.Link, it.Description, it.Creator, parseFeedTime(it.PubDate))
		}
		for _, e := range doc.AtomEnturs {
			body := e.Summary
			if len(e.Content) > len(body) {
				body = e.Content
			}
			emit(e.Title, atomHref(e.Links), body, e.Author.Name, parseTime(e.Published, e.Updated))
		}
	}
	return out, firstErr
}

func atomHref(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "alternate" || l.Rel == "" {
			return l.Href
		}
	}
	if len(links) > 0 {
		return links[0].Href
	}
	return ""
}

func stripHTML(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func excerpt(s string, max int) string {
	s = stripHTML(s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func orName(name, u string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

func parseFeedTime(s string) time.Time {
	layouts := []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "Mon, 2 Jan 2006 15:04:05 -0700", "2006-01-02T15:04:05Z07:00"}
	s = strings.TrimSpace(s)
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
