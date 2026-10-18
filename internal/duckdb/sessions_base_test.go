//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
	"go.kenn.io/agentsview/internal/storage"
)

func TestMatchingMachineIDsFindsQuotedAndTrashedIDs(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	for _, id := range []string{"id-b", "id'quoted"} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Machine: id, Project: "project", Agent: "claude", ParentSessionID: new("missing-parent")}))
	}
	require.NoError(t, local.SoftDeleteSession(ctx, "id-b"))
	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())
	ids, err := store.GetMatchingMachineIDs(ctx, []string{"id-b", "id'quoted", "missing"})
	require.NoError(t, err)
	assert.Equal(t, []string{"id'quoted", "id-b"}, ids)
}

func TestMatchingMachineIDsWrapsReadErrors(t *testing.T) {
	store := NewStoreFromDB(openTestDuckDB(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := store.GetMatchingMachineIDs(ctx, []string{"id-a"})
	require.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "reading machine metadata:")
}

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
