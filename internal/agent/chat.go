package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"news/internal/store"
)

// PageContext tells the agent what the reader is currently looking at.
type PageContext struct {
	Kind  string `json:"kind"` // item | theme | feed
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Chat orchestrates Managed Agents chat sessions over the reader's collection.
type Chat struct {
	cl    *Client
	st    *store.Store
	model string

	mu       sync.Mutex
	ready    bool
	agentID  string
	envID    string
	sessions map[string]*sessState
}

type sessState struct {
	page      PageContext
	processed map[string]bool
}

func NewChat(apiKey string, st *store.Store, model string) *Chat {
	return &Chat{cl: NewClient(apiKey), st: st, model: model, sessions: map[string]*sessState{}}
}

const (
	agentName = "newsdive-chat"
	envName   = "newsdive-env"
)

const chatSystem = `You are a research chat companion inside "newsdive", a personal reader for frontier LLM/agents research. The reader is browsing their collection and wants to talk about what they're looking at.

Tools:
- read_current_topic: read the full details of the item or theme the reader is currently viewing. Call it first when the reader refers to "this", "the paper", "this theme", etc.
- search_corpus: find related items already in the reader's collection by keyword — use for "related work", comparisons, and connections.
- web_search / web_fetch: go to the public web for details, the full text of a paper/post, or newer information.

Style: conversational and concrete, like a knowledgeable colleague. Keep answers tight; lead with the point. Ground claims in the sources you read and link them. Don't invent citations. When the reader is on a specific item or theme, assume follow-up questions are about it unless they say otherwise.`

func (c *Chat) agentTools() []any {
	return []any{
		map[string]any{
			"type":           "agent_toolset_20260401",
			"default_config": map[string]any{"enabled": false},
			"configs": []any{
				map[string]any{"name": "web_search", "enabled": true},
				map[string]any{"name": "web_fetch", "enabled": true},
			},
		},
		map[string]any{
			"type": "custom", "name": "read_current_topic",
			"description":  "Read the full details of the item or theme the reader is currently viewing (title, summary, key ideas, claims, evidence, limitations, open questions, or a theme's synthesis). Call this first to understand what the reader means by 'this'.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		map[string]any{
			"type": "custom", "name": "search_corpus",
			"description": "Search the reader's personal collection for related items by keyword. Returns matching items with title, url, summary, and tags. Use for related work, comparisons, and cross-item connections.",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": map[string]any{"type": "string", "description": "keywords"}},
				"required":   []string{"query"},
			},
		},
	}
}

// ensureSetup creates (or finds) the persisted agent + environment once, caching
// their IDs in the settings table.
func (c *Chat) ensureSetup(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready {
		return nil
	}
	// Cached in DB?
	if a, ok := c.st.GetSetting("ma_agent_id"); ok {
		if e, ok2 := c.st.GetSetting("ma_env_id"); ok2 {
			c.agentID, c.envID, c.ready = a, e, true
			return nil
		}
	}
	// Reuse existing resources by name if present (idempotent across DB resets).
	envID, err := c.cl.FindEnvironment(ctx, envName)
	if err != nil {
		return err
	}
	if envID == "" {
		if envID, err = c.cl.CreateEnvironment(ctx, envName); err != nil {
			return err
		}
	}
	agentID, err := c.cl.FindAgent(ctx, agentName)
	if err != nil {
		return err
	}
	if agentID == "" {
		if agentID, err = c.cl.CreateAgent(ctx, agentName, c.model, chatSystem, c.agentTools()); err != nil {
			return err
		}
	}
	c.st.SetSetting("ma_agent_id", agentID)
	c.st.SetSetting("ma_env_id", envID)
	c.agentID, c.envID, c.ready = agentID, envID, true
	return nil
}

// Reply is one assistant turn.
type Reply struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

// Send handles one chat turn: starts a session if needed (seeding page context),
// sends the message, and drains the agent's turn (running custom tools).
func (c *Chat) Send(ctx context.Context, sessionID, message string, page PageContext) (*Reply, error) {
	if err := c.ensureSetup(ctx); err != nil {
		return nil, err
	}

	c.mu.Lock()
	ss := c.sessions[sessionID]
	c.mu.Unlock()

	if sessionID == "" || ss == nil {
		s, err := c.cl.CreateSession(ctx, c.agentID, c.envID, "newsdive chat")
		if err != nil {
			return nil, err
		}
		sessionID = s.ID
		ss = &sessState{page: page, processed: map[string]bool{}}
		c.mu.Lock()
		c.sessions[sessionID] = ss
		c.mu.Unlock()
	} else {
		ss.page = page // page context can change between turns
	}

	// Prepend a short context note on the first turn so "this" is unambiguous.
	toSend := message
	if len(ss.processed) == 0 && page.Kind != "" && page.Kind != "feed" {
		toSend = fmt.Sprintf("[Context: I'm currently viewing the %s \"%s\". Use read_current_topic for its details.]\n\n%s",
			page.Kind, page.Title, message)
	}
	if err := c.cl.SendMessage(ctx, sessionID, toSend); err != nil {
		return nil, err
	}

	text, err := c.drainTurn(ctx, sessionID, ss)
	return &Reply{SessionID: sessionID, Text: strings.TrimSpace(text)}, err
}

