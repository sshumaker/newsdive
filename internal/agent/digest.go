package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"news/internal/llm"
	"news/internal/store"
)

// IdleBefore is how long a conversation must sit untouched before it is
// summarized. Summarizing while the reader is still typing would produce a
// digest that is stale on arrival, and each new turn clears the old one.
const IdleBefore = 15 * time.Minute

const digestSystem = `You are summarizing a chat conversation between a reader and a research assistant about a specific paper or post in the reader's collection.

Write a compact digest the reader will see on the article itself, weeks later, when they have forgotten the conversation. Cover:
- what they asked about and why
- what was concluded, including any specific numbers, names, or sources that came up
- anything left open or worth following up

Write 2-5 sentences of plain prose. No headings, no bullets, no preamble. Refer to the reader as "you". If the conversation reached no substantive conclusion, say so in one sentence rather than padding.`

// Digester rolls idle chat threads up into per-article summaries.
//
// The transcript is not stored locally — it lives in the Managed Agents session,
// so the digest is generated from that session's event list. This keeps a single
// source of truth for the conversation and means the sweep works even for
// threads started before a restart.
type Digester struct {
	chat  *Chat
	llm   *llm.Client
	model string
}

func NewDigester(chat *Chat, cl *llm.Client, model string) *Digester {
	return &Digester{chat: chat, llm: cl, model: model}
}

// SweepOnce summarizes every conversation that has been idle at least IdleBefore
// and has no current digest. Returns how many it wrote.
func (d *Digester) SweepOnce(ctx context.Context) (int, error) {
	convs, err := d.chat.st.StaleConversations(IdleBefore, 20)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, cv := range convs {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		summary, err := d.summarize(ctx, cv)
		if err != nil {
			log.Printf("digest %s: %v", cv.Topic, err)
			continue
		}
		if summary == "" {
			continue
		}
		// Guarded on last_activity: if the reader said something else while we
		// were summarizing, the write is skipped rather than overwriting a fresh
		// exchange with a stale digest.
		if err := d.chat.st.SaveConversationSummary(cv.Topic, summary, cv.LastActivity); err != nil {
			log.Printf("digest %s: save: %v", cv.Topic, err)
			continue
		}
		n++
	}
	return n, nil
}

// Scheduler runs SweepOnce on an interval until ctx is cancelled.
func (d *Digester) Scheduler(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	log.Printf("digest: summarizing idle conversations every %s (idle threshold %s)", every, IdleBefore)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := d.SweepOnce(ctx); err != nil {
				if ctx.Err() == nil {
					log.Printf("digest sweep: %v", err)
				}
			} else if n > 0 {
				log.Printf("digest: summarized %d conversation(s)", n)
			}
		}
	}
}

func (d *Digester) summarize(ctx context.Context, cv *store.Conversation) (string, error) {
	transcript, err := d.transcript(ctx, cv.SessionID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(transcript) == "" {
		return "", nil // nothing was actually said
	}

	head := ""
	if cv.Title != "" {
		head = fmt.Sprintf("The conversation is about: %s\n\n", cv.Title)
	}
	resp, err := d.llm.Do(ctx, llm.Request{
		Model:     d.model,
		MaxTokens: 1200,
		System:    digestSystem,
		Messages:  []llm.Message{{Role: "user", Content: head + "Conversation:\n\n" + transcript}},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text()), nil
}

// transcript renders a session's user/assistant turns as plain text, skipping
// tool traffic and status events.
func (d *Digester) transcript(ctx context.Context, sessionID string) (string, error) {
	events, err := d.chat.cl.ListEvents(ctx, sessionID)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, e := range events {
		var role string
		switch {
		case strings.HasSuffix(e.Type, "user.message"):
			role = "Reader"
		case strings.HasSuffix(e.Type, "agent.message"):
			role = "Assistant"
		default:
			continue
		}
		var text strings.Builder
		for _, b := range e.Content {
			if b.Type == "text" {
				text.WriteString(b.Text)
			}
		}
		if strings.TrimSpace(text.String()) == "" {
			continue
		}
		fmt.Fprintf(&sb, "%s: %s\n\n", role, strings.TrimSpace(text.String()))
	}
	return sb.String(), nil
}
