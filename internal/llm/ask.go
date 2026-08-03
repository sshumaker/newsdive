package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CorpusHit is a compact record returned by the corpus-search tool.
type CorpusHit struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	URL       string   `json:"url"`
	Source    string   `json:"source"`
	OneLiner  string   `json:"one_liner"`
	Summary   string   `json:"summary"`
	Tags      []string `json:"tags"`
	Published string   `json:"published"`
}

// CorpusSearcher runs a keyword search over the reader's collected items.
type CorpusSearcher func(query string, limit int) []CorpusHit

// Answer is the result of an ask session.
type Answer struct {
	Text      string     `json:"text"`
	Citations []Citation `json:"citations"`
	Turns     int        `json:"turns"`
	UsedWeb   bool       `json:"used_web"`
	Usage     Usage      `json:"usage"`
}

type Citation struct {
	Title  string `json:"title"`
	URL    string `json:"url"`
	Source string `json:"source"`
}

const askSystem = `You are a research assistant for one reader's personal collection of frontier LLM/agents research (papers, blog posts, HN stories).

You have three tools:
- search_corpus: search the reader's OWN collected items. Start here to ground answers in what they've been following.
- web_search: search the public web for details, recent developments, or things not in the collection.
- web_fetch: fetch the full text of a specific URL (e.g. an arXiv paper or blog post) surfaced by search to read it closely.

How to work:
1. Search the corpus first to see what the reader already has on the topic.
2. Follow up: use web_search / web_fetch to read the actual sources, verify claims, or fill gaps — don't stop at the summaries you already have if the question needs more depth.
3. Synthesize across sources. Note agreements, disagreements, and how the ideas connect or evolve.

Answer style: direct and specific. Lead with the answer. Cite sources inline with their title and URL. If the collection is thin on the topic, say so and lean on web sources. Never invent citations — only cite things you actually retrieved.`

var corpusTool = map[string]any{
	"name":        "search_corpus",
	"description": "Search the reader's personal collection of LLM/agents research items. Returns matching items with title, URL, summary, and tags.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "keywords to search for"},
			"limit": map[string]any{"type": "integer", "description": "max results (default 8)"},
		},
		"required": []string{"query"},
	},
}

func webTools() []any {
	return []any{
		map[string]any{"type": "web_search_20260209", "name": "web_search", "max_uses": 6},
		map[string]any{"type": "web_fetch_20260209", "name": "web_fetch", "max_uses": 6},
	}
}

// Ask runs a tool-using research loop. It tries with web tools enabled; if the
// account can't use them, it falls back to a corpus-only run so the feature
// still works.
func (c *Client) Ask(ctx context.Context, model, question string, search CorpusSearcher) (*Answer, error) {
	full := append(webTools(), corpusTool)
	ans, err := c.runAsk(ctx, model, question, search, full, true)
	if err != nil && strings.Contains(err.Error(), "web_") {
		// Web tools unavailable on this account — retry corpus-only.
		return c.runAsk(ctx, model, question, search, []any{corpusTool}, false)
	}
	return ans, err
}

func (c *Client) runAsk(ctx context.Context, model, question string, search CorpusSearcher, tools []any, usedWeb bool) (*Answer, error) {
	messages := []Message{{Role: "user", Content: question}}
	ans := &Answer{UsedWeb: usedWeb}
	seenCite := map[string]bool{}

	const maxTurns = 14
	for turn := 0; turn < maxTurns; turn++ {
		ans.Turns = turn + 1
		resp, err := c.Do(ctx, Request{
			Model:     model,
			MaxTokens: 8000,
			System:    askSystem,
			Messages:  messages,
			Tools:     tools,
		})
		if err != nil {
			return ans, err
		}
		ans.Usage.InputTokens += resp.Usage.InputTokens
		ans.Usage.OutputTokens += resp.Usage.OutputTokens

		switch resp.StopReason {
		case "end_turn", "":
			ans.Text = strings.TrimSpace(resp.Text())
			return ans, nil

		case "refusal":
			ans.Text = "The model declined to answer this request."
			return ans, nil

		case "pause_turn":
			// Server-side tool loop hit its cap; resend to resume.
			messages = append(messages, Message{Role: "assistant", Content: resp.Content})

		case "tool_use":
			messages = append(messages, Message{Role: "assistant", Content: resp.Content})
			var results []any
			for _, b := range resp.Blocks() {
				if b.Type != "tool_use" || b.Name != "search_corpus" {
					continue
				}
				var in struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}
				_ = json.Unmarshal(b.Input, &in)
				if in.Limit <= 0 || in.Limit > 20 {
					in.Limit = 8
				}
				hits := search(in.Query, in.Limit)
				for _, h := range hits {
					if h.URL != "" && !seenCite[h.URL] {
						seenCite[h.URL] = true
						ans.Citations = append(ans.Citations, Citation{Title: h.Title, URL: h.URL, Source: h.Source})
					}
				}
				payload, _ := json.Marshal(hits)
				results = append(results, map[string]any{
					"type":        "tool_result",
					"tool_use_id": b.ID,
					"content":     string(payload),
				})
			}
			if len(results) == 0 {
				// No client tool to run and not paused — treat as done.
				ans.Text = strings.TrimSpace(resp.Text())
				return ans, nil
			}
			messages = append(messages, Message{Role: "user", Content: results})

		default:
			ans.Text = strings.TrimSpace(resp.Text())
			return ans, nil
		}
	}
	if ans.Text == "" {
		ans.Text = "(reached the maximum number of research steps without a final answer)"
	}
	return ans, fmt.Errorf("ask: reached max turns")
}
