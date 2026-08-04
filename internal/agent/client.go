// Package agent integrates Anthropic Managed Agents: a persisted, versioned
// agent + a reusable environment, and per-chat stateful sessions the reader can
// talk to about the page they're on. Tools: read the current topic, search
// related items in the collection, and web search/fetch.
//
// It talks to the Managed Agents REST API over HTTP (same lightweight style as
// the rest of the app) with the managed-agents beta header.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	apiBase = "https://api.anthropic.com"
	maBeta  = "managed-agents-2026-04-01"
)

type Client struct {
	apiKey string
	http   *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{apiKey: apiKey, http: &http.Client{Timeout: 90 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", maBeta)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// APIError carries the HTTP status so callers can detect e.g. a beta-access 403.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	b := e.Body
	if len(b) > 500 {
		b = b[:500] + "…"
	}
	return fmt.Sprintf("managed-agents %d: %s", e.Status, b)
}

// ---------- control plane ----------

type idVersion struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// CreateAgent creates a persisted agent and returns its ID.
func (c *Client) CreateAgent(ctx context.Context, name, model, system string, tools []any) (string, error) {
	var out idVersion
	err := c.do(ctx, http.MethodPost, "/v1/agents", map[string]any{
		"name": name, "model": model, "system": system, "tools": tools,
	}, &out)
	return out.ID, err
}

// CreateEnvironment creates a cloud environment with unrestricted egress
// (needed for web search) and returns its ID.
func (c *Client) CreateEnvironment(ctx context.Context, name string) (string, error) {
	var out idVersion
	err := c.do(ctx, http.MethodPost, "/v1/environments", map[string]any{
		"name": name,
		"config": map[string]any{
			"type":       "cloud",
			"networking": map[string]any{"type": "unrestricted"},
		},
	}, &out)
	return out.ID, err
}

type namedResource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FindEnvironment returns the ID of an environment with the given name, or "".
func (c *Client) FindEnvironment(ctx context.Context, name string) (string, error) {
	return c.findByName(ctx, "/v1/environments", name)
}

// FindAgent returns the ID of an agent with the given name, or "".
func (c *Client) FindAgent(ctx context.Context, name string) (string, error) {
	return c.findByName(ctx, "/v1/agents", name)
}

func (c *Client) findByName(ctx context.Context, path, name string) (string, error) {
	var out struct {
		Data []namedResource `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path+"?limit=100", nil, &out); err != nil {
		return "", err
	}
	for _, r := range out.Data {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return "", nil
}

// ---------- data plane ----------

type Session struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// CreateSession starts a chat session for the given agent + environment.
func (c *Client) CreateSession(ctx context.Context, agentID, envID, title string) (*Session, error) {
	var s Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions", map[string]any{
		"agent":          agentID, // string shorthand → latest version
		"environment_id": envID,
		"title":          title,
	}, &s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SendMessage sends a user text message to a session.
func (c *Client) SendMessage(ctx context.Context, sessionID, text string) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{
		"events": []any{map[string]any{
			"type":    "user.message",
			"content": []any{map[string]any{"type": "text", "text": text}},
		}},
	}, nil)
}

// SendToolResult returns a custom tool result for a pending custom_tool_use.
func (c *Client) SendToolResult(ctx context.Context, sessionID, toolUseID, result string) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{
		"events": []any{map[string]any{
			"type":               "user.custom_tool_result",
			"custom_tool_use_id": toolUseID,
			"content":            []any{map[string]any{"type": "text", "text": result}},
		}},
	}, nil)
}

// Event is a permissive view of a session event; Managed Agents event type
// strings are matched by suffix so minor naming differences don't break us.
type Event struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Input      json.RawMessage `json:"input"`
	StopReason *struct {
		Type     string   `json:"type"`
		EventIDs []string `json:"event_ids"`
	} `json:"stop_reason"`
}

// ListEvents returns all events for a session (oldest first).
func (c *Client) ListEvents(ctx context.Context, sessionID string) ([]Event, error) {
	var out struct {
		Data []Event `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+sessionID+"/events?limit=1000", nil, &out)
	return out.Data, err
}

// Archive best-effort cleans up a finished session.
func (c *Client) Archive(ctx context.Context, sessionID string) {
	_ = c.do(ctx, http.MethodPost, "/v1/sessions/"+sessionID+"/archive", map[string]any{}, nil)
}
