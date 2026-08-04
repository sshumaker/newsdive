// Package ingest fetches from all sources and persists new items.
package ingest

import (
	"log"

	"news/internal/sources"
	"news/internal/store"
)

type Report struct {
	Fetched  int
	Inserted int
	Errors   []error
}

// Run fetches from every source and upserts new items into the store.
func Run(st *store.Store, perSource int) (Report, error) {
	res := sources.FetchAll(perSource, st.GetSourcesConfig())
	rep := Report{Fetched: len(res.Items), Errors: res.Errors}
	for _, err := range res.Errors {
		log.Printf("ingest: source error: %v", err)
	}
	for _, it := range res.Items {
		inserted, err := st.Upsert(it)
		if err != nil {
			return rep, err
		}
		if inserted {
			rep.Inserted++
		}
	}
	return rep, nil
}
