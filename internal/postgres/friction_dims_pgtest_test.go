//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPGPushFrictionFleetDims(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "machine-fleet-dims", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx))

	started := time.Now().UTC().Format(time.RFC3339)
	first := "fleet dims"
	const id = "fleet-dims-001"
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: id, Project: "p", Machine: "local", Agent: "claude",
		FirstMessage: &first, StartedAt: &started, MessageCount: 1,
	}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: first}}))

	read := func() db.FrictionSessionDims {
		t.Helper()
		d := db.FrictionSessionDims{SessionID: id}
		require.NoError(t, ps.pg.QueryRowContext(ctx,
			`SELECT seat, persona, channel, dims_source, review_excluded
			 FROM friction_session_dims WHERE session_id = $1`, id,
		).Scan(&d.Seat, &d.Persona, &d.Channel, &d.DimsSource, &d.ReviewExcluded))
		return d
	}

	steps := []struct {
		name string
		dims db.FrictionSessionDims
		hash string
	}{
		{"nanoclaw", db.FrictionSessionDims{SessionID: id, Persona: "helper", Channel: "general", DimsSource: "nanoclaw"}, "h1"},
		{"nanoclaw_plus_seat", db.FrictionSessionDims{SessionID: id, Seat: "seat-17", Persona: "helper", Channel: "general, ops", DimsSource: "nanoclaw+seat_pattern"}, "h2"},
		{"seat_only", db.FrictionSessionDims{SessionID: id, Seat: "seat-17", DimsSource: "seat_pattern"}, "h3"},
		{"excluded", db.FrictionSessionDims{SessionID: id, DimsSource: "nanoclaw", ReviewExcluded: true}, "h4"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			dims := step.dims
			require.NoError(t, local.ReplaceSessionFriction(ctx, id, nil, &dims, friction.RulesVersion, step.hash))
			_, err := ps.Push(ctx, false, nil)
			require.NoError(t, err)
			assert.Equal(t, step.dims, read())
		})
	}
}
