//go:build chtest

package clickhouse

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
)

func newFinalRowsStore(t *testing.T) *Store {
	t.Helper()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	require.NoError(t, EnsureSchemaOn(t.Context(), conn))
	return newTestStore(t, conn, Target{URL: dsn, Database: database})
}

// finalRowsStep is one write to a tracked table and whether the tracker
// must read the whole table to follow it.
type finalRowsStep struct {
	name string
	sql  []string
	full bool
}

// requireTrackerFollows applies each step and checks that the tracker's
// count, hash sum, and kept fields equal what a FINAL read of the table
// returns, and that only the steps that need it read the whole table.
func requireTrackerFollows(
	t *testing.T, store *Store, f *finalRows, want func(context.Context) (string, map[string]finalFields), steps []finalRowsStep,
) {
	t.Helper()
	ctx := t.Context()
	sync := func() *finalRowsChange {
		t.Helper()
		snapshot, err := store.readPartsSnapshot(ctx)
		require.NoError(t, err)
		change, err := store.prepareFinalRows(ctx, f, snapshot[f.spec.table])
		require.NoError(t, err)
		if change != nil {
			f.apply(change)
		}
		return change
	}
	sync()
	for _, step := range steps {
		execPreparedUsage(t, store, step.sql...)
		change := sync()
		require.NotNil(t, change, step.name)
		require.Equal(t, step.full, change.full, step.name)
		identity, rows := want(ctx)
		require.Equal(t, identity, f.identity(), step.name)
		if rows != nil {
			require.Equal(t, rows, f.rows, step.name)
		}
	}
}

// The sessions tracker must end every write sequence with the count, row
// hash sum, and fields that FINAL returns, reading only the written keys
// for inserts and for merges that fold new rows into small parts.
func TestFinalRowsFollowSessions(t *testing.T) {
	t.Parallel()
	store := newFinalRowsStore(t)
	want := func(ctx context.Context) (string, map[string]finalFields) {
		var count uint64
		var sum string
		require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT count(),
			reinterpretAsFixedString(sumWithOverflow(reinterpretAsUInt128(sipHash128Reference(tuple(*)))))
			FROM sessions FINAL`).Scan(&count, &sum))
		hash, err := parseRowHash(sum)
		require.NoError(t, err)
		rows, err := store.DB().QueryContext(ctx, `SELECT id, push_version, machine, sipHash128Reference(tuple(*))
			FROM sessions FINAL`)
		require.NoError(t, err)
		defer rows.Close()
		fields := map[string]finalFields{}
		for rows.Next() {
			var id, rowHash string
			var r finalFields
			require.NoError(t, rows.Scan(&id, &r.pushVersion, &r.text, &rowHash))
			r.hash, err = parseRowHash(rowHash)
			require.NoError(t, err)
			fields[id] = r
		}
		require.NoError(t, rows.Err())
		return (&finalRows{count: count, sum: hash}).identity(), fields
	}
	insert := func(ids string, version int, name string) string {
		return fmt.Sprintf(`INSERT INTO sessions (id, machine, display_name, push_version)
			SELECT arrayJoin([%s]), 'host-a', '%s', %d`, ids, name, version)
	}
	f := &finalRows{spec: sessionsFinalTable}
	requireTrackerFollows(t, store, f, want, []finalRowsStep{
		{name: "first rows", sql: []string{"SYSTEM STOP MERGES sessions", insert("'a','b','c'", 1, "one")}},
		{name: "new session", sql: []string{insert("'d'", 1, "one")}},
		{name: "rename without a message change", sql: []string{insert("'b'", 2, "renamed")}},
		{name: "older version stays hidden", sql: []string{insert("'c'", 0, "stale")}},
		{name: "equal version replaces", sql: []string{insert("'d'", 1, "tie")}},
		{
			name: "merge folding a new row into kept parts",
			sql:  []string{insert("'a'", 3, "folded"), "SYSTEM START MERGES sessions", "OPTIMIZE TABLE sessions FINAL", "SYSTEM STOP MERGES sessions"},
		},
		{
			name: "lightweight delete",
			sql:  []string{"SYSTEM START MERGES sessions", "DELETE FROM sessions WHERE id = 'c'", "SYSTEM STOP MERGES sessions"},
			full: true,
		},
		{name: "push after the delete", sql: []string{insert("'e'", 4, "one")}},
		{name: "truncate", sql: []string{"TRUNCATE TABLE sessions"}, full: true},
	})
}

// The snapshot tracker orders rows by revision, which a push sets above
// the previous snapshot's.
func TestFinalRowsFollowSnapshots(t *testing.T) {
	t.Parallel()
	store := newFinalRowsStore(t)
	want := func(ctx context.Context) (string, map[string]finalFields) {
		var count uint64
		var sum string
		require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT count(),
			reinterpretAsFixedString(sumWithOverflow(reinterpretAsUInt128(sipHash128Reference(tuple(id, revision, push_version)))))
			FROM usage_session_snapshots FINAL`).Scan(&count, &sum))
		hash, err := parseRowHash(sum)
		require.NoError(t, err)
		rows, err := store.DB().QueryContext(ctx, `SELECT id, push_version, toString(revision), prepares_usage,
			sipHash128Reference(tuple(id, revision, push_version)) FROM usage_session_snapshots FINAL`)
		require.NoError(t, err)
		defer rows.Close()
		fields := map[string]finalFields{}
		for rows.Next() {
			var id, rowHash string
			var r finalFields
			require.NoError(t, rows.Scan(&id, &r.pushVersion, &r.text, &r.flag, &rowHash))
			r.hash, err = parseRowHash(rowHash)
			require.NoError(t, err)
			fields[id] = r
		}
		require.NoError(t, rows.Err())
		return (&finalRows{count: count, sum: hash}).identity(), fields
	}
	insert := func(id string, version int) string {
		return fmt.Sprintf(`INSERT INTO usage_session_snapshots (id, push_version, usage_messages)
			VALUES ('%s', %d, [(0, 'assistant', NULL, 'claude-test', '', '{"input_tokens":%d}', '', '', '')])`,
			id, version, version)
	}
	f := &finalRows{spec: snapshotsFinalTable}
	requireTrackerFollows(t, store, f, want, []finalRowsStep{
		{name: "first snapshots", sql: []string{"SYSTEM STOP MERGES usage_session_snapshots", insert("a", 1), insert("b", 1)}},
		{name: "new snapshot", sql: []string{insert("a", 2)}},
		{name: "merge alone", sql: []string{"SYSTEM START MERGES usage_session_snapshots", "OPTIMIZE TABLE usage_session_snapshots FINAL"}},
		{name: "snapshot after the merge", sql: []string{"SYSTEM STOP MERGES usage_session_snapshots", insert("b", 3)}},
	})
}

