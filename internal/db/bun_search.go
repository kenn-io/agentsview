package db

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

type bunSearchSession struct {
	ID           string              `bun:"id"`
	Project      string              `bun:"project"`
	Agent        string              `bun:"agent"`
	FirstMessage *string             `bun:"first_message"`
	DisplayName  *string             `bun:"display_name"`
	SessionName  *string             `bun:"session_name"`
	CreatedAt    bunmodel.Timestamp  `bun:"created_at"`
	StartedAt    *bunmodel.Timestamp `bun:"started_at"`
	EndedAt      *bunmodel.Timestamp `bun:"ended_at"`
}

// HasFTS reports whether the adapter has an available lexical capability.
func (s *BunStore) HasFTS() bool {
	capability := s.backend.Capabilities().FullText
	return capability != nil && capability.Available()
}

// Search resolves engine-specific lexical hits through canonical session rows.
func (s *BunStore) Search(
	ctx context.Context, filter SearchFilter,
) (SearchPage, error) {
	if filter.Limit <= 0 || filter.Limit > MaxSearchLimit {
		filter.Limit = DefaultSearchLimit
	}
	filter.Query = PrepareFTSQuery(filter.Query)
	if filter.Query == "" {
		return SearchPage{}, nil
	}
	capability := s.backend.Capabilities().FullText
	if capability == nil || !capability.Available() {
		return SearchPage{}, errFTSUnavailable
	}

	var page SearchPage
	err := s.consistentView(ctx, func(store bun.IDB) error {
		attempt := SearchPage{}
		capabilityFilter := filter
		capabilityFilter.Limit = filter.Limit + 1
		hits, err := capability.Search(ctx, store, capabilityFilter)
		if err != nil {
			return fmt.Errorf("searching full-text capability: %w", err)
		}
		if len(hits) == 0 {
			page = attempt
			return nil
		}

		ids := make([]string, 0, len(hits))
		seen := make(map[string]struct{}, len(hits))
		for _, hit := range hits {
			if hit.SessionID == "" {
				continue
			}
			if _, ok := seen[hit.SessionID]; ok {
				continue
			}
			seen[hit.SessionID] = struct{}{}
			ids = append(ids, hit.SessionID)
		}
		if len(ids) == 0 {
			page = attempt
			return nil
		}

		var rows []bunSearchSession
		query := store.NewSelect().Table("sessions").
			Column("id", "project", "agent", "first_message", "display_name", "session_name").
			Column("created_at", "started_at", "ended_at").
			Where("id IN (?)", bun.List(ids)).
			Where("deleted_at IS NULL")
		if filter.Project != "" {
			query = query.Where("project = ?", filter.Project)
		}
		if err := query.Scan(ctx, &rows); err != nil {
			return fmt.Errorf("hydrating search sessions: %w", err)
		}
		byID := make(map[string]bunSearchSession, len(rows))
		for _, row := range rows {
			byID[row.ID] = row
		}
		attempt.Results = make([]SearchResult, 0, min(len(rows), filter.Limit+1))
		for _, hit := range hits {
			row, ok := byID[hit.SessionID]
			if !ok {
				continue
			}
			attempt.Results = append(attempt.Results, SearchResult{
				SessionID: row.ID, Project: row.Project, Agent: row.Agent,
				Name: searchSessionName(row), Ordinal: hit.Ordinal,
				SessionEndedAt: searchSessionActivity(row),
				Snippet:        hit.Snippet, Rank: hit.Rank,
			})
			delete(byID, hit.SessionID)
		}
		if len(attempt.Results) > filter.Limit {
			attempt.Results = attempt.Results[:filter.Limit]
			attempt.NextCursor = filter.Cursor + filter.Limit
		}
		page = attempt
		return nil
	})
	return page, err
}

// SearchSession delegates lexical matching while retaining the backend guard.
func (s *BunStore) SearchSession(
	ctx context.Context, sessionID, query string,
) ([]int, error) {
	if query == "" {
		return nil, nil
	}
	capability := s.backend.Capabilities().FullText
	if capability == nil || !capability.Available() {
		return nil, errFTSUnavailable
	}
	var ordinals []int
	err := s.consistentView(ctx, func(store bun.IDB) error {
		attempt, err := capability.SearchSession(ctx, store, sessionID, query)
		if err != nil {
			return fmt.Errorf("searching session full-text capability: %w", err)
		}
		ordinals = append([]int(nil), attempt...)
		return nil
	})
	return ordinals, err
}

func searchSessionName(row bunSearchSession) string {
	for _, value := range []*string{row.DisplayName, row.SessionName, row.FirstMessage} {
		if value != nil && *value != "" {
			return *value
		}
	}
	return ""
}

func searchSessionActivity(row bunSearchSession) string {
	for _, value := range []*bunmodel.Timestamp{row.EndedAt, row.StartedAt, &row.CreatedAt} {
		if value != nil && !value.IsZero() {
			return value.UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}
