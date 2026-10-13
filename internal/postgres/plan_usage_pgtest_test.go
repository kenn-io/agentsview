//go:build pgtest

package postgres

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const planUsageRootID = "codex-cloud:root-thread"

// planUsagePushFixture is a local archive holding one codex-cloud session
// and a Sync that pushes it into a fresh PG schema.
type planUsagePushFixture struct {
	local *db.DB
	sync  *Sync
	store *Store
}

func newPlanUsagePushFixture(t *testing.T, schema string) planUsagePushFixture {
	t.Helper()
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, pg.Close()) })
	_, err = pg.ExecContext(t.Context(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(t.Context(), pg, schema))

	local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, local.Close()) })
	started := "2026-10-01T09:00:00Z"
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{
		ID: planUsageRootID, Project: "cloud", Machine: "test-machine",
		Agent: "codex-cloud", StartedAt: &started, CreatedAt: started,
	}))

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })
	return planUsagePushFixture{
		local: local,
		sync: &Sync{
			pg: pg, local: local, machine: "test-machine",
			schema: schema, schemaDone: true,
		},
		store: store,
	}
}

func (f planUsagePushFixture) push(t *testing.T) {
	t.Helper()
	result, err := f.sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
}

func fullPlanUsage() *db.SessionPlanUsage {
	return &db.SessionPlanUsage{
		HasPlanUsage:    true,
		DataAsOf:        "2026-10-01T10:00:00.123456789Z",
		DataStatus:      "complete",
		UsageSource:     "query_v2",
		WeeklyPercent:   new(12.5),
		FiveHourPercent: nil,
		Credits:         new("0.25"),
		Groups: []db.SessionPlanUsageGroup{
			{Model: "gpt-5-codex", ReasoningEffort: "high", Speed: "standard", ProductExperience: "cloud", WeeklyPercent: new(10.0), Credits: new("0.25")},
			{Model: "gpt-5-codex-mini", ReasoningEffort: "low", Speed: "fast", WeeklyPercent: new(2.5), FiveHourPercent: new(7.75)},
		},
		Final:           true,
		OutcomeKind:     "left_out",
		OutcomeAt:       "2026-10-02T08:30:00Z",
		HasCoverage:     true,
		LastCaptureAt:   "2026-10-01T09:59:00.5Z",
		UncapturedTurns: 3,
	}
}

func TestPushPlanUsageRoundTrip(t *testing.T) {
	f := newPlanUsagePushFixture(t, "agentsview_push_plan_usage_roundtrip")
	require.NoError(t, f.local.ReplaceSessionPlanUsage(t.Context(), planUsageRootID, fullPlanUsage()))
	f.push(t)

	got, err := f.store.GetSessionPlanUsage(t.Context(), planUsageRootID)
	require.NoError(t, err)
	assert.Equal(t, fullPlanUsage(), got)

	require.NoError(t, f.local.ReplaceSessionPlanUsage(t.Context(), planUsageRootID, nil))
	f.push(t)
	got, err = f.store.GetSessionPlanUsage(t.Context(), planUsageRootID)
	require.NoError(t, err)
	assert.Nil(t, got, "a row removed from the archive must leave PG")
}

func TestPushPlanUsageChangeRepushes(t *testing.T) {
	f := newPlanUsagePushFixture(t, "agentsview_push_plan_usage_change")
	require.NoError(t, f.local.ReplaceSessionPlanUsage(t.Context(), planUsageRootID, fullPlanUsage()))
	f.push(t)
	var updatedBefore time.Time
	require.NoError(t, f.sync.pg.QueryRowContext(t.Context(), `SELECT updated_at FROM sessions WHERE id = $1`, planUsageRootID).Scan(&updatedBefore))

	// Only the plan usage changes: the session row and its messages
	// stay as they were, so nothing but plan usage can select it.
	changed := fullPlanUsage()
	changed.WeeklyPercent = new(40.0)
	changed.Final = false
	require.NoError(t, f.local.ReplaceSessionPlanUsage(t.Context(), planUsageRootID, changed))
	f.push(t)

	got, err := f.store.GetSessionPlanUsage(t.Context(), planUsageRootID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, new(40.0), got.WeeklyPercent)
	assert.False(t, got.Final)
	var updatedAfter time.Time
	require.NoError(t, f.sync.pg.QueryRowContext(t.Context(), `SELECT updated_at FROM sessions WHERE id = $1`, planUsageRootID).Scan(&updatedAfter))
	assert.True(t, updatedAfter.After(updatedBefore),
		"a plan-usage-only change must bump updated_at for PG read-mode watchers")
}

