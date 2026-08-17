package db

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

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

		attempt.Results = hits[:0]
		for _, hit := range hits {
			if hit.SessionID == "" {
				continue
			}
			attempt.Results = append(attempt.Results, hit)
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
	capability := s.backend.Capabilities().SessionSearch
	if capability == nil {
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