// The price tracker counts only its digest's records, so records under
// other digests change nothing it reports.
func TestFinalRowsFollowOneDigestsPrices(t *testing.T) {
	t.Parallel()
	store := newFinalRowsStore(t)
	want := func(ctx context.Context) (string, map[string]finalFields) {
		var count uint64
		var sum string
		require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT count(),
			reinterpretAsFixedString(sumWithOverflow(reinterpretAsUInt128(sipHash128Reference(tuple(*)))))
			FROM usage_event_prices FINAL WHERE pricing_digest = 'current'`).Scan(&count, &sum))
		hash, err := parseRowHash(sum)
		require.NoError(t, err)
		return (&finalRows{count: count, sum: hash}).identity(), nil
	}
	insert := func(digest, keys string, cost, version int) string {
		return fmt.Sprintf(`INSERT INTO usage_event_prices (pricing_digest, price_key, token_cost_microdollars, priced, push_version)
			SELECT '%s', arrayJoin([%s]), %d, 1, %d`, digest, keys, cost, version)
	}
	f := &finalRows{spec: pricesFinalTable, digest: "current"}
	requireTrackerFollows(t, store, f, want, []finalRowsStep{
		{name: "first records", sql: []string{"SYSTEM STOP MERGES usage_event_prices", insert("current", "'k1','k2'", 10, 1)}},
		{name: "new record", sql: []string{insert("current", "'k3'", 10, 1)}},
		{name: "repriced record", sql: []string{insert("current", "'k1'", 11, 2)}},
	})
	before := f.identity()
	snapshot, err := store.readPartsSnapshot(t.Context())
	require.NoError(t, err)
	execPreparedUsage(t, store, insert("retired", "'k1','k9'", 99, 5))
	after, err := store.readPartsSnapshot(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, snapshot[chUsagePriceTable].digest, after[chUsagePriceTable].digest)
	change, err := store.prepareFinalRows(t.Context(), f, after[chUsagePriceTable])
	require.NoError(t, err)
	require.Nil(t, change, "records of another digest must not change the tracked ones")
	require.Equal(t, before, f.identity())
}

// Each kind of write that changes what usage coverage reads must change
// the version that keys it, and a merge alone must leave the version and
// the Activity probe as they were.
func TestUsageCoverageVersionInvalidation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, _, store := newPreparedUsageStore(t)
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.True(t, state.ready)
	var digest string
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT pricing_digest FROM prepared_usage LIMIT 1 SETTINGS final=0`).Scan(&digest))
	require.NotEmpty(t, digest)
	version := func() string {
		t.Helper()
		v, ok, err := store.usageRowsVersion(ctx, chUsageCoverageTables, digest)
		require.NoError(t, err)
		require.True(t, ok)
		return v
	}
	probe := func() any {
		t.Helper()
		p, err := store.ActivityReportSourceProbe(ctx)
		require.NoError(t, err)
		return p
	}
	merge := func() {
		t.Helper()
		for _, table := range chUsageCoverageTables {
			execPreparedUsage(t, store, "OPTIMIZE TABLE "+table+" FINAL")
		}
	}
	previous, previousProbe := version(), probe()
	merge()
	require.Equal(t, previous, version(), "a merge alone must keep the version")
	require.Equal(t, previousProbe, probe(), "a merge alone must keep the probe")

	for _, change := range []struct {
		name string
		sql  string
	}{
		{"session edit without a message change", `INSERT INTO sessions
			SELECT * REPLACE (push_version + 1 AS push_version, 'renamed' AS display_name)
			FROM sessions ORDER BY id LIMIT 1`},
		{"new snapshot", `INSERT INTO usage_session_snapshots
			SELECT * REPLACE (push_version + 1 AS push_version, revision + 2 AS revision)
			FROM usage_session_snapshots ORDER BY id LIMIT 1`},
		{"price record change", fmt.Sprintf(`INSERT INTO usage_event_prices
			SELECT * REPLACE (push_version + 1 AS push_version, token_cost_microdollars + 1 AS token_cost_microdollars)
			FROM usage_event_prices WHERE pricing_digest = '%s' ORDER BY price_key LIMIT 1`, digest)},
	} {
		execPreparedUsage(t, store, change.sql)
		current := version()
		require.NotEqual(t, previous, current, change.name)
		previous = current
		merge()
		require.Equal(t, previous, version(), "a merge after the %s must keep the version", change.name)
	}

	previousProbe = probe()
	runPreparedUsageRefresh(t, store)
	require.NotEqual(t, previousProbe, probe(), "a prepared usage refresh must change the probe")
}

