package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"news/internal/llm"
	"news/internal/sources"
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

	// enrich summarizes/scores articles the reader adds on demand, so a manually
	// added item looks identical to an ingested one. Optional: without it,
	// add_article still stores the item but leaves it unenriched for the next
	// scheduled pass to pick up.
	enrich    *llm.Client
	enrichCfg EnrichConfig

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

// EnrichConfig carries what the enrichment pass needs when the reader adds an
// article on demand.
type EnrichConfig struct {
	Model     string
	Interests string
}

// WithEnrichment lets add_article summarize and score a newly added article
// inline, instead of leaving it for the next scheduled enrich pass.
func (c *Chat) WithEnrichment(cl *llm.Client, cfg EnrichConfig) *Chat {
	c.enrich, c.enrichCfg = cl, cfg
	return c
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
		map[string]any{
			"type": "custom", "name": "add_article",
			"description": "Add an article to the reader's collection from a URL. Fetches the page, summarizes and scores it the same way the ingest pipeline does, and files it under the best-matching existing theme. Call this when the reader gives you a URL and asks to add/save/ingest it, or asks you to track a paper or post. Returns the new item's id, title, scores, and assigned theme.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string", "description": "The article/paper/post URL to add."},
				},
				"required": []string{"url"},
			},
		},
	}
}

// toolsetVersion changes whenever agentTools() changes. The agent is a persisted
// server-side object whose id is cached in settings, so without this marker a
// newly added tool would never reach an already-created agent — ensureSetup
// would find the cached id and skip straight past the tool list.
const toolsetVersion = "2-add-article"

