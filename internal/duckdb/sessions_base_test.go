//go:build !(windows && arm64)

package duckdb

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/readbase"
)

func TestSessionReadErrors(t *testing.T) {
	cause := errors.New("read failed")
	backend := sessionsSQL{Store: &Store{}}
	tests := []struct {
		operation readbase.ReadOperation
		text      string
		raw       bool
	}{
		{readbase.QueryMessages, "querying duckdb messages: read failed", false},
		{readbase.QueryAroundMessages, "querying duckdb around-window messages: read failed", false},
		{readbase.QueryAllMessages, "querying all duckdb messages: read failed", false},
		{readbase.ScanAllMessages, "read failed", true},
		{readbase.QueryToolCalls, "read failed", true},
		{readbase.ScanToolCall, "read failed", true},
		{readbase.QueryToolResultEvents, "read failed", true},
		{readbase.ScanToolResultEvent, "read failed", true},
		{readbase.ListStarredSessions, "read failed", true},
		{readbase.ListPinnedMessages, "listing duckdb pinned messages: read failed", false},
		{readbase.ReadMachineMetadata, "reading machine metadata: read failed", false},
		{readbase.ListFindings, "listing duckdb findings: read failed", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.operation), func(t *testing.T) {
			err := backend.ReadError(tt.operation, cause)
			require.ErrorIs(t, err, cause)
			require.EqualError(t, err, tt.text)
			if tt.raw {
				assert.Same(t, cause, err)
			}
		})
	}
}
