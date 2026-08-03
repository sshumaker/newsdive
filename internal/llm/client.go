// Package llm is a thin client for the Anthropic Messages API plus the three
// higher-level operations this app needs: per-item enrichment, cross-item
// theme synthesis, and the tool-using "ask" agent.
//
// It talks raw HTTP (rather than pulling in the SDK) to stay dependency-light
// and easy to cross-compile.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	apiURL     = "https://api.anthropic.com/v1/messages"
	apiVersion = "2023-06-01"
)

type Client struct {
	apiKey string
	http   *http.Client
}

func New(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 5 * time.Minute},
	}
}

// ---------- wire types ----------

type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []block | json.RawMessage
}

type Request struct {
	Model        string        `json:"model"`
	MaxTokens    int           `json:"max_tokens"`
	System       string        `json:"system,omitempty"`
	Messages     []Message     `json:"messages"`
	Tools        []any         `json:"tools,omitempty"`
	OutputConfig *OutputConfig `json:"output_config,omitempty"`
	betas        []string
}

type OutputConfig struct {
	Format *Format `json:"format,omitempty"`
	Effort string  `json:"effort,omitempty"`
}

type Format struct {
	Type   string `json:"type"`
	Schema any    `json:"schema,omitempty"`
}

type Response struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
	Usage      Usage           `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Block is a typed view of a single content block. server_tool_use /
// web_search_tool_result and similar are left as raw for verbatim echo.
type Block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (r *Response) Blocks() []Block {
	var b []Block
	_ = json.Unmarshal(r.Content, &b)
	return b
}

// Text concatenates all text blocks in the response.
func (r *Response) Text() string {
	var sb strings.Builder
	for _, b := range r.Blocks() {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// Do sends one Messages request.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("content-type", "application/json")
	if len(req.betas) > 0 {
		httpReq.Header.Set("anthropic-beta", strings.Join(req.betas, ","))
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(raw), 600))
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
