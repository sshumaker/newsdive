// Package config loads runtime configuration from the environment and a local
// .env file, plus the static research-interest profile that drives relevance
// scoring, theme clustering, and the ask agent.
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AnthropicAPIKey string
	DBPath          string
	Port            string

	// Models. Batch enrichment/themes default to Sonnet (high volume, cost);
	// the interactive ask agent defaults to Opus (depth + tool use).
	EnrichModel string
	ThemeModel  string
	AskModel    string

	// Scheduler: how often the background loop ingests + enriches. Zero disables it.
	RefreshEvery time.Duration

	// How many candidate items each source pulls per ingest run.
	SourceLimit int

	// Interests describe what "relevant" means for this reader.
	Interests string
}

// Interests is the reader's research profile. It is injected into enrichment,
// theme, and ask prompts so scoring and synthesis are grounded in what this
// person actually cares about. Edit freely.
const defaultInterests = `This reader is a software engineer following the frontier of applied LLM research. They care most about:
- LLM agents, agent harnesses, and orchestration (tool-use loops, planning, multi-agent systems, memory)
- Coding agents and AI software engineering (SWE-bench-style work, code generation, program repair)
- Inference-time / test-time compute (reasoning, search, verification, self-consistency, scaling laws for thinking)
- Tool use and function calling, structured outputs, and reliability of tool-augmented models
- Program synthesis and neuro-symbolic methods
- Retrieval, context engineering, long-context, and prompt/context management
- Evaluation of agents and reasoning systems; benchmarks and their pitfalls
- Practical systems, harnesses, and engineering lessons (not pure theory)

They value: concrete methods, real evidence, honest limitations, and results that change how one builds systems.
They are less interested in: pure computer-vision, robotics hardware, unrelated ML theory, marketing/hype posts with no substance.`

// Load reads .env (if present) into the process environment, then builds Config.
func Load() (*Config, error) {
	loadDotEnv(".env")

	c := &Config{
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		DBPath:          getenv("NEWS_DB", "data/news.db"),
		Port:            getenv("PORT", "8787"),
		EnrichModel:     getenv("NEWS_ENRICH_MODEL", "claude-sonnet-5"),
		ThemeModel:      getenv("NEWS_THEME_MODEL", "claude-sonnet-5"),
		AskModel:        getenv("NEWS_ASK_MODEL", "claude-opus-5"),
		SourceLimit:     getenvInt("NEWS_SOURCE_LIMIT", 40),
		Interests:       getenv("NEWS_INTERESTS", defaultInterests),
	}

	mins := getenvInt("NEWS_REFRESH_MINUTES", 0)
	c.RefreshEvery = time.Duration(mins) * time.Minute

	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// loadDotEnv loads KEY=VALUE lines from path into the environment without
// overriding variables already set. Best-effort: a missing file is fine.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(line[len("export "):])
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, val)
			}
		}
	}
}
