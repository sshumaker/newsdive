# Signal — a personal LLM/agents research newsreader

newsdive ingests recent work on LLMs, agents, harnesses, coding agents,
inference-time compute, tool use, and program synthesis from **arXiv, Hacker
News, research blogs, and Bluesky**; uses Claude to **summarize, extract, and
score** each item; **groups related work into themes** with cross-paper
synthesis; and serves a **mobile-first** reading UI with search and a
**page-aware chat agent** (built on Anthropic **Managed Agents**) you can pop
open anywhere to talk about what you're looking at.

It's a single Go binary (with an embedded web UI) backed by SQLite. No external
services beyond the Anthropic API.

## What it does

- **Ingest** — pulls candidate items from arXiv (topical searches), the Hacker
  News Algolia API, a curated set of research blogs (RSS/Atom), and any
  **Bluesky** accounts + extra feeds you configure in the settings panel (⚙).
- **Enrich** — for each item Claude writes a one-liner + summary and extracts
  **key ideas, claims, evidence, limitations, and open questions**, plus tags
  and 0–100 scores for **relevance / novelty / importance**.
- **Rank** — a recency-adjusted blend of those scores (relevance-weighted), with
  small nudges for source popularity and your own 👍/👎.
- **Themes** — Claude clusters recent items into cross-cutting themes, each with
  a synthesis of **connections, disagreements, and where the work is heading** —
  the "aggregate ideas across papers" layer, not just per-item summaries.
- **Read** — scan a ranked feed, tap into a full breakdown, **save / dismiss /
  mark read / thumbs up-down**, and browse by theme. Links back to every source.
- **Search** — full-text (SQLite FTS5) over titles, summaries, ideas, and tags.
- **Chat (✦ FAB)** — a page-aware chat agent built on Anthropic **Managed
  Agents**. Tap the floating button on any screen; it knows whether you're on an
  item, a theme, or the feed. Tools: `read_current_topic` (the item/theme you're
  viewing), `search_corpus` (related work in your collection), and
  `web_search`/`web_fetch` (the real sources on the public web). Each chat is a
  stateful Managed Agents session.

See [DESIGN.md](DESIGN.md) for the architecture and the decisions behind it.

## Quick start (local)

Requires Go 1.23+ and an Anthropic API key.

```bash
cd news
echo 'ANTHROPIC_API_KEY=sk-ant-...' > .env      # your key

go build -o bin/news ./cmd/news

./bin/news all       # ingest + enrich + build themes (first run: a few minutes)
./bin/news serve     # open http://localhost:8787
```

That's it — open the URL on your phone (same Wi-Fi, use your machine's LAN IP)
or desktop. `all` can be re-run any time; only new items are fetched and
enriched.

### Commands

```
news serve            run the web server + API (default)
news ingest           fetch new items from all sources (no API key needed)
news enrich [N]       summarize/score up to N unenriched items (default 200)
news themes           re-cluster recent items into themes
news all              ingest + enrich + themes (one full cycle)
news chat "message" [itemID]   talk to the Managed Agents chat agent from the CLI
news ask "question"   one-shot research answer (web + corpus) from the CLI
news stats            print collection stats
```

### Secrets with Doppler (optional, for multi-machine dev)

Instead of a per-machine `.env`, you can keep `ANTHROPIC_API_KEY` in
[Doppler](https://www.doppler.com) and inject it at runtime:

```bash
brew install dopplerhq/cli/doppler   # once
doppler login                        # interactive browser auth
./scripts/doppler-setup.sh           # creates the 'newsdive' project, seeds from .env

doppler run -- ./bin/news serve      # run with secrets injected (no .env needed)
```

On a second machine the secret is already in Doppler — just:
`doppler login && doppler setup -p newsdive -c dev`, then `doppler run -- ...`.
The app reads `ANTHROPIC_API_KEY` from the environment either way, so `.env` and
Doppler are interchangeable.

### Configuration (env / `.env`)

| Variable | Default | Purpose |
|---|---|---|
| `ANTHROPIC_API_KEY` | — | **Required** for enrich / themes / ask / serve. |
| `PORT` | `8787` | HTTP port (Railway sets this automatically). |
| `NEWS_DB` | `data/news.db` | SQLite path. Point at a mounted volume in prod. |
| `NEWS_REFRESH_MINUTES` | `0` | If >0, the server ingests+enriches+re-themes on this interval. |
| `NEWS_SOURCE_LIMIT` | `40` | Max items each source query pulls per run. |
| `NEWS_ENRICH_MODEL` | `claude-sonnet-5` | Per-item enrichment (high volume → Sonnet). |
| `NEWS_THEME_MODEL` | `claude-sonnet-5` | Theme clustering. |
| `NEWS_ASK_MODEL` | `claude-opus-5` | The interactive research agent (depth + tools). |
| `NEWS_INTERESTS` | (built-in profile) | Your research interests, used for scoring/synthesis. |

Edit the built-in interest profile in `internal/config/config.go`
(`defaultInterests`) or override with `NEWS_INTERESTS` to retune relevance.
Edit the source lists in `internal/sources/*.go`.

## Deploy

### Railway (recommended — one service, serves API + UI)

The repo ships a `Dockerfile` and `railway.json`. In Railway:

1. New project → **Deploy from repo** (or `railway up`). It builds the Dockerfile.
2. Add a **Volume** mounted at `/data` (the Dockerfile sets `NEWS_DB=/data/news.db`).
3. Set variables: `ANTHROPIC_API_KEY`, and optionally `NEWS_REFRESH_MINUTES=360`.
   Railway injects `PORT` automatically.
4. First populate: open a Railway shell and run `/app/news all`, or just let the
   scheduler fill it in over the first few cycles.

The Go binary serves both the JSON API and the web UI, so a single Railway
service is all you need.

### Vercel (optional — static frontend, split from the API)

Host the `web/` folder as a static Vercel project and point it at the
Railway-hosted API: edit the one line in `web/index.html`:

```html
<script>window.API_BASE = "https://your-app.up.railway.app";</script>
```

(The API already sends permissive CORS headers.) For most people the
Railway-only monolith above is simpler.

## Notes

- **Cost control:** enrichment is the main spend and uses Sonnet at `low`
  effort. `NEWS_SOURCE_LIMIT` bounds how much each run pulls. The ask agent uses
  Opus with web tools and is only invoked on demand.
- **Data:** everything lives in one SQLite file (`data/news.db`), including
  reader state and feedback. Delete it to start fresh.
- The `.env` and `data/` directory are git-ignored.
