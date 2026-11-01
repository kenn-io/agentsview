package service_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestCommitSessionsOriginalKeysAndEmptyArrays(t *testing.T) {
	svc, env := newDirectTestSvc(t)
	require.NoError(t, env.db.StoreCommitLinks(t.Context(), []db.CommitLink{{SessionID: "unknown", CommitSHA: "abcdef0123456789012345678901234567890123", Resolved: true, Source: "trailer"}}))
	got, err := svc.CommitSessions(t.Context(), []string{"ABCDEF0", "0000000", "abcdef0"})
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Len(t, got["ABCDEF0"], 1)
	assert.Len(t, got["abcdef0"], 1)
	require.NotNil(t, got["0000000"])
	assert.Empty(t, got["0000000"])
	_, err = svc.CommitSessions(t.Context(), []string{"not sha"})
	assert.ErrorIs(t, err, service.ErrInvalidCommitQuery)
	got, err = svc.CommitSessions(t.Context(), nil)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestCommitResponseSizeLimit(t *testing.T) {
	assert.NoError(t, db.CheckCommitResponseSize(map[string]string{"small": "ok"}))
	assert.ErrorIs(t, db.CheckCommitResponseSize(strings.Repeat("x", db.MaxCommitResponseBytes)), service.ErrCommitLookupTooLarge)
}
