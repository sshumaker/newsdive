package store

import (
	"math"
	"sort"
	"time"
)

// Rank computes a recency-adjusted display score for each item in place and
// sorts the slice best-first.
//
// The base signal blends the three LLM judgments (relevance weighted highest,
// since this is a personal reader), then mixes in a recency term with a 10-day
// half-life, small nudges for source popularity (HN points) and the reader's
// own thumbs up/down. Unenriched items fall back to recency + popularity only.
func Rank(items []*Item) {
	now := time.Now()
	for _, it := range items {
		var base float64
		if it.Enriched {
			base = 0.45*float64(it.Relevance) + 0.30*float64(it.Importance) + 0.25*float64(it.Novelty)
		} else {
			base = 35 // unranked-but-present baseline; recency does the rest
		}

		ageDays := now.Sub(it.PublishedAt).Hours() / 24
		if it.PublishedAt.IsZero() || ageDays < 0 {
			ageDays = 0
		}
		recency := 100 * math.Pow(0.5, ageDays/10.0)

		score := 0.70*base + 0.30*recency

		// Popularity prior: HN points, saturating.
		if it.Points > 0 {
			score += math.Min(10, math.Log10(float64(it.Points)+1)*5)
		}
		// Explicit reader feedback dominates.
		score += float64(it.Feedback) * 20

		it.Score = math.Round(score*10) / 10
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].Score != items[b].Score {
			return items[a].Score > items[b].Score
		}
		return items[a].PublishedAt.After(items[b].PublishedAt)
	})
}
