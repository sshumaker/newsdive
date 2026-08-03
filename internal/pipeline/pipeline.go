// Package pipeline wires ingestion, enrichment, and theme synthesis together,
// and runs them on a background schedule when the server is up.
package pipeline

import (
	"context"
	"log"
	"sync"
	"time"

	"news/internal/config"
	"news/internal/ingest"
	"news/internal/llm"
	"news/internal/store"
)

// EnrichPending summarizes and scores up to `max` unenriched items, running the
// (slow) LLM calls with bounded concurrency while DB writes serialize.
func EnrichPending(ctx context.Context, st *store.Store, cl *llm.Client, model, interests string, max int) (int, error) {
	pending, err := st.PendingEnrichment(max)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	const workers = 4
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		done int
		jobs = make(chan *store.Item)
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := cl.Enrich(ctx, model, interests, it); err != nil {
					log.Printf("enrich %q: %v", it.Title, err)
					continue
				}
				if err := st.SaveEnrichment(it); err != nil {
					log.Printf("save enrichment %q: %v", it.Title, err)
					continue
				}
				mu.Lock()
				done++
				n := done
				mu.Unlock()
				log.Printf("enriched [%d/%d] (rel=%d) %s", n, len(pending), it.Relevance, it.Title)
			}
		}()
	}
	for _, it := range pending {
		if ctx.Err() != nil {
			break
		}
		jobs <- it
	}
	close(jobs)
	wg.Wait()
	return done, nil
}

// RunThemes re-clusters the most recent enriched items into themes.
func RunThemes(ctx context.Context, st *store.Store, cl *llm.Client, model string, limit int) (int, error) {
	items, err := st.RecentEnriched(limit)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	themes, err := cl.SynthesizeThemes(ctx, model, items)
	if err != nil {
		return 0, err
	}
	if err := st.ReplaceThemes(themes); err != nil {
		return 0, err
	}
	return len(themes), nil
}

// RunAll does one full cycle: ingest, enrich, re-theme.
func RunAll(ctx context.Context, st *store.Store, cl *llm.Client, cfg *config.Config) error {
	rep, err := ingest.Run(st, cfg.SourceLimit)
	if err != nil {
		return err
	}
	log.Printf("ingest: fetched=%d inserted=%d", rep.Fetched, rep.Inserted)

	n, err := EnrichPending(ctx, st, cl, cfg.EnrichModel, cfg.Interests, 200)
	if err != nil {
		return err
	}
	log.Printf("enrich: %d items", n)

	t, err := RunThemes(ctx, st, cl, cfg.ThemeModel, 90)
	if err != nil {
		return err
	}
	log.Printf("themes: %d clusters", t)
	return nil
}

// Scheduler runs RunAll on an interval until the context is cancelled.
func Scheduler(ctx context.Context, st *store.Store, cl *llm.Client, cfg *config.Config) {
	if cfg.RefreshEvery <= 0 {
		return
	}
	log.Printf("scheduler: refreshing every %s", cfg.RefreshEvery)
	ticker := time.NewTicker(cfg.RefreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := RunAll(ctx, st, cl, cfg); err != nil {
				log.Printf("scheduler run error: %v", err)
			}
		}
	}
}
