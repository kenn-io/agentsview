// Package readbasetest provides session-read observations over real backends.
package readbasetest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/readbase"
)

// RecordingSessions records tool ordinals returned by queries during a session read.
type RecordingSessions struct {
	readbase.SessionsBackend
	T            testing.TB
	ToolOrdinals [][]int
}

func (s *RecordingSessions) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := s.SessionsBackend.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	columns, err := rows.Columns()
	require.NoError(s.T, err)
	if len(columns) < 2 || columns[1] != "call_index" {
		return rows, nil
	}
	// Consume the real result to observe fetched rows, then replay the same
	// query for the reader. Fixtures stay unchanged during the read.
	defer rows.Close()
	values := make([]any, len(columns))
	var ordinal int
	values[0] = &ordinal
	for i := 1; i < len(values); i++ {
		values[i] = new(any)
	}
	ordinals := []int{}
	for rows.Next() {
		require.NoError(s.T, rows.Scan(values...))
		ordinals = append(ordinals, ordinal)
	}
	require.NoError(s.T, rows.Err())
	require.NoError(s.T, rows.Close())
	s.ToolOrdinals = append(s.ToolOrdinals, ordinals)
	return s.SessionsBackend.QueryContext(ctx, query, args...)
}
