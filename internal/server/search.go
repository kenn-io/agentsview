package server

import (
	"go.kenn.io/agentsview/internal/db"
)

type searchResponse struct {
	Query   string            `json:"query"`
	Results []db.SearchResult `json:"results"`
	Count   int               `json:"count"`
	Next    int               `json:"next"`
}

// prepareFTSQuery turns a raw search query into a well-formed FTS5 MATCH
// expression. It delegates to db.PrepareFTSQuery so the HTTP, SQLite, and
// PostgreSQL search paths all share one quoting/semantics implementation:
// each term is quoted (making operator characters like '-' and ':' literal so
// a single token never 500s), terms combine under implicit AND, and an
// exact phrase remains opt-in via a leading double quote.
func prepareFTSQuery(raw string) string {
	return db.PrepareFTSQuery(raw)
}
