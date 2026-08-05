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
	"strconv"
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
	Thinking     *Thinking     `json:"thinking,omitempty"`
	betas        []string
}

type Thinking struct {
	Type string `json:"type"` // "adaptive" | "disabled"
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

// maxAttempts bounds retries of rate-limited / transient failures. Enrichment
// runs many requests concurrently, so 429s are expected rather than exceptional.
const maxAttempts = 5

// Do sends one Messages request, retrying 429s and 5xx with backoff.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}
		out, retryAfter, err := c.do(ctx, body, req.betas)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if retryAfter < 0 {
			return nil, err // not retryable
		}
		if retryAfter > 0 {
			if err := sleep(ctx, retryAfter); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// do performs a single attempt. The returned duration is the server-requested
// retry delay; zero means "retryable, use backoff" and -1 means "do not retry".
func (c *Client) do(ctx context.Context, body []byte, betas []string) (*Response, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, -1, err
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("content-type", "application/json")
	if len(betas) > 0 {
		httpReq.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, -1, err
		}
		return nil, 0, err // transport hiccup: retry
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(raw), 600))
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, retryAfter(resp), err
		case resp.StatusCode >= 500:
			return nil, 0, err
		default:
			return nil, -1, err
		}
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, -1, fmt.Errorf("decode response: %w", err)
	}
	return &out, 0, nil
}

// retryAfter reads the server's requested delay, capped so one throttled
// request cannot stall a worker for minutes.
func retryAfter(resp *http.Response) time.Duration {
	secs, err := strconv.Atoi(resp.Header.Get("retry-after"))
	if err != nil || secs <= 0 {
		return 0
	}
	return min(time.Duration(secs)*time.Second, 30*time.Second)
}

// backoff grows exponentially from 1s, jittered by attempt so concurrent
// workers throttled together do not retry in lockstep.
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	return min(d, 16*time.Second) + time.Duration(attempt*137)*time.Millisecond
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
