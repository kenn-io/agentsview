//go:build pgtest

// This parity test proves that GetActivityReport returns an identical
// activity.Report from all three storage backends (SQLite, PostgreSQL,
// DuckDB) given the same underlying data. One SQLite fixture is built, then
// pushed to PostgreSQL and DuckDB through the production push paths
// (postgres.Sync / duckdb.Sync). All three stores are queried with the same
// filter and date and the resulting reports are deep-compared.
//
// It lives in internal/activity as an EXTERNAL test package
// (activity_test) rather than inside any backend package: postgres, duckdb,
// and db all import activity, so an internal activity test that imported
// them would form an import cycle. An external _test package is compiled
// separately and may import all three backends -- activity_test -> {db,
// postgres, duckdb} -> activity is acyclic. The pgtest build tag keeps it
// out of the package's default, backend-free test runs.
package activity_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	duckdbstore "go.kenn.io/agentsview/internal/duckdb"
	postgresstore "go.kenn.io/agentsview/internal/postgres"
)

// parityDate is a calendar day safely in the past relative to any realistic
// test-run wall clock. A past full day makes activity.Aggregate treat the
// report as complete (partial=false, effective_end == day_end, as_of=nil),
// removing the only source of nondeterminism: each backend calls
// time.Now().UTC() independently, so a current-or-future day could yield
// microsecond-different as_of/effective_end values across the three reports.
const parityDate = "2026-06-14"

// paritySchema is dedicated to this test so it never collides with the shared
// "agentsview" schema that internal/postgres pgtests create and drop. Go runs
// package tests concurrently, so a shared-schema DROP here could wipe another
// package's active schema mid-run.
const paritySchema = "agentsview_daily_report_parity_test"

// parityFixtureSession describes one seeded session and its message stream for
// the parity fixture. Assistant messages carry token_usage JSON so cost is
// exercised through the message-source usage path (no usage_events needed).
type parityFixtureSession struct {
	id        string
	project   string
	gitBranch string
	model     string
	// events is an ordered (role, RFC3339-timestamp) list. Assistant rows get
	// token_usage with the given outputTokens so they contribute cost.
	events       []parityEvent
	outputTokens int
	// relationship overrides the session's relationship_type ("" means
	// "root"); parent sets parent_session_id when non-empty. Subagent and
	// fork sessions must count in every backend's report so the cost
	// totals match daily usage.
	relationship string
	parent       string
}

type parityEvent struct {
	role string
	ts   string
	// model and outputTokens override the session defaults for this one
	// assistant message. They exist so the fixture can inject an
	// ineligible (synthetic-model) usage row that every backend must
	// exclude identically. Zero values mean "use the session defaults".
	model        string
	outputTokens int
	// claudeMessageID/claudeRequestID set the message's Claude dedup keys.
	// When the same pair recurs across sessions the usage union dedups to a
	// single first-seen-wins row, exercising the cross-backend ordering of
	// duplicate usage rows.
	claudeMessageID string
	claudeRequestID string
}

