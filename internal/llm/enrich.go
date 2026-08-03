package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"news/internal/store"
)

// enrichResult mirrors the JSON schema we force the model to emit.
type enrichResult struct {
	OneLiner      string   `json:"one_liner"`
	Summary       string   `json:"summary"`
	KeyIdeas      []string `json:"key_ideas"`
	Claims        []string `json:"claims"`
	Evidence      []string `json:"evidence"`
	Limitations   []string `json:"limitations"`
	OpenQuestions []string `json:"open_questions"`
	Tags          []string `json:"tags"`
	Relevance     int      `json:"relevance"`
	Novelty       int      `json:"novelty"`
	Importance    int      `json:"importance"`
}

var enrichSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"one_liner":      map[string]any{"type": "string"},
		"summary":        map[string]any{"type": "string"},
		"key_ideas":      strArray,
		"claims":         strArray,
		"evidence":       strArray,
		"limitations":    strArray,
		"open_questions": strArray,
		"tags":           strArray,
		"relevance":      map[string]any{"type": "integer"},
		"novelty":        map[string]any{"type": "integer"},
		"importance":     map[string]any{"type": "integer"},
	},
	"required": []string{"one_liner", "summary", "key_ideas", "claims", "evidence",
		"limitations", "open_questions", "tags", "relevance", "novelty", "importance"},
	"additionalProperties": false,
}

var strArray = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}

const enrichSystem = `You are a research analyst curating a personal feed of frontier LLM/agents work for one reader.

Reader's interests (use these to judge relevance):
%s

For each item you are given (a paper, blog post, or news story), extract structure and score it. Be concrete and skeptical — surface real claims and real evidence, and name limitations honestly. If the item is thin (e.g. a title-only news story), infer what you reasonably can and keep lists short rather than padding.

Fields:
- one_liner: a single crisp sentence capturing what this is and why it matters.
- summary: 3-5 sentences. What problem, what approach, what result.
- key_ideas: the core ideas/techniques (bullet phrases).
- claims: specific claims the work makes.
- evidence: what backs the claims (datasets, benchmarks, experiments, ablations) — empty if none stated.
- limitations: honest limitations, caveats, or threats to validity.
- open_questions: what remains unresolved or worth following up.
- tags: 3-6 short lowercase topic tags (e.g. "agents", "tool-use", "rl", "evaluation").
- relevance: 0-100, how relevant to THIS reader's interests above.
- novelty: 0-100, how novel vs. prior work.
- importance: 0-100, likely importance/impact if the claims hold.

Score conservatively; reserve 90+ for genuinely strong, on-topic work.`

// Enrich summarizes, extracts structure from, and scores one item, mutating it
// in place. Uses structured outputs so the response is guaranteed valid JSON.
func (c *Client) Enrich(ctx context.Context, model, interests string, it *store.Item) error {
	user := fmt.Sprintf("Source: %s (%s)\nTitle: %s\nURL: %s\nAuthors: %s\nPublished: %s\n\nContent:\n%s",
		it.Source, it.SourceType, it.Title, it.URL, strings.Join(it.Authors, ", "),
		it.PublishedAt.Format("2006-01-02"), fallback(it.RawText, "(no abstract/excerpt available; reason from the title)"))

	resp, err := c.Do(ctx, Request{
		Model:     model,
		MaxTokens: 4000,
		System:    fmt.Sprintf(enrichSystem, interests),
		Messages:  []Message{{Role: "user", Content: user}},
		OutputConfig: &OutputConfig{
			Format: &Format{Type: "json_schema", Schema: enrichSchema},
			Effort: "low",
		},
	})
	if err != nil {
		return err
	}

	var r enrichResult
	if err := json.Unmarshal([]byte(resp.Text()), &r); err != nil {
		return fmt.Errorf("parse enrichment for %q: %w", it.Title, err)
	}

	it.OneLiner = r.OneLiner
	it.Summary = r.Summary
	it.KeyIdeas = r.KeyIdeas
	it.Claims = r.Claims
	it.Evidence = r.Evidence
	it.Limitations = r.Limitations
	it.OpenQuestions = r.OpenQuestions
	it.Tags = lowerAll(r.Tags)
	it.Relevance = clamp(r.Relevance)
	it.Novelty = clamp(r.Novelty)
	it.Importance = clamp(r.Importance)
	it.Enriched = true
	// Store a base score; the feed recomputes a recency-adjusted score on read.
	it.Score = 0.45*float64(it.Relevance) + 0.30*float64(it.Importance) + 0.25*float64(it.Novelty)
	return nil
}

func clamp(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func lowerAll(v []string) []string {
	for i := range v {
		v[i] = strings.ToLower(strings.TrimSpace(v[i]))
	}
	return v
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