func TestPGSessionUsageIncludesPlanUsage(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		f := newPlanUsagePushFixture(t, "agentsview_push_plan_usage_session_usage")
		require.NoError(t, f.local.ReplaceSessionPlanUsage(t.Context(), planUsageRootID, fullPlanUsage()))
		f.push(t)
		usage, err := f.store.GetSessionUsage(t.Context(), planUsageRootID, false)
		require.NoError(t, err)
		require.NotNil(t, usage)
		assert.Equal(t, fullPlanUsage(), usage.PlanUsage)
	})
	t.Run("hosted", func(t *testing.T) {
		pf := newProjectionFixture(t)
		m, _ := pf.accept(t, "device-a", "capture-a", "")
		p := projectionOutcome("cloud thread")
		session := &p.Outcome.Results[0].Result.Session
		session.PlanUsage = &parser.ParsedPlanUsage{
			DataAsOf: new(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)), DataStatus: "partial",
			WeeklyPercent: new(3.5), Credits: new("0"),
		}
		session.CaptureCoverage = &parser.ParsedCaptureCoverage{UncapturedTurns: 2}
		require.NoError(t, pf.sink.Project(t.Context(), pf.lease(t, m), m, p))
		h, err := newHostedAdapter(pf.runtime, pf.tenant)
		require.NoError(t, err)
		want := &db.SessionPlanUsage{
			HasPlanUsage: true, DataAsOf: "2026-10-01T10:00:00Z", DataStatus: "partial",
			WeeklyPercent: new(3.5), Credits: new("0"), HasCoverage: true, UncapturedTurns: 2,
		}
		usage, err := h.GetSessionUsage(t.Context(), "codex:portable", false)
		require.NoError(t, err)
		require.NotNil(t, usage)
		assert.Equal(t, want, usage.PlanUsage)
		direct, err := h.GetSessionPlanUsage(t.Context(), "codex:portable")
		require.NoError(t, err)
		assert.Equal(t, want, direct)
	})
}

func TestHostedProjectionWritesPlanUsage(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	p := projectionOutcome("descendant thread")
	session := &p.Outcome.Results[0].Result.Session
	session.PlanUsage = &parser.ParsedPlanUsage{
		OutcomeKind: "counted_under", OutcomeAt: new(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)),
		CountedUnder: "codex:root",
	}
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, p))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.Equal(t, RawIdentityUnique, resolved.State)

	got, err := (&Store{pg: f.runtime}).GetSessionPlanUsage(t.Context(), resolved.SessionID)
	require.NoError(t, err)
	assert.Equal(t, &db.SessionPlanUsage{
		HasPlanUsage: true, OutcomeKind: "counted_under",
		OutcomeAt: "2026-10-02T00:00:00Z", CountedUnder: "codex:root",
	}, got, "the physical row keeps the source root ID; hosted reads map it")
	var target string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(),
		`SELECT target_alias FROM raw_session_links WHERE kind = 'usage_root'`).Scan(&target))
	assert.Equal(t, "codex:root", target)

	// No session codex:root exists, so hosted reads cannot name a public
	// root and must not leak the source ID.
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	hosted, err := h.GetSessionPlanUsage(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NotNil(t, hosted)
	assert.Empty(t, hosted.CountedUnder)
	usage, err := h.GetSessionUsage(t.Context(), "codex:portable", false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.NotNil(t, usage.PlanUsage)
	assert.Empty(t, usage.PlanUsage.CountedUnder)
}

func TestHostedGrandchildUsageRootMapped(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	root := projectionOutcome("root")
	child := linkChildOutcome()
	grandchild := projectionOutcome("grandchild")
	gs := &grandchild.Outcome.Results[0].Result.Session
	gs.ID, gs.SourceSessionID = "codex:grandchild", "grandchild"
	gs.ParentSessionID, gs.RelationshipType = "codex:child", "subagent"
	gs.PlanUsage = &parser.ParsedPlanUsage{OutcomeKind: "counted_under", CountedUnder: "codex:portable"}
	root.Outcome.Results = append(root.Outcome.Results, child.Outcome.Results[0], grandchild.Outcome.Results[0])
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, root))

	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	got, err := h.GetSessionPlanUsage(t.Context(), "codex:grandchild")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "codex:portable", got.CountedUnder,
		"a usage root two levels up must resolve to its public ID")
	usage, err := h.GetSessionUsage(t.Context(), "codex:grandchild", false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.NotNil(t, usage.PlanUsage)
	assert.Equal(t, "codex:portable", usage.PlanUsage.CountedUnder)
}

func TestHostedReprovisionAddsPlanUsageTable(t *testing.T) {
	f := newHostedFixture(t, "tenant-plan-usage-upgrade")
	_, err := f.admin.ExecContext(t.Context(), `DROP TABLE session_plan_usage`)
	require.NoError(t, err)
	require.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant),
		"a tenant schema without the table must fail its catalog check")

	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
}

func TestHostedRuntimeRequiresPlanUsageInsert(t *testing.T) {
	f := newProjectionFixture(t)
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	_, err := f.admin.ExecContext(t.Context(), `REVOKE INSERT ON session_plan_usage FROM "`+f.role+`"`)
	require.NoError(t, err)
	require.ErrorContains(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull), "privileges")
}