// parityFixture returns the sessions seeded into every backend. Two sessions
// overlap on parityDate across two projects and two models (driving peak
// concurrency 2 and multi-key breakdowns); a third, non-overlapping session
// adds a second interval to the alpha/model-x rollups so the breakdowns are
// non-trivial. Three more untimed sessions exercise the usage edge cases the
// dedup/primary-model fixes touched: a resumed/forked pair (parity-d/parity-e)
// sharing one Claude dedup key in the same second (whole-second vs fractional),
// and a zero-cost known-model session (parity-f). All timestamps are well
// inside the day so the report is a full, non-partial day.
func parityFixture() []parityFixtureSession {
	return []parityFixtureSession{
		{
			id: "parity-a", project: "alpha", gitBranch: "main", model: "model-x",
			outputTokens: 1200,
			events: []parityEvent{
				{role: "user", ts: parityDate + "T10:00:00Z"},
				{role: "assistant", ts: parityDate + "T10:02:00Z"},
				{role: "user", ts: parityDate + "T10:05:00Z"},
				{role: "assistant", ts: parityDate + "T10:07:00Z"},
			},
		},
		{
			// Same branch name "main" as parity-a but a different project; the
			// (project, branch) grain must keep them in separate buckets.
			id: "parity-b", project: "beta", gitBranch: "main", model: "model-y",
			outputTokens: 800,
			events: []parityEvent{
				{role: "user", ts: parityDate + "T10:01:00Z"},
				{role: "assistant", ts: parityDate + "T10:03:00Z"},
				{role: "user", ts: parityDate + "T10:06:00Z"},
				{role: "assistant", ts: parityDate + "T10:08:00Z"},
			},
		},
		{
			// Second branch within alpha, so a project carries multiple branches.
			id: "parity-c", project: "alpha", gitBranch: "feature-x", model: "model-x",
			outputTokens: 300,
			events: []parityEvent{
				{role: "user", ts: parityDate + "T14:00:00Z"},
				{role: "assistant", ts: parityDate + "T14:04:00Z"},
				// Ineligible usage row: a synthetic-model assistant message
				// carrying real tokens. Every backend's usage union must drop
				// model == '<synthetic>', so it contributes no cost or output
				// tokens. If any backend (notably DuckDB, which inlines its own
				// usage CTE) failed to exclude it, that backend's totals would
				// diverge and the deep-compare below would fail.
				{role: "assistant", ts: parityDate + "T14:06:00Z",
					model: "<synthetic>", outputTokens: 9999},
			},
		},
		{
			// Resumed/forked dedup pair: parity-d and parity-e share one
			// (claude_message_id, claude_request_id) in the same second, one
			// whole-second instant and one fractional. First-seen-wins dedup
			// keeps the earlier whole-second row (500 tokens) on every backend;
			// the later fractional duplicate (9000) is dropped. A text sort of
			// the timestamp would invert them ('.' < 'Z'), so this guards the
			// parsed-instant ordering across backends.
			id: "parity-d", project: "gamma", model: "model-x",
			outputTokens: 500,
			events: []parityEvent{
				{role: "assistant", ts: parityDate + "T11:00:00Z",
					claudeMessageID: "dup-m", claudeRequestID: "dup-r"},
			},
		},
		{
			id: "parity-e", project: "gamma", model: "model-x",
			outputTokens: 9000,
			events: []parityEvent{
				{role: "assistant", ts: parityDate + "T11:00:00.123Z",
					claudeMessageID: "dup-m", claudeRequestID: "dup-r"},
			},
		},
		{
			// Zero-cost usage-only session: a single known-model assistant
			// message with zero tokens (zero cost). Every backend must still
			// report model-x as the primary model rather than a blank one.
			id: "parity-f", project: "delta", model: "model-x",
			outputTokens: 0,
			events: []parityEvent{
				{role: "assistant", ts: parityDate + "T12:00:00Z"},
			},
		},
		{
			// Subagent of parity-a: a usage-only single message. Its tokens
			// must count in every backend (the report includes subagent
			// sessions so its cost matches daily usage) and its session row
			// must appear in BySession.
			id: "parity-sub", project: "alpha", model: "model-x",
			outputTokens: 250, relationship: "subagent", parent: "parity-a",
			events: []parityEvent{
				{role: "assistant", ts: parityDate + "T10:03:30Z"},
			},
		},
		{
			// Fork of parity-a with a unique-keyed usage row: a rewound
			// branch's messages are real spend that daily usage counts, so
			// every backend must count its tokens and list its session row.
			id: "parity-fork", project: "alpha", model: "model-x",
			outputTokens: 9000, relationship: "fork", parent: "parity-a",
			events: []parityEvent{
				{role: "assistant", ts: parityDate + "T10:09:00Z"},
			},
		},
		{
			// Fork replaying parity-a's dedup key (same Claude pair as the
			// parity-d/parity-e pair but scoped to this fork): its usage row
			// must collapse in every backend's dedup, contributing a session
			// row but no tokens or cost.
			id: "parity-fork-replay", project: "gamma", model: "model-x",
			outputTokens: 7777, relationship: "fork", parent: "parity-d",
			events: []parityEvent{
				{role: "assistant", ts: parityDate + "T11:00:02Z",
					claudeMessageID: "dup-m", claudeRequestID: "dup-r"},
			},
		},
	}
}

