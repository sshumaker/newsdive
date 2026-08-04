// Command news is a personal research newsreader: it ingests LLM/agents work
// from arXiv, Hacker News, and research blogs, summarizes and scores each item
// with Claude, groups related work into themes, and serves a mobile-first UI
// with search and a tool-using research "ask" agent.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"news/internal/agent"
	"news/internal/config"
	"news/internal/ingest"
	"news/internal/llm"
	"news/internal/pipeline"
	"news/internal/server"
	"news/internal/store"
)

func main() {
	log.SetFlags(log.Ltime)

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer st.Close()

	cl := llm.New(cfg.AnthropicAPIKey)
	needsKey := func() {
		if cfg.AnthropicAPIKey == "" {
			log.Fatal("ANTHROPIC_API_KEY is required for this command (set it in .env)")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		serve(ctx, st, cl, cfg)

	case "ingest":
		rep, err := ingest.Run(st, cfg.SourceLimit)
		if err != nil {
			log.Fatalf("ingest: %v", err)
		}
		fmt.Printf("ingest: fetched=%d inserted=%d errors=%d\n", rep.Fetched, rep.Inserted, len(rep.Errors))

	case "enrich":
		needsKey()
		max := 200
		if len(os.Args) > 2 {
			max, _ = strconv.Atoi(os.Args[2])
		}
		n, err := pipeline.EnrichPending(ctx, st, cl, cfg.EnrichModel, cfg.Interests, max)
		if err != nil {
			log.Fatalf("enrich: %v", err)
		}
		fmt.Printf("enriched %d items\n", n)

	case "themes":
		needsKey()
		n, err := pipeline.RunThemes(ctx, st, cl, cfg.ThemeModel, 90)
		if err != nil {
			log.Fatalf("themes: %v", err)
		}
		fmt.Printf("built %d themes\n", n)

	case "all":
		needsKey()
		if err := pipeline.RunAll(ctx, st, cl, cfg); err != nil {
			log.Fatalf("all: %v", err)
		}

	case "ask":
		needsKey()
		if len(os.Args) < 3 {
			log.Fatal(`usage: news ask "your question"`)
		}
		question := os.Args[2]
		askCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		searcher := func(q string, limit int) []llm.CorpusHit {
			items, _ := st.Search(q, limit)
			hits := make([]llm.CorpusHit, 0, len(items))
			for _, it := range items {
				hits = append(hits, llm.CorpusHit{ID: it.ID, Title: it.Title, URL: it.URL,
					Source: it.Source, OneLiner: it.OneLiner, Summary: it.Summary, Tags: it.Tags,
					Published: it.PublishedAt.Format("2006-01-02")})
			}
			return hits
		}
		ans, err := cl.Ask(askCtx, cfg.AskModel, question, searcher)
		if err != nil && (ans == nil || ans.Text == "") {
			log.Fatalf("ask: %v", err)
		}
		fmt.Printf("\n%s\n", ans.Text)
		if len(ans.Citations) > 0 {
			fmt.Println("\nSources:")
			for _, c := range ans.Citations {
				fmt.Printf("  - %s — %s\n", c.Title, c.URL)
			}
		}
		fmt.Printf("\n[%d research steps, web=%v, %d in / %d out tokens]\n",
			ans.Turns, ans.UsedWeb, ans.Usage.InputTokens, ans.Usage.OutputTokens)

	case "chat":
		needsKey()
		if len(os.Args) < 3 {
			log.Fatal(`usage: news chat "message" [itemID for context]`)
		}
		page := agent.PageContext{Kind: "feed"}
		if len(os.Args) > 3 {
			if it, err := st.Get(os.Args[3]); err == nil {
				page = agent.PageContext{Kind: "item", ID: it.ID, Title: it.Title}
			}
		}
		ch := agent.NewChat(cfg.AnthropicAPIKey, st, cfg.AskModel)
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		r, err := ch.Send(cctx, "", os.Args[2], page)
		if err != nil && (r == nil || r.Text == "") {
			log.Fatalf("chat: %v", err)
		}
		fmt.Printf("\n[session %s]\n%s\n", r.SessionID, r.Text)

	case "stats":
		s, err := st.Stats()
		if err != nil {
			log.Fatalf("stats: %v", err)
		}
		fmt.Printf("items=%d enriched=%d unread=%d saved=%d themes=%d\n",
			s.Total, s.Enriched, s.Unread, s.Saved, s.Themes)

	default:
		fmt.Printf(`news — personal research newsreader

usage:
  news serve            run the web server + API (default)
  news ingest           fetch new items from all sources
  news enrich [N]       summarize/score up to N unenriched items (default 200)
  news themes           re-cluster recent items into themes
  news all              ingest + enrich + themes (one full cycle)
  news ask "question"   ask the research agent a question
  news stats            print collection stats

env: ANTHROPIC_API_KEY (required for enrich/themes/ask/serve), PORT,
     NEWS_REFRESH_MINUTES (background refresh; 0=off), NEWS_DB, NEWS_*_MODEL
`)
	}
}

func serve(ctx context.Context, st *store.Store, cl *llm.Client, cfg *config.Config) {
	if cfg.AnthropicAPIKey == "" {
		log.Println("WARNING: ANTHROPIC_API_KEY unset — enrichment and ask will fail until it is set")
	}
	go pipeline.Scheduler(ctx, st, cl, cfg)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: server.New(st, cl, cfg).Handler(),
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Printf("newsreader listening on http://localhost:%s", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}
