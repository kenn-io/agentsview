package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/nanoclaw"
	"go.kenn.io/agentsview/internal/nanoclaw/nanoclawtest"
)

func TestRefreshNanoClawFriction(t *testing.T) {
	t.Run("db_recovery_ends_fail_closed_exclusion", func(t *testing.T) {
		filter := nanoclaw.Filter{Exclude: []string{"reviewer"}}
		fx := newNanoClawFixture(t, filter, nil, "ag-1")
		refresher := nanoclaw.NewResolver(fx.data, "", filter)
		// No v2.db yet: with a filter configured, ag-1 fails closed.
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "s-1", nanoclawtest.SessionBody))
		dims, err := fx.db.SessionFrictionDims(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, dims)
		require.True(t, dims.ReviewExcluded)

		n, err := fx.engine.RefreshNanoClawFriction(t.Context(), refresher)
		require.NoError(t, err)
		assert.Equal(t, 1, n, "first run records the fingerprint and recomputes")
		n, err = fx.engine.RefreshNanoClawFriction(t.Context(), refresher)
		require.NoError(t, err)
		assert.Zero(t, n, "unchanged map: nothing to do")

		nanoclawtest.WriteV2DB(t, fx.data)
		n, err = fx.engine.RefreshNanoClawFriction(t.Context(), refresher)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		dims, err = fx.db.SessionFrictionDims(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, dims)
		assert.Equal(t, db.FrictionSessionDims{SessionID: id, Persona: "helper", Channel: "general", DimsSource: "nanoclaw"}, *dims)
		findings, err := fx.db.SessionFrictionFindings(t.Context(), id)
		require.NoError(t, err)
		assert.NotEmpty(t, findings, "the recomputed session is reviewed")
	})

	t.Run("rename_updates_stored_persona", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		refresher := nanoclaw.NewResolver(fx.data, "", nanoclaw.Filter{})
		dbPath := nanoclawtest.WriteV2DB(t, fx.data)
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "s-1", nanoclawtest.SessionBody))
		_, err := fx.engine.RefreshNanoClawFriction(t.Context(), refresher)
		require.NoError(t, err)

		nanoclawtest.Exec(t, dbPath, `UPDATE agent_groups SET name = 'assistant' WHERE id = 'ag-1'`)
		later := time.Now().Add(2 * time.Second)
		require.NoError(t, os.Chtimes(dbPath, later, later))
		n, err := fx.engine.RefreshNanoClawFriction(t.Context(), refresher)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		dims, err := fx.db.SessionFrictionDims(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, dims)
		assert.Equal(t, "assistant", dims.Persona)
	})

	t.Run("nil_resolver_is_a_no_op", func(t *testing.T) {
		fx := newEngineFixture(t)
		n, err := fx.engine.RefreshNanoClawFriction(t.Context(), nil)
		require.NoError(t, err)
		assert.Zero(t, n)
		v, err := fx.db.GetSyncState(t.Context(), NanoClawFingerprintKey)
		require.NoError(t, err)
		assert.Empty(t, v)
	})

	t.Run("sessions_outside_the_cell_are_left_alone", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		nanoclawtest.WriteV2DB(t, fx.data)
		outside := filepath.Join(t.TempDir(), "plain.jsonl")
		require.NoError(t, fx.db.UpsertSession(t.Context(), db.Session{
			ID: "plain", Project: "p", Machine: "local", Agent: "claude", FilePath: &outside,
		}))
		n, err := fx.engine.RefreshNanoClawFriction(t.Context(), nanoclaw.NewResolver(fx.data, "", nanoclaw.Filter{}))
		require.NoError(t, err)
		assert.Zero(t, n)
	})
}