// seedParitySQLite builds the SQLite fixture: pricing rows for the two models
// plus the sessions and messages from parityFixture, all written through the
// public WriteSessionBatchAtomic path so the data matches what a real sync
// would push.
func seedParitySQLite(t *testing.T) *db.DB {
	t.Helper()
	local, err := db.Open(filepath.Join(t.TempDir(), "parity.sqlite"))
	require.NoError(t, err, "opening sqlite fixture")
	t.Cleanup(func() { require.NoError(t, local.Close()) })

	// Explicit pricing for both models so all three backends price the same
	// token amounts identically (the syncs copy model_pricing to PG/DuckDB).
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{
		{ModelPattern: "model-x", InputPerMTok: 3, OutputPerMTok: 15,
			CacheCreationPerMTok: 3.75, CacheReadPerMTok: 0.3},
		{ModelPattern: "model-y", InputPerMTok: 1, OutputPerMTok: 5,
			CacheCreationPerMTok: 1.25, CacheReadPerMTok: 0.1},
	}), "seeding pricing")

	var writes []db.SessionBatchWrite
	for _, fs := range parityFixture() {
		writes = append(writes, paritySessionWrite(fs))
	}
	_, err = local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err, "writing fixture sessions")
	return local
}

// paritySessionWrite turns one fixture session into a SessionBatchWrite. The
// session window spans its first and last event; assistant messages carry the
// model and token_usage so they feed the usage path.
func paritySessionWrite(fs parityFixtureSession) db.SessionBatchWrite {
	first := fs.events[0].ts
	last := fs.events[len(fs.events)-1].ts
	firstMsg := "parity " + fs.id
	relationship := fs.relationship
	if relationship == "" {
		relationship = "root"
	}
	sess := db.Session{
		ID:               fs.id,
		Project:          fs.project,
		GitBranch:        fs.gitBranch,
		Machine:          "local",
		Agent:            "claude",
		FirstMessage:     &firstMsg,
		StartedAt:        &first,
		EndedAt:          &last,
		CreatedAt:        first,
		LocalModifiedAt:  &first,
		MessageCount:     len(fs.events),
		UserMessageCount: 1,
		RelationshipType: relationship,
		DataVersion:      1,
	}
	if fs.parent != "" {
		parent := fs.parent
		sess.ParentSessionID = &parent
	}
	msgs := make([]db.Message, 0, len(fs.events))
	for i, ev := range fs.events {
		m := db.Message{
			SessionID:     fs.id,
			Ordinal:       i,
			Role:          ev.role,
			Content:       ev.role + " " + fs.id,
			Timestamp:     ev.ts,
			ContentLength: len(ev.role + " " + fs.id),
		}
		if ev.claudeMessageID != "" {
			m.ClaudeMessageID = ev.claudeMessageID
		}
		if ev.claudeRequestID != "" {
			m.ClaudeRequestID = ev.claudeRequestID
		}
		if ev.role == "assistant" {
			model := fs.model
			if ev.model != "" {
				model = ev.model
			}
			outputTokens := fs.outputTokens
			if ev.outputTokens != 0 {
				outputTokens = ev.outputTokens
			}
			m.Model = model
			usage, _ := json.Marshal(map[string]int{
				"input_tokens":  outputTokens / 2,
				"output_tokens": outputTokens,
			})
			m.TokenUsage = usage
			m.OutputTokens = outputTokens
			m.HasOutputTokens = true
		}
		msgs = append(msgs, m)
	}
	return db.SessionBatchWrite{
		Session:         sess,
		Messages:        msgs,
		DataVersion:     1,
		ReplaceMessages: true,
	}
}