// drainTurn polls the event stream until the turn completes, executing custom
// tool calls (read_current_topic / search_corpus) as they arrive.
func (c *Chat) drainTurn(ctx context.Context, sessionID string, ss *sessState) (string, error) {
	var reply strings.Builder
	emptyIdle := 0

	for {
		if err := ctx.Err(); err != nil {
			return reply.String(), err
		}
		events, err := c.cl.ListEvents(ctx, sessionID)
		if err != nil {
			return reply.String(), err
		}

		var pending []Event
		var needAnswer, done bool
		for _, e := range events {
			if ss.processed[e.ID] {
				continue
			}
			ss.processed[e.ID] = true
			switch {
			case strings.HasSuffix(e.Type, "agent.message"), e.Type == "agent.message":
				for _, b := range e.Content {
					if b.Type == "text" {
						reply.WriteString(b.Text)
					}
				}
			case strings.Contains(e.Type, "custom_tool_use"):
				pending = append(pending, e)
			case strings.Contains(e.Type, "status_terminated"):
				done = true
			case strings.Contains(e.Type, "status_idle"):
				if e.StopReason != nil && e.StopReason.Type == "requires_action" {
					needAnswer = true
				} else {
					done = true
				}
			}
		}

		if len(pending) > 0 {
			for _, tc := range pending {
				result := c.runTool(ctx, ss, tc.Name, tc.Input)
				if err := c.cl.SendToolResult(ctx, sessionID, tc.ID, result); err != nil {
					return reply.String(), err
				}
			}
			emptyIdle = 0
			continue
		}
		if done {
			return reply.String(), nil
		}
		if needAnswer {
			// Idle wanting action but nothing to answer — avoid a deadlock.
			if emptyIdle++; emptyIdle > 3 {
				return reply.String(), nil
			}
		}
		time.Sleep(1200 * time.Millisecond)
	}
}

// runTool executes a custom tool against the collection.
func (c *Chat) runTool(ctx context.Context, ss *sessState, name string, input json.RawMessage) string {
	switch name {
	case "read_current_topic":
		return c.readCurrentTopic(ss.page)
	case "search_corpus":
		var in struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(input, &in)
		return c.searchCorpus(in.Query)
	default:
		return "unknown tool: " + name
	}
}

func (c *Chat) readCurrentTopic(page PageContext) string {
	switch page.Kind {
	case "item":
		it, err := c.st.Get(page.ID)
		if err != nil {
			return "Could not load the current item."
		}
		return formatItem(it)
	case "theme":
		var id int64
		fmt.Sscanf(page.ID, "%d", &id)
		t, err := c.st.ThemeWithItems(id)
		if err != nil {
			return "Could not load the current theme."
		}
		return formatTheme(t)
	default:
		return "The reader is on the main feed — no single topic is in focus. Ask what they'd like to explore, or use search_corpus."
	}
}

func (c *Chat) searchCorpus(query string) string {
	items, err := c.st.Search(query, 8)
	if err != nil || len(items) == 0 {
		return "No matching items in the collection for: " + query
	}
	var sb strings.Builder
	for _, it := range items {
		line := it.OneLiner
		if line == "" {
			line = it.Summary
		}
		fmt.Fprintf(&sb, "- %s (%s) [%s]\n  %s\n", it.Title, it.Source, it.URL, truncate(line, 200))
	}
	return sb.String()
}

func formatItem(it *store.Item) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "TITLE: %s\nSOURCE: %s (%s)\nURL: %s\n\n", it.Title, it.Source, it.SourceType, it.URL)
	if it.Summary != "" {
		fmt.Fprintf(&sb, "SUMMARY: %s\n\n", it.Summary)
	}
	writeList(&sb, "KEY IDEAS", it.KeyIdeas)
	writeList(&sb, "CLAIMS", it.Claims)
	writeList(&sb, "EVIDENCE", it.Evidence)
	writeList(&sb, "LIMITATIONS", it.Limitations)
	writeList(&sb, "OPEN QUESTIONS", it.OpenQuestions)
	if len(it.Tags) > 0 {
		fmt.Fprintf(&sb, "TAGS: %s\n", strings.Join(it.Tags, ", "))
	}
	return sb.String()
}

func formatTheme(t *store.Theme) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "THEME: %s\n%s\n\n", t.Title, t.Description)
	fmt.Fprintf(&sb, "CONNECTIONS: %s\nDISAGREEMENTS: %s\nTRAJECTORY: %s\n\nITEMS IN THEME:\n", t.Connections, t.Disagreements, t.Trajectory)
	for _, it := range t.Items {
		line := it.OneLiner
		if line == "" {
			line = it.Title
		}
		fmt.Fprintf(&sb, "- %s [%s]\n  %s\n", it.Title, it.URL, truncate(line, 180))
	}
	return sb.String()
}

func writeList(sb *strings.Builder, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(sb, "%s:\n", label)
	for _, x := range items {
		fmt.Fprintf(sb, "- %s\n", x)
	}
	sb.WriteString("\n")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
