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
	digest := stored(t, d, "2026-08-30")
	require.NotNil(t, digest)
	assert.Equal(t, 0, digest.SessionsScanned)
	assert.Zero(t, rowCount(t, d, "friction_patterns"))
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
	t.Run("epoch", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		r := newRunner(d)
		r.Diagnostics = d
		var err error
		r.Loc, err = time.LoadLocation("America/Los_Angeles")
		require.NoError(t, err)
		_, err = d.AppendDiagnostic(t.Context(), db.DiagnosticEvent{ID: uuid.NewV7().String(), Source: "worker", Message: "launch failed", OccurredAt: time.Unix(0, 0)})
		require.NoError(t, err)
		_, err = r.CatchUp(t.Context())
		require.NoError(t, err)
		assert.Contains(t, string(stored(t, d, "1969-12-31").Markdown), "`worker`: `launch failed` x1")
		assert.NotNil(t, stored(t, d, "2026-09-21"))
	})
}