// pushParityPostgres pushes the SQLite fixture to PostgreSQL via the production
// Sync and returns a read-only PG store over the same database. The schema is
// dropped before and after so the test is self-contained.
func pushParityPostgres(
	t *testing.T, ctx context.Context, local *db.DB,
) *postgresstore.Store {
	t.Helper()
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL not set; skipping cross-backend parity test")
	}
	dropParitySchema(t, pgURL)
	t.Cleanup(func() { dropParitySchema(t, pgURL) })

	ps, err := postgresstore.New(
		pgURL, paritySchema, local, "parity-machine", true,
		postgresstore.SyncOptions{},
	)
	require.NoError(t, err, "creating pg sync")
	t.Cleanup(func() { require.NoError(t, ps.Close()) })
	require.NoError(t, ps.EnsureSchema(ctx), "ensuring pg schema")

	res, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "pushing to pg")
	require.Equal(t, len(parityFixture()), res.SessionsPushed,
		"pg sessions pushed")

	store, err := postgresstore.NewStore(pgURL, paritySchema, true)
	require.NoError(t, err, "opening pg store")
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

// dropParitySchema removes the PG schema so each run starts clean.
func dropParitySchema(t *testing.T, pgURL string) {
	t.Helper()
	store, err := postgresstore.NewStore(pgURL, paritySchema, true)
	require.NoError(t, err, "opening pg for schema drop")
	defer func() { require.NoError(t, store.Close()) }()
	_, _ = store.DB().Exec("DROP SCHEMA IF EXISTS " + paritySchema + " CASCADE")
}