// The highest message id must match max(id) under FINAL when the rows with
// the highest stored ids were replaced by rows with lower ids.
func TestActivitySourceProbeMaxIDWithReplacedTopRows(t *testing.T) {
	t.Parallel()
	store := newFinalRowsStore(t)
	execPreparedUsage(t, store,
		"SYSTEM STOP MERGES messages",
		`INSERT INTO messages (id, session_id, ordinal, push_version) SELECT 1001 + number, 'a', number, 1 FROM numbers(40)`,
		`INSERT INTO messages (id, session_id, ordinal, push_version) SELECT 1 + number, 'b', number, 1 FROM numbers(3)`,
		// The 20 highest ids of a move to ids below 100.
		`INSERT INTO messages (id, session_id, ordinal, push_version) SELECT number - 19, 'a', number, 2 FROM numbers(20, 20)`,
	)
	got, err := store.ActivityReportSourceProbe(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1020, got.MaxMessageID)

	execPreparedUsage(t, store, `INSERT INTO messages (id, session_id, ordinal, push_version) VALUES (5000, 'b', 0, 2)`)
	got, err = store.ActivityReportSourceProbe(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 5000, got.MaxMessageID)
}

// A merge between the parts snapshot and the read of each part's top ids
// leaves the snapshot naming parts that no longer exist. The highest id
// must still come back, not the zero the missing parts would give.
func TestActivitySourceProbeMaxIDAfterMergeDuringRead(t *testing.T) {
	t.Parallel()
	store := newFinalRowsStore(t)
	ctx := t.Context()
	execPreparedUsage(t, store,
		"SYSTEM STOP MERGES messages",
		`INSERT INTO messages (id, session_id, ordinal, push_version) SELECT 1 + number, 'a', number, 1 FROM numbers(3)`,
		`INSERT INTO messages (id, session_id, ordinal, push_version) VALUES (700, 'b', 0, 1)`,
	)
	snapshot, err := store.readPartsSnapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snapshot["messages"].parts, 2)
	execPreparedUsage(t, store, "SYSTEM START MERGES messages", "OPTIMIZE TABLE messages FINAL")

	got, err := store.maxFinalID(ctx, "messages", snapshot["messages"], &finalIDs{})
	require.NoError(t, err)
	require.EqualValues(t, 700, got)
}

