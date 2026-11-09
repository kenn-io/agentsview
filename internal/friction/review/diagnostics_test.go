package review_test

import (
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
)

func TestDiagnosticsCatchUpRebuildsOldDay(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	r := newRunner(d)
	r.Diagnostics = d
	when := time.Date(2026, 8, 30, 23, 55, 0, 0, time.UTC)
	for range 3 {
		_, err := d.AppendDiagnostic(t.Context(), db.DiagnosticEvent{ID: uuid.NewV7().String(), Source: "worker", Message: "launch failed", OccurredAt: when})
		require.NoError(t, err)
	}
	_, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	assert.False(t, friction.Fileable(friction.KindDiagnostic))
	digest := stored(t, d, "2026-08-30")
	require.NotNil(t, digest)
	assert.Contains(t, string(digest.Markdown), "signals_captured: 3\n")
	assert.Contains(t, string(digest.Markdown), "`worker`: `launch failed` x3")
	assert.Equal(t, 0, digest.SessionsScanned)
	_, err = d.AppendDiagnostic(t.Context(), db.DiagnosticEvent{ID: uuid.NewV7().String(), Source: "worker", Message: "launch failed", OccurredAt: when})
	require.NoError(t, err)
	_, err = r.CatchUp(t.Context())
	require.NoError(t, err)
	rebuilt := stored(t, d, "2026-08-30")
	assert.Contains(t, string(rebuilt.Markdown), "`worker`: `launch failed` x4")
	assert.Equal(t, 1, strings.Count(string(rebuilt.Markdown), "`worker`:"))
	assert.Greater(t, rebuilt.Revision, digest.Revision)
	_, err = r.CatchUp(t.Context())
	require.NoError(t, err)
	assert.Equal(t, rebuilt.Revision, stored(t, d, "2026-08-30").Revision)
}
