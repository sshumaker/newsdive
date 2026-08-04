# Design

Signal is a personal research newsreader for frontier LLM/agents work. This
document explains the shape of the system and the decisions behind it.

## Goals (from the brief)

1. Discover & ingest new papers/posts from arXiv, blogs, HN, and feeds.
2. Summarize each item and extract ideas, claims, evidence, limitations, open questions.
3. Group related work into recurring themes and show how ideas evolve.
4. Rank by relevance, novelty, importance.
5. A clean interface to scan and drill into items; preserve source links.
6. Save / dismiss / mark read / give feedback.
7. Search + natural-language Q&A across the collection.
8. **The point:** aggregate ideas *across* papers — surface connections,
   disagreements, and emerging themes — not just summarize each in isolation.

## Shape of the system

A single Go binary with an embedded web UI, backed by one SQLite file.

```
sources ─▶ ingest ─▶ [ SQLite ] ─▶ enrich (Claude) ─▶ themes (Claude)
(arXiv,HN,RSS)          ▲   │                                  │
                        │   └───────────────▶ HTTP API ◀───────┘
                        │                        │
                     ask agent  ◀────────────────┤
                 (Claude+web+corpus)         mobile web UI
```

- **`internal/sources`** — one fetcher per source. arXiv uses topical
  abstract/title searches (keeps the ingested set on-topic instead of dumping
  whole categories); HN uses the Algolia `search_by_date` API with a points
  floor; blogs are a curated RSS/Atom list. Each item gets a stable ID = hash of
  its canonical URL, so re-ingesting is idempotent.
- **`internal/store`** — SQLite via the pure-Go `modernc.org/sqlite` driver (no
  cgo → trivial static builds and Docker images). One `items` table, one
  `themes` table, and an FTS5 virtual table for search. WAL mode + a single
  writer connection keeps concurrency simple and safe.
- **`internal/llm`** — a thin raw-HTTP client for the Anthropic Messages API
  plus three operations: `Enrich`, `SynthesizeThemes`, `Ask`.
- **`internal/pipeline`** — composes ingest → enrich → theme, plus a background
  scheduler.
- **`internal/server`** — JSON API + serves the embedded `web/` UI.
- **`web/`** — a dependency-free, mobile-first SPA (vanilla HTML/CSS/JS),
  embedded into the binary with `go:embed`.

## Key decisions

**Go + SQLite + embedded UI, one binary.** The brief asked for simple and
maintainable. A single statically-linked binary that serves its own UI and
stores everything in one file is about as simple as a full-stack app gets to
run, back up, and deploy. `modernc.org/sqlite` is pure Go so there's no cgo and
the Docker image is tiny.

**No vector database; LLM-driven grouping.** Rather than stand up embeddings +
a vector store for clustering and Q&A, themes are produced by asking Claude to
cluster recent items and *synthesize across them* (connections / disagreements /
trajectory), and search is FTS5. This is less machinery, and it directly serves
goal #8 — the value is the cross-item synthesis, which an embedding cosine score
doesn't give you. Retrieval for the ask agent is FTS5 + the agent's own web
tools, which is plenty at personal scale and keeps the system to a single
dependency and a single API provider.

**Ranking lives in code, not the LLM.** The LLM produces three interpretable
0–100 signals (relevance/novelty/importance); the final feed score is a
transparent, tunable formula in `store/rank.go` (relevance-weighted base + a
10-day-half-life recency term + small popularity and feedback nudges). Keeping
ranking deterministic and inspectable beats a black-box score, and recency stays
fresh because the score is recomputed at read time.

**Two agent surfaces: a Managed Agents chat, and a one-shot `ask`.**

The primary conversational surface is a **page-aware chat** built on Anthropic
**Managed Agents** (`internal/agent`). A single persisted agent + reusable
environment are created once and cached in the `settings` table; each chat is a
stateful **session**. The agent has three tools:
- `read_current_topic` (custom, client-side) — reads the item or theme the
  reader is currently viewing. The frontend sends the current page context with
  every message, so "this paper" / "this theme" resolves correctly.
- `search_corpus` (custom, client-side) — related work already in the collection.
- `web_search` + `web_fetch` (built-in Managed Agents toolset) — the real
  sources on the public web.

Managed Agents fits the chat use case: it owns the agent loop and session state,
so multi-turn conversations "just work" and the agent is a versioned, reusable
object. The Go side is a small raw-HTTP client (`internal/agent/client.go`) plus
an orchestrator that polls the session event stream and answers custom-tool
calls (`chat.go`) — matching the app's dependency-light style. Event-type
strings are matched by suffix so minor naming differences don't break parsing.

A lighter **one-shot `ask`** (`internal/llm/ask.go`) also remains — a single
Messages-API tool loop (`search_corpus` + `web_search` + `web_fetch`) exposed on
the CLI and `/api/ask`. It's handy for a quick cited answer without a session,
and degrades to corpus-only if web tools aren't enabled on the account.

**Model choice.** Batch enrichment and theme clustering default to
`claude-sonnet-5` (high volume → cost matters, and structured extraction is well
within Sonnet's range) at `low`/`medium` effort. The interactive ask agent
defaults to `claude-opus-5` — it's on-demand, benefits from stronger reasoning
and tool use, and is where depth pays off. All three are single env knobs
(`NEWS_ENRICH_MODEL`, `NEWS_THEME_MODEL`, `NEWS_ASK_MODEL`) if you want to bump
enrichment to Opus.

**Structured outputs everywhere it matters.** Enrichment and theme synthesis use
the Messages API `output_config.format` JSON-schema mode, so parsing is
guaranteed and there's no brittle string-scraping.

## Data model

`items`: identity + source metadata, the raw abstract/excerpt, the enrichment
fields (summary, key_ideas, claims, evidence, limitations, open_questions, tags),
the three scores, reader `state` (unread/read/saved/dismissed) and `feedback`
(−1/0/+1), and a `theme_id`. `themes`: title, description, and the three
synthesis fields. `items_fts`: FTS5 over title + body + tags.

A small `settings` key/value table holds the cached Managed Agents agent/env IDs
and the user-editable **sources config** (`sources_config`): Bluesky handles and
extra RSS/Atom feeds, managed from the settings panel and merged on top of the
built-in arXiv/HN/blog sources at ingest time. Bluesky posts come from the
public AppView API (`app.bsky.feed.getAuthorFeed`) — no key required.

Re-theming is a full atomic replace (`ReplaceThemes`) — themes are a derived
view of the current collection, so recomputing them wholesale is simpler and
avoids drift.

## Trade-offs & what's deliberately out of scope for the MVP

- **No auth / multi-user.** It's a personal tool; add a reverse proxy or a shared
  secret if hosting publicly.
- **Blog/HN items carry only an excerpt/title**, so their enrichment is shallower
  than an arXiv abstract's. The ask agent compensates by fetching full text on
  demand.
- **Feedback is stored and nudges ranking**, but there's no learned interest
  model yet — a natural next step is to periodically fold thumbs up/down into the
  `NEWS_INTERESTS` profile.
- **Themes are recomputed wholesale**, so theme IDs aren't stable across runs;
  fine for browsing, not for deep-linking a specific theme over time.
- **Ranking candidate set is capped at 800** items in `Feed` before in-Go
  ranking — comfortable for a personal collection, would need SQL-side ranking
  at much larger scale.
