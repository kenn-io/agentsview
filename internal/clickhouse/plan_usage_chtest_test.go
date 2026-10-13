//go:build chtest

package clickhouse

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func chPlanUsage(weekly float64) *db.SessionPlanUsage {
	return &db.SessionPlanUsage{
		HasPlanUsage:  true,
		DataAsOf:      "2026-10-01T10:00:00.123456789Z",
		DataStatus:    "complete",
		UsageSource:   "query_v2",
		WeeklyPercent: new(weekly),
		Credits:       new("1.5"),
		Groups: []db.SessionPlanUsageGroup{
			{Model: "gpt-5-codex", ReasoningEffort: "high", Speed: "standard", WeeklyPercent: new(weekly), FiveHourPercent: new(0.5)},
		},
		Final:           true,
		OutcomeKind:     "left_out",
		OutcomeAt:       "2026-10-02T08:30:00Z",
		CountedUnder:    "",
		HasCoverage:     true,
		LastCaptureAt:   "2026-10-01T09:59:00.5Z",
		UncapturedTurns: 4,
	}
}

func pushedStoreWithPlanUsage(t *testing.T, p *db.SessionPlanUsage) (*Store, *Sync, *db.DB, Target) {
	t.Helper()
	local, target := seedFixture(t)
	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), fixtureAlphaID, p))
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := s.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, s, local, target
}

func TestClickHousePlanUsageRoundTrip(t *testing.T) {
	ctx := t.Context()
	store, s, local, _ := pushedStoreWithPlanUsage(t, chPlanUsage(12.5))

	got, err := store.GetSessionPlanUsage(ctx, fixtureAlphaID)
	require.NoError(t, err)
	assert.Equal(t, chPlanUsage(12.5), got)
	none, err := store.GetSessionPlanUsage(ctx, fixtureBetaID)
	require.NoError(t, err)
	assert.Nil(t, none, "a session without plan usage has no row")

	require.NoError(t, local.ReplaceSessionPlanUsage(ctx, fixtureAlphaID, chPlanUsage(40)))
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)
	got, err = store.GetSessionPlanUsage(ctx, fixtureAlphaID)
	require.NoError(t, err)
	assert.Equal(t, chPlanUsage(40), got, "a changed row replaces the pushed one")

	require.NoError(t, local.ReplaceSessionPlanUsage(ctx, fixtureAlphaID, nil))
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)
	got, err = store.GetSessionPlanUsage(ctx, fixtureAlphaID)
	require.NoError(t, err)
	assert.Nil(t, got, "a row removed from the archive must leave the mirror")
}

func TestClickHouseDeleteClearsPlanUsage(t *testing.T) {
	ctx := t.Context()
	_, s, local, target := pushedStoreWithPlanUsage(t, chPlanUsage(12.5))
	conn := chtest.Open(t, target.URL, target.Database)
	require.Equal(t, 1, chtest.Count(t, conn, "session_plan_usage", "session_id = ?", fixtureAlphaID))

	require.NoError(t, local.SoftDeleteSession(t.Context(), fixtureAlphaID))
	deleted, err := local.DeleteSessionIfTrashed(t.Context(), fixtureAlphaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Zero(t, chtest.Count(t, conn, "session_plan_usage", "session_id = ?", fixtureAlphaID))
}

func TestClickHouseSessionUsageIncludesPlanUsage(t *testing.T) {
	store, _, _, _ := pushedStoreWithPlanUsage(t, chPlanUsage(12.5))
	usage, err := store.GetSessionUsage(t.Context(), fixtureAlphaID, false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, chPlanUsage(12.5), usage.PlanUsage)
}

// TestClickHouseSameSessionFromTwoMachines records what the mirror holds
// when two archives push the same codex-cloud thread with different plan
// usage. Session IDs come from the thread ID, so both archives use one ID.
// The mirror has no owner check: the latest push replaces the session and
// its plan usage, and an archive with no local change does not push again.
func TestClickHouseSameSessionFromTwoMachines(t *testing.T) {
	ctx := t.Context()
	const id = "codex-cloud:shared-thread"
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	type machine struct {
		local *db.DB
		sync  *Sync
	}
	open := func(name string, weekly float64) machine {
		// A fresh archive, not the shared test template, so each machine
		// has its own archive ID as two real installs do.
		local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, local.Close()) })
		sess := fixtureSession(id, "cloud", "shared thread", "2026-10-01T09:00:00.000Z", 0)
		sess.Agent = "codex-cloud"
		_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
			Session: sess, DataVersion: 1, ReplaceMessages: true, PlanUsage: chPlanUsage(weekly),
		}})
		require.NoError(t, err)
		s, err := New(ctx, target, local, name, storage.PusherOptions{})
		require.NoError(t, err)
		require.NoError(t, s.EnsureSchema(ctx))
		t.Cleanup(func() { s.Close() })
		return machine{local, s}
	}
	a := open("machine-a", 10)
	b := open("machine-b", 20)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	conn := chtest.Open(t, target.URL, target.Database)
	read := func() (*float64, string) {
		t.Helper()
		got, err := store.GetSessionPlanUsage(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got)
		var machine string
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT machine FROM sessions WHERE id = ?", id).Scan(&machine))
		return got.WeeklyPercent, machine
	}

	_, err = a.sync.Push(ctx, false, nil)
	require.NoError(t, err)
	weekly, owner := read()
	assert.Equal(t, new(10.0), weekly)
	assert.Equal(t, "machine-a", owner)

	result, err := b.sync.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed, "the second archive is not refused")
	weekly, owner = read()
	assert.Equal(t, new(20.0), weekly, "the latest push replaces plan usage")
	assert.Equal(t, "machine-b", owner)
	assert.Equal(t, 1, chtest.Count(t, conn, "session_plan_usage", "session_id = ?", id),
		"the mirror keeps one row for the thread, not one per machine")

	result, err = a.sync.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, result.SessionsPushed, "an unchanged archive does not push the session again")
	weekly, _ = read()
	assert.Equal(t, new(20.0), weekly)

	require.NoError(t, a.local.ReplaceSessionPlanUsage(ctx, id, chPlanUsage(30)))
	_, err = a.sync.Push(ctx, false, nil)
	require.NoError(t, err)
	weekly, owner = read()
	assert.Equal(t, new(30.0), weekly, "a local change on the first archive takes the session back")
	assert.Equal(t, "machine-a", owner)
}
