package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"news/internal/store"
)

type themesResult struct {
	Themes []struct {
		Title         string   `json:"title"`
		Description   string   `json:"description"`
		Connections   string   `json:"connections"`
		Disagreements string   `json:"disagreements"`
		Trajectory    string   `json:"trajectory"`
		ItemIDs       []string `json:"item_ids"`
	} `json:"themes"`
}

var themesSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"themes": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":         map[string]any{"type": "string"},
					"description":   map[string]any{"type": "string"},
					"connections":   map[string]any{"type": "string"},
					"disagreements": map[string]any{"type": "string"},
					"trajectory":    map[string]any{"type": "string"},
					"item_ids":      strArray,
				},
				"required":             []string{"title", "description", "connections", "disagreements", "trajectory", "item_ids"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"themes"},
	"additionalProperties": false,
}

const themesSystem = `You are a research synthesist. You are given a list of recent items (papers, posts, stories) about frontier LLM/agents work, each with an ID, title, one-line summary, and tags.

Group them into 4-10 coherent THEMES that reveal what is happening across the field right now — not just topical buckets, but meaningful groupings that surface recurring ideas, tensions, and directions.

For each theme:
- title: a short, specific name for the theme.
- description: 2-3 sentences on what the theme is and why it matters.
- connections: how the items relate and reinforce each other — shared ideas, methods, or findings that recur across them.
- disagreements: where the items conflict, give contradictory results, or take opposing approaches. Say "No clear disagreements among these" only if genuinely true.
- trajectory: how this line of work seems to be evolving — what's emerging, what's maturing, what's next.
- item_ids: the IDs of the items in this theme (use the exact IDs given).

Assign each item to its single best-fitting theme (an item may appear in more than one theme only if it genuinely bridges them). Prefer substantive themes over catch-all buckets.`

// SynthesizeThemes clusters recent enriched items into cross-cutting themes with
// synthesis of connections, disagreements, and trajectory.
func (c *Client) SynthesizeThemes(ctx context.Context, model string, items []*store.Item) ([]store.ThemeInput, error) {
	if len(items) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	for _, it := range items {
		line := it.OneLiner
		if line == "" {
			line = it.Title
		}
		fmt.Fprintf(&sb, "- id=%s | %s | %s | tags: %s\n",
			it.ID, truncate(it.Title, 120), truncate(line, 220), strings.Join(it.Tags, ", "))
	}

	resp, err := c.Do(ctx, Request{
		Model:     model,
		MaxTokens: 8000,
		System:    themesSystem,
		Messages:  []Message{{Role: "user", Content: "Recent items:\n\n" + sb.String()}},
		OutputConfig: &OutputConfig{
			Format: &Format{Type: "json_schema", Schema: themesSchema},
			Effort: "medium",
		},
	})
	if err != nil {
		return nil, err
	}

	var r themesResult
	if err := json.Unmarshal([]byte(resp.Text()), &r); err != nil {
		return nil, fmt.Errorf("parse themes: %w", err)
	}

	valid := map[string]bool{}
	for _, it := range items {
		valid[it.ID] = true
	}
	var out []store.ThemeInput
	for _, t := range r.Themes {
		var ids []string
		for _, id := range t.ItemIDs {
			if valid[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		out = append(out, store.ThemeInput{
			Title:         t.Title,
			Description:   t.Description,
			Connections:   t.Connections,
			Disagreements: t.Disagreements,
			Trajectory:    t.Trajectory,
			ItemIDs:       ids,
		})
	}
	return out, nil
}
