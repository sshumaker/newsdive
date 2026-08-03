// Package server exposes the JSON API and serves the embedded mobile web UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"news/internal/config"
	"news/internal/llm"
	"news/internal/pipeline"
	"news/internal/store"
	"news/web"
)

type Server struct {
	st  *store.Store
	cl  *llm.Client
	cfg *config.Config

	mu        sync.Mutex
	refreshing bool
}

func New(st *store.Store, cl *llm.Client, cfg *config.Config) *Server {
	return &Server{st: st, cl: cl, cfg: cfg}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/feed", s.handleFeed)
	mux.HandleFunc("GET /api/item/{id}", s.handleItem)
	mux.HandleFunc("POST /api/item/{id}/state", s.handleSetState)
	mux.HandleFunc("POST /api/item/{id}/feedback", s.handleFeedback)
	mux.HandleFunc("GET /api/themes", s.handleThemes)
	mux.HandleFunc("GET /api/theme/{id}", s.handleTheme)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("POST /api/ask", s.handleAsk)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)

	// Static SPA (embedded).
	mux.Handle("/", http.FileServerFS(web.Files))

	return withCORS(mux)
}

// ---------- feed / items ----------

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := store.FeedOpts{
		State:      q.Get("state"),
		SavedOnly:  q.Get("saved") == "1" || q.Get("saved") == "true",
		SourceType: q.Get("source"),
		Query:      q.Get("q"),
		Limit:      atoi(q.Get("limit"), 40),
		Offset:     atoi(q.Get("offset"), 0),
	}
	if t := q.Get("theme"); t != "" {
		opts.ThemeID = int64(atoi(t, 0))
	}
	items, err := s.st.Feed(opts)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"items": items})
}

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	it, err := s.st.Get(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, it)
}

func (s *Server) handleSetState(w http.ResponseWriter, r *http.Request) {
	var body struct{ State string `json:"state"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	switch body.State {
	case "unread", "read", "saved", "dismissed":
	default:
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	if err := s.st.SetState(r.PathValue("id"), body.State); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var body struct{ Feedback int `json:"feedback"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.Feedback < -1 || body.Feedback > 1 {
		http.Error(w, "feedback must be -1, 0, or 1", http.StatusBadRequest)
		return
	}
	if err := s.st.SetFeedback(r.PathValue("id"), body.Feedback); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------- themes ----------

func (s *Server) handleThemes(w http.ResponseWriter, r *http.Request) {
	themes, err := s.st.Themes()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"themes": themes})
}

func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	id := int64(atoi(r.PathValue("id"), 0))
	t, err := s.st.ThemeWithItems(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, st)
}

// ---------- ask ----------

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var body struct{ Question string `json:"question"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Question == "" {
		http.Error(w, "question required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	searcher := func(query string, limit int) []llm.CorpusHit {
		items, err := s.st.Search(query, limit)
		if err != nil {
			log.Printf("corpus search: %v", err)
			return nil
		}
		hits := make([]llm.CorpusHit, 0, len(items))
		for _, it := range items {
			hits = append(hits, llm.CorpusHit{
				ID: it.ID, Title: it.Title, URL: it.URL, Source: it.Source,
				OneLiner: it.OneLiner, Summary: it.Summary, Tags: it.Tags,
				Published: it.PublishedAt.Format("2006-01-02"),
			})
		}
		return hits
	}

	ans, err := s.cl.Ask(ctx, s.cfg.AskModel, body.Question, searcher)
	if err != nil {
		if ans != nil && ans.Text != "" {
			writeJSON(w, ans) // partial answer is still useful
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, ans)
}

// ---------- refresh ----------

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.refreshing {
		s.mu.Unlock()
		writeJSON(w, map[string]any{"status": "already running"})
		return
	}
	s.refreshing = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.refreshing = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := pipeline.RunAll(ctx, s.st, s.cl, s.cfg); err != nil {
			log.Printf("refresh: %v", err)
		}
	}()
	writeJSON(w, map[string]any{"status": "started"})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	log.Printf("api error: %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