// ensureSetup creates (or finds) the persisted agent + environment once, caching
// their IDs in the settings table.
func (c *Chat) ensureSetup(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready {
		return nil
	}
	// Cached in DB? Only reuse if the cached agent was built with the current
	// tool set; otherwise fall through and push a new agent version.
	if a, ok := c.st.GetSetting("ma_agent_id"); ok {
		if e, ok2 := c.st.GetSetting("ma_env_id"); ok2 {
			if v, _ := c.st.GetSetting("ma_toolset"); v == toolsetVersion {
				c.agentID, c.envID, c.ready = a, e, true
				return nil
			}
			if err := c.cl.UpdateAgent(ctx, a, c.model, chatSystem, c.agentTools()); err != nil {
				return fmt.Errorf("update agent tools: %w", err)
			}
			c.st.SetSetting("ma_toolset", toolsetVersion)
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
	} else if err := c.cl.UpdateAgent(ctx, agentID, c.model, chatSystem, c.agentTools()); err != nil {
		// Found by name but built by an older build — sync its tools rather than
		// silently running with a stale tool set.
		return fmt.Errorf("update agent tools: %w", err)
	}
	c.st.SetSetting("ma_agent_id", agentID)
	c.st.SetSetting("ma_env_id", envID)
	c.st.SetSetting("ma_toolset", toolsetVersion)
	c.agentID, c.envID, c.ready = agentID, envID, true
	return nil
}

// Reply is one assistant turn.
type Reply struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

// Progress reports turn activity as it happens. A turn can run for minutes
// when the agent searches the web, so callers stream these to show real
// movement instead of an indefinite spinner.
type Progress struct {
	// "text"    — live delta; append to the block identified by ID
	// "message"  — authoritative text for block ID; replaces any deltas
	// "tool"    — a tool call started
	// "session" — carries the session id
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Tool string `json:"tool,omitempty"`
	ID   string `json:"id,omitempty"` // session id on "session"; event id otherwise
}

// Send handles one chat turn: starts a session if needed (seeding page context),
// sends the message, and drains the agent's turn (running custom tools).
//
// onEvent, if non-nil, receives incremental progress; it is called from this
// goroutine, so it must not block for long.
func (c *Chat) Send(ctx context.Context, sessionID, message string, page PageContext, onEvent func(Progress)) (*Reply, error) {
	if err := c.ensureSetup(ctx); err != nil {
		return nil, err
	}

	topic := c.topicOf(page)

	c.mu.Lock()
	ss := c.sessions[sessionID]
	c.mu.Unlock()

	// The in-memory session map doesn't survive a restart. Fall back to the
	// persisted topic→session mapping so a reconnecting client resumes its thread
	// instead of silently starting a new one while still showing the old
	// transcript.
	if ss == nil {
		if persisted, ok := c.st.ConversationSession(topic); ok {
			sessionID = persisted
			ss = &sessState{page: page, processed: map[string]bool{}}
			c.mu.Lock()
			c.sessions[sessionID] = ss
			c.mu.Unlock()
		}
	}

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
	emit := func(p Progress) {
		if onEvent != nil {
			onEvent(p)
		}
	}
	// Report the session id up front so a client that disconnects mid-turn can
	// still resume the conversation rather than orphaning the session.
	emit(Progress{Type: "session", ID: sessionID})

	// Open the stream BEFORE sending: it delivers only events that occur after it
	// opens, so send-then-stream loses the start of the turn.
	frames, stop, err := c.cl.StreamEvents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer stop()

	if err := c.cl.SendMessage(ctx, sessionID, toSend); err != nil {
		return nil, err
	}

	text, err := c.drainTurn(ctx, sessionID, ss, frames, emit)

	// Record the thread even on a failed turn: the session exists server-side and
	// the reader should resume it rather than orphan it.
	itemID := ""
	if page.Kind == "item" {
		itemID = page.ID
	}
	if uerr := c.st.UpsertConversation(topic, itemID, page.Title, sessionID); uerr != nil {
		log.Printf("chat: record conversation %s: %v", topic, uerr)
	}
	return &Reply{SessionID: sessionID, Text: strings.TrimSpace(text)}, err
}

// topicOf is the durable key for a chat thread. Themes are keyed by their stable
// key rather than their row id, which is reassigned on re-clustering — keying on
// the id would silently attach the thread to a different theme.
func (c *Chat) topicOf(page PageContext) string {
	switch page.Kind {
	case "item":
		return "item:" + page.ID
	case "theme":
		var id int64
		fmt.Sscanf(page.ID, "%d", &id)
		if t, err := c.st.ThemeWithItems(id); err == nil && t.Key != "" {
			return "theme:" + t.Key
		}
		return "theme:" + store.ThemeKey(page.Title)
	default:
		return "feed"
	}
}

// drainTurn consumes the session's SSE stream until the turn completes,
// executing custom tool calls (read_current_topic / search_corpus / add_article)
// as they arrive.
//
// Assistant text is handled twice on purpose: event_delta frames stream a live
// preview so output appears as it is generated, and the buffered agent.message
// that follows is the authoritative version. Both carry the same event id, so
// the preview is emitted as "text" (append) and the final as "message"
// (replace) — deltas are best-effort and may be a truncated prefix, so the
// reply we return is built only from the buffered events.
func (c *Chat) drainTurn(ctx context.Context, sessionID string, ss *sessState, frames <-chan Frame, emit func(Progress)) (string, error) {
	var reply strings.Builder
	for {
		select {
		case <-ctx.Done():
			return reply.String(), ctx.Err()
		case f, ok := <-frames:
			if !ok {
				// Stream closed without a terminal status. Reconcile against the
				// event list rather than reporting nothing: SSE has no replay, so
				// anything emitted during the drop is only available here.
				return c.reconcile(ctx, sessionID, ss, reply.String(), emit)
			}

			if id, text, isDelta := f.DeltaText(); isDelta {
				emit(Progress{Type: "text", ID: id, Text: text})
				continue
			}
			if f.Type == "event_start" {
				continue // nothing to render yet; the deltas follow
			}
			if f.ID != "" {
				if ss.processed[f.ID] {
					continue
				}
				ss.processed[f.ID] = true
			}

			switch {
			case strings.HasSuffix(f.Type, "agent.message"):
				// A turn with tool calls produces several message events. Separate
				// them, or the preamble runs into the answer ("...collection.Both
				// arXiv fetches..."). Blocks *within* one event are contiguous.
				var msg strings.Builder
				for _, b := range f.Content {
					if b.Type == "text" {
						msg.WriteString(b.Text)
					}
				}
				if msg.Len() == 0 {
					continue
				}
				if reply.Len() > 0 {
					reply.WriteString("\n\n")
				}
				reply.WriteString(msg.String())
				emit(Progress{Type: "message", ID: f.ID, Text: msg.String()})

			case strings.Contains(f.Type, "custom_tool_use"):
				emit(Progress{Type: "tool", Tool: f.Name})
				result := c.runTool(ctx, ss, f.Name, f.Input)
				if err := c.cl.SendToolResult(ctx, sessionID, f.ID, result); err != nil {
					return reply.String(), err
				}

			// Server-side tools (web_search / web_fetch) arrive as agent.tool_use
			// and run without us; surfacing them is what makes a long turn legible.
			// Must stay below the custom case, which its type also matches.
			case strings.Contains(f.Type, "tool_use"):
				emit(Progress{Type: "tool", Tool: f.Name})

			case strings.Contains(f.Type, "status_terminated"):
				return reply.String(), nil

			case strings.Contains(f.Type, "status_idle"):
				// Idle is transient — it also fires while the agent waits on a tool
				// result we owe it. Only a non-requires_action idle ends the turn.
				if f.StopReason == nil || f.StopReason.Type != "requires_action" {
					return reply.String(), nil
				}
			}
		}
	}
}

// reconcile fetches the event list after the stream ends unexpectedly and
// recovers any assistant text that was emitted during the gap.
func (c *Chat) reconcile(ctx context.Context, sessionID string, ss *sessState, sofar string, emit func(Progress)) (string, error) {
	events, err := c.cl.ListEvents(ctx, sessionID)
	if err != nil {
		return sofar, nil // the stream already gave us what it could
	}
	var b strings.Builder
	b.WriteString(sofar)
	for _, e := range events {
		if !strings.HasSuffix(e.Type, "agent.message") || ss.processed[e.ID] {
			continue
		}
		ss.processed[e.ID] = true
		var msg strings.Builder
		for _, blk := range e.Content {
			if blk.Type == "text" {
				msg.WriteString(blk.Text)
			}
		}
		if msg.Len() == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(msg.String())
		emit(Progress{Type: "message", ID: e.ID, Text: msg.String()})
	}
	return b.String(), nil
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
	case "add_article":
		var in struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(input, &in)
		return c.addArticle(ctx, in.URL)
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

// addArticle fetches a URL, stores it, enriches it, and files it under the
// best-matching existing theme.
//
// Theme assignment reuses the existing clustering rather than re-running it: a
// full re-cluster costs minutes and would renumber everything, which is the
// wrong trade for adding one article. The item's tags and title are matched
// against current themes, and a full re-theme on the next scheduled pass will
// place it properly.
// AddArticle is addArticle exposed for the UI's add-by-URL endpoint, so the
// button and the agent tool share one implementation.
func (c *Chat) AddArticle(ctx context.Context, rawURL string) string {
	return c.addArticle(ctx, rawURL)
}

func (c *Chat) addArticle(ctx context.Context, rawURL string) string {
	it, err := sources.FetchURL(rawURL)
	if err != nil {
		return "Could not add that URL: " + err.Error()
	}
	if existing, err := c.st.Get(it.ID); err == nil && existing != nil {
		return fmt.Sprintf("Already in the collection: %q (%s). id=%s",
			existing.Title, existing.URL, existing.ID)
	}
	if _, err := c.st.Upsert(it); err != nil {
		return "Could not save that article: " + err.Error()
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Added %q (%s)\nid=%s\n", it.Title, it.URL, it.ID)

	if c.enrich == nil {
		sb.WriteString("Stored unenriched — the next enrichment pass will summarize and score it.\n")
		return sb.String()
	}
	if err := c.enrich.Enrich(ctx, c.enrichCfg.Model, c.enrichCfg.Interests, it); err != nil {
		sb.WriteString("Stored, but enrichment failed (" + err.Error() + "); it will be retried automatically.\n")
		return sb.String()
	}
	if err := c.st.SaveEnrichment(it); err != nil {
		sb.WriteString("Stored, but saving the enrichment failed: " + err.Error() + "\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "relevance=%d novelty=%d importance=%d\ntags: %s\n%s\n",
		it.Relevance, it.Novelty, it.Importance, strings.Join(it.Tags, ", "), it.OneLiner)

	if theme, err := c.assignTheme(it); err == nil && theme != "" {
		fmt.Fprintf(&sb, "Filed under theme: %s\n", theme)
	} else {
		sb.WriteString("No existing theme matched closely; it will be placed on the next re-clustering.\n")
	}
	return sb.String()
}

// assignTheme files an item under the existing theme whose title/description
// shares the most tag and title terms with it. Returns the theme title, or "" if
// nothing overlapped.
func (c *Chat) assignTheme(it *store.Item) (string, error) {
	themes, err := c.st.Themes()
	if err != nil || len(themes) == 0 {
		return "", err
	}
	terms := map[string]bool{}
	for _, t := range it.Tags {
		for _, w := range strings.Fields(strings.ToLower(t)) {
			if len(w) > 3 {
				terms[w] = true
			}
		}
	}
	for _, w := range strings.Fields(strings.ToLower(it.Title)) {
		if len(w) > 4 {
			terms[w] = true
		}
	}

	best, bestScore := (*store.Theme)(nil), 0
	for _, th := range themes {
		hay := strings.ToLower(th.Title + " " + th.Description)
		score := 0
		for term := range terms {
			if strings.Contains(hay, term) {
				score++
			}
		}
		if th.Starred {
			score++ // nudge toward themes the reader is deliberately tracking
		}
		if score > bestScore {
			best, bestScore = th, score
		}
	}
	if best == nil || bestScore < 2 {
		return "", nil // too weak to be worth a wrong assignment
	}
	if err := c.st.SetItemTheme(it.ID, best.ID); err != nil {
		return "", err
	}
	return best.Title, nil
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
	if it.SkepticsTake != "" {
		fmt.Fprintf(&sb, "SKEPTIC'S TAKE: %s\n\n", it.SkepticsTake)
	}
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