// A coverage check whose parts move while it reads keeps what its trackers
// read, so each retry reads only the new parts. Before, every retry reread
// both tables in full.
func TestUsageCoverageRetryReadsOnlyMovedParts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, syncer, store := newPreparedUsageStore(t)
	fullReads := func() (sessions, snapshots int) {
		t.Helper()
		// The log shows a key lookup's keys inline, in has(...).
		execPreparedUsage(t, store, "SYSTEM FLUSH LOGS")
		require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT
			countIf(query LIKE '%sipHash128Reference(tuple(*))%FROM sessions%'),
			countIf(query LIKE '%sipHash128Reference(tuple(id, revision, push_version))%FROM usage_session_snapshots%')
			FROM system.query_log
			WHERE current_database = currentDatabase() AND type = 'QueryFinish'
				AND query NOT LIKE '%has(%' AND query NOT LIKE '%system.query_log%'`).Scan(&sessions, &snapshots))
		return sessions, snapshots
	}
	// A store that has not read the tables yet, as after a restart.
	cold := newTestStore(t, syncer.conn, syncer.target)
	moves := 0
	cold.afterTrackerReads = func() {
		if moves < 2 {
			moves++
			execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO sessions
				SELECT * REPLACE (concat(id, '-moved-%d') AS id) FROM sessions ORDER BY id LIMIT 1`, moves))
		}
	}
	sessionsBefore, snapshotsBefore := fullReads()
	got, err := cold.readPreparedUsageState(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 2, moves)
	sessionsAfter, snapshotsAfter := fullReads()
	assert.Equal(t, 1, sessionsAfter-sessionsBefore, "full sessions reads")
	assert.Equal(t, 1, snapshotsAfter-snapshotsBefore, "full snapshot reads")

	want, err := store.readPreparedUsageState(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// A full read of kept rows that names parts a merge removes still
// describes the parts it read, and the next check reads only the merged
// part when the read missed the rows. An incremental read whose parts
// vanished cannot tell what it missed, so it is not kept.
func TestFinalRowsSettleAfterMerge(t *testing.T) {
	t.Parallel()
	insert := func(ids string) string {
		return fmt.Sprintf(`INSERT INTO sessions (id, machine, display_name, push_version)
			SELECT arrayJoin([%s]), 'host-a', 'one', 1`, ids)
	}
	merge := []string{"SYSTEM START MERGES sessions", "OPTIMIZE TABLE sessions FINAL", "SYSTEM STOP MERGES sessions"}
	for _, tc := range []struct {
		name string
		// tracked runs before the tracker's first read.
		tracked bool
		// mergeBeforeRead merges the parts between the snapshot and the read.
		mergeBeforeRead bool
		settles         bool
	}{
		{name: "full read after the merge", mergeBeforeRead: true, settles: true},
		{name: "full read before the merge", settles: true},
		{name: "incremental read before the merge", tracked: true, settles: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store := newFinalRowsStore(t)
			f := &finalRows{spec: sessionsFinalTable}
			execPreparedUsage(t, store, "SYSTEM STOP MERGES sessions", insert("'a','b'"))
			if tc.tracked {
				snapshot, err := store.readPartsSnapshot(ctx)
				require.NoError(t, err)
				change, err := store.prepareFinalRows(ctx, f, snapshot["sessions"])
				require.NoError(t, err)
				f.apply(change)
			}
			execPreparedUsage(t, store, insert("'b','c'"))
			snapshot, err := store.readPartsSnapshot(ctx)
			require.NoError(t, err)
			if tc.mergeBeforeRead {
				execPreparedUsage(t, store, merge...)
			}
			change, err := store.prepareFinalRows(ctx, f, snapshot["sessions"])
			require.NoError(t, err)
			require.Equal(t, !tc.tracked, change.full)
			if !tc.mergeBeforeRead {
				execPreparedUsage(t, store, merge...)
			}
			fresh, err := store.readPartsSnapshot(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.settles, change.settle(fresh["sessions"]))
			if !tc.settles {
				return
			}
			f.apply(change)

			next, err := store.prepareFinalRows(ctx, f, fresh["sessions"])
			require.NoError(t, err)
			require.NotNil(t, next)
			assert.False(t, next.full, "the check after a kept read must not reread the table")
			f.apply(next)
			var count uint64
			var sum string
			require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT count(),
				reinterpretAsFixedString(sumWithOverflow(reinterpretAsUInt128(sipHash128Reference(tuple(*)))))
				FROM sessions FINAL`).Scan(&count, &sum))
			require.EqualValues(t, 3, count)
			hash, err := parseRowHash(sum)
			require.NoError(t, err)
			assert.Equal(t, (&finalRows{count: count, sum: hash}).identity(), f.identity())
		})
	}
}
