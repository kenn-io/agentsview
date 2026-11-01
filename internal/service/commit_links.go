package service

import (
	"context"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// CommitSessionLink is the shared commit provenance wire contract.
type CommitSessionLink = db.CommitLink
type CommitSession = db.CommitSession
type CommitTurn = db.CommitTurn

var (
	ErrInvalidCommitQuery     = db.ErrInvalidCommitQuery
	ErrCommitLookupTooLarge   = db.ErrCommitLookupTooLarge
	ErrCommitIndexUnavailable = db.ErrCommitIndexUnavailable
)

// CommitSessionsResponse is the HTTP/MCP envelope keyed by original requests.
type CommitSessionsResponse struct {
	Commits map[string][]CommitSessionLink `json:"commits"`
}
type SessionCommitsResponse struct {
	Commits []CommitSessionLink `json:"commits"`
}

func (b *directBackend) CommitSessions(ctx context.Context, shas []string) (map[string][]CommitSessionLink, error) {
	if err := db.ValidateCommitSHAs(shas); err != nil {
		return nil, err
	}
	out := map[string][]CommitSessionLink{}
	if len(shas) == 0 {
		return out, nil
	}
	rows, err := b.db.CommitLinks(ctx, shas, "")
	if err != nil {
		return nil, err
	}
	total := 0
	for _, sha := range shas {
		if _, ok := out[sha]; ok {
			continue
		}
		links := []CommitSessionLink{}
		for _, row := range rows {
			if db.CommitSHAMatches(row, sha) {
				total++
				if total > db.MaxCommitLinks {
					return nil, ErrCommitLookupTooLarge
				}
				links = append(links, row)
			}
		}
		db.MarkCommitAmbiguity(links, []string{sha})
		db.SortCommitLinks(links)
		out[sha] = links
	}
	if err := db.CheckCommitResponseSize(CommitSessionsResponse{Commits: out}); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *directBackend) SessionCommits(ctx context.Context, id string) ([]CommitSessionLink, error) {
	if strings.TrimSpace(id) == "" || len(id) > 1024 {
		return nil, ErrInvalidCommitQuery
	}
	rows, err := b.db.CommitLinks(ctx, nil, id)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []CommitSessionLink{}
	}
	if len(rows) > db.MaxCommitLinks {
		return nil, ErrCommitLookupTooLarge
	}
	if err := db.CheckCommitResponseSize(SessionCommitsResponse{Commits: rows}); err != nil {
		return nil, err
	}
	return rows, nil
}