// pushParityDuckDB pushes the SQLite fixture to a DuckDB mirror via the
// production Push entry point and returns a read-only DuckDB store over
// the built mirror file.
func pushParityDuckDB(
	t *testing.T, ctx context.Context, local *db.DB,
) *duckdbstore.Store {
	t.Helper()
	target := filepath.Join(t.TempDir(), "parity.duckdb")
	res, err := duckdbstore.Push(
		ctx, target, local, "parity-machine",
		duckdbstore.SyncOptions{}, true, nil,
	)
	require.NoError(t, err, "pushing to duckdb")
	require.Equal(t, len(parityFixture()), res.SessionsPushed,
		"duckdb sessions pushed")

	store, err := duckdbstore.NewStore(target)
	require.NoError(t, err, "opening duckdb store")
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

// canonicalizeReport sorts the report's order-unspecified slices by a stable
// key so the deep comparison is order-independent. ByProject/ByModel/ByAgent
// are already minutes-then-key sorted by the aggregator, but resorting purely
// by Key guards against any backend-introduced ordering difference among equal
// minutes. BySession is minutes-desc from the aggregator; we resort by
// SessionID so equal-minute ties cannot diverge across backends. Intervals
// needs no canonicalization: Aggregate already sorts it by (start, end,
// sessionID), a total order with no cross-backend ties to break.
func canonicalizeReport(r *activity.Report) {
	sort.Slice(r.ByProject, func(i, j int) bool {
		return r.ByProject[i].Key < r.ByProject[j].Key
	})
	sort.Slice(r.ByModel, func(i, j int) bool {
		return r.ByModel[i].Key < r.ByModel[j].Key
	})
	sort.Slice(r.ByAgent, func(i, j int) bool {
		return r.ByAgent[i].Key < r.ByAgent[j].Key
	})
	sort.Slice(r.ByBranch, func(i, j int) bool {
		if r.ByBranch[i].Project != r.ByBranch[j].Project {
			return r.ByBranch[i].Project < r.ByBranch[j].Project
		}
		return r.ByBranch[i].Branch < r.ByBranch[j].Branch
	})
	sort.Slice(r.BySession, func(i, j int) bool {
		return r.BySession[i].SessionID < r.BySession[j].SessionID
	})
}

// TestGetActivityReportParityAcrossBackends builds one fixture, loads it into
// all three backends through their production push paths, and asserts the three
// GetActivityReport results are byte-for-byte equal after canonicalizing the
// order-unspecified slices. It sweeps several ranges and bucket policies --
// minute, hourly, daily-calendar, a custom sub-day window, and a DST-spanning NY
// month -- so the parity guarantee covers every bucketing path, not just one
// day. Each case resolves against a fixed far-future now so the range is always
// complete (non-partial) and every backend's report is deterministic regardless
// of wall clock.
func TestGetActivityReportParityAcrossBackends(t *testing.T) {
	ctx := context.Background()
	local := seedParitySQLite(t)

	// Materialize PG first: it skips the whole test when TEST_PG_URL is unset,
	// so we avoid building the DuckDB mirror needlessly on a skip.
	pgStore := pushParityPostgres(t, ctx, local)
	duckStore := pushParityDuckDB(t, ctx, local)

	fixedNow, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(t, err, "parsing fixed now")

	cases := []struct {
		name  string
		input activity.QueryInput
	}{
		// A single past day -> minute (5m) buckets; carries the full fixture
		// activity and the fixture-sanity assertions below.
		{"day-minute", activity.QueryInput{
			Preset: "day", Date: parityDate, Timezone: "UTC"}},
		// A 3-day range -> hourly buckets.
		{"three-day-hourly", activity.QueryInput{
			Preset: "custom", Timezone: "UTC",
			From: "2026-06-12T00:00:00Z", To: "2026-06-15T00:00:00Z"}},
		// A 30-day range -> daily calendar buckets.
		{"thirty-day-daily", activity.QueryInput{
			Preset: "custom", Timezone: "UTC",
			From: "2026-05-16T00:00:00Z", To: "2026-06-15T00:00:00Z"}},
		// A custom sub-day window that slices into the fixture's morning.
		{"custom-subday", activity.QueryInput{
			Preset: "custom", Timezone: "UTC",
			From: parityDate + "T09:30:00Z", To: parityDate + "T15:00:00Z"}},
		// A NY month spanning the March 8 2026 DST transition. The fixture has
		// no March activity, so this asserts every backend produces identical
		// empty aggregation over identical DST-aware calendar bucket boundaries.
		{"dst-month-ny", activity.QueryInput{
			Preset: "month", Date: "2026-03-14", Timezone: "America/New_York"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := activity.ResolveQuery(tc.input, fixedNow)
			require.NoError(t, err, "resolving query")
			filter := db.AnalyticsFilter{Timezone: tc.input.Timezone}

			sqliteReport := assertParityForCase(t, ctx, q, filter,
				local, pgStore, duckStore)

			if tc.name == "day-minute" {
				assertDayMinuteFixtureSanity(t, sqliteReport)
			}
		})
	}
}

// assertParityForCase queries all three backends with the resolved query and
// filter, asserts the range is complete, deep-compares the canonicalized
// reports (SQLite==PG and SQLite==DuckDB), and returns the canonicalized SQLite
// report so the caller can run case-specific fixture assertions on it.
func assertParityForCase(
	t *testing.T, ctx context.Context, q activity.Query,
	filter db.AnalyticsFilter,
	local *db.DB, pgStore *postgresstore.Store, duckStore *duckdbstore.Store,
) activity.Report {
	t.Helper()

	sqliteReport, err := local.GetActivityReport(ctx, filter, q)
	require.NoError(t, err, "sqlite GetActivityReport")
	pgReport, err := pgStore.GetActivityReport(ctx, filter, q)
	require.NoError(t, err, "pg GetActivityReport")
	duckReport, err := duckStore.GetActivityReport(ctx, filter, q)
	require.NoError(t, err, "duckdb GetActivityReport")

	require.False(t, sqliteReport.Partial, "past range must be complete")

	canonicalizeReport(&sqliteReport)
	canonicalizeReport(&pgReport)
	canonicalizeReport(&duckReport)

	require.Equal(t, sqliteReport, pgReport,
		"SQLite and PostgreSQL activity reports diverge")
	require.Equal(t, sqliteReport, duckReport,
		"SQLite and DuckDB activity reports diverge")
	return sqliteReport
}

// assertDayMinuteFixtureSanity checks the day-minute report actually exercises
// the fixture: a full day with peak concurrency 2, nine sessions, non-zero
// cost, and exactly 14050 output tokens. The token total proves the
// synthetic-model usage row (9999 tokens) is excluded, the dedup pair
// collapses to its earlier 500-token row, the subagent's and unique fork's
// tokens count, and the replaying fork's do not -- not merely that the
// backends agree on a wrong number -- so the deep-compare above extends those
// guarantees, plus the zero-cost primary-model fallback, to PG and DuckDB.
func assertDayMinuteFixtureSanity(t *testing.T, r activity.Report) {
	t.Helper()
	require.False(t, r.Partial, "fixture day must be a full day")
	require.Equal(t, 2, r.Peak.Agents, "fixture must reach peak concurrency 2")
	require.Equal(t, 9, r.Totals.Sessions, "fixture session count")
	require.Greater(t, r.Totals.Cost, 0.0, "fixture must exercise cost")
	// 2400 (parity-a) + 1600 (parity-b) + 300 (parity-c; synthetic 9999 row
	// excluded) + 500 (parity-d wins the dedup) + 0 (parity-e deduped away;
	// parity-f zero-cost) + 250 (parity-sub) + 9000 (parity-fork, unique)
	// + 0 (parity-fork-replay deduped away) = 14050.
	require.Equal(t, 14050, r.Totals.OutputTokens,
		"synthetic row excluded, dedup collapses, subagent and unique fork count")

	bySession := map[string]activity.SessionRow{}
	for _, s := range r.BySession {
		bySession[s.SessionID] = s
	}
	require.Contains(t, bySession, "parity-sub",
		"subagent session must appear in the report")
	require.Contains(t, bySession, "parity-fork",
		"fork session must appear in the report")
	require.Equal(t, 9000, bySession["parity-fork"].OutputTokens,
		"unique fork usage counts toward the report")
	require.Contains(t, bySession, "parity-fork-replay",
		"replaying fork still appears as a session row")
	require.Equal(t, 0, bySession["parity-fork-replay"].OutputTokens,
		"replayed fork usage dedups away")
	require.Contains(t, bySession, "parity-d")
	require.Contains(t, bySession, "parity-e")
	require.Contains(t, bySession, "parity-f")
	require.Equal(t, 500, bySession["parity-d"].OutputTokens,
		"dedup keeps the earlier whole-second duplicate's tokens")
	require.Equal(t, 0, bySession["parity-e"].OutputTokens,
		"the later fractional duplicate is dropped")
	require.Equal(t, "model-x", bySession["parity-f"].PrimaryModel,
		"zero-cost usage still reports its known model as primary")

	// Empty branches survive only when they carry minutes or cost.
	type branchKey struct{ project, branch string }
	byBranch := map[branchKey]activity.BranchKeyMinutes{}
	for _, b := range r.ByBranch {
		byBranch[branchKey{b.Project, b.Branch}] = b
	}
	require.Contains(t, byBranch, branchKey{"alpha", "main"},
		"alpha/main bucket")
	require.Contains(t, byBranch, branchKey{"beta", "main"},
		"beta/main is distinct from alpha/main under the (project, branch) grain")
	require.Contains(t, byBranch, branchKey{"alpha", "feature-x"},
		"a project can carry multiple branches")
	require.Contains(t, byBranch, branchKey{"gamma", ""},
		"empty branch bucket")
	require.NotContains(t, byBranch, branchKey{"delta", ""},
		"zero-cost zero-minute branch bucket is dropped like any zero row")
}
