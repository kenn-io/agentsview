//go:build duckdb && !(windows && arm64)

package duckdb

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func rollupMessage(sessionID string, ordinal int, ts, model, claudeID, sourceUUID, usage string) db.Message {
	msg := syncMessage(sessionID, ordinal, "assistant", "work", ts)
	msg.Model = model
	msg.TokenUsage = []byte(usage)
	if claudeID != "" {
		msg.ClaudeMessageID = claudeID
		msg.ClaudeRequestID = "req-" + claudeID
	}
	msg.SourceUUID = sourceUUID
	return msg
}

func rollupSession(id, agent, project, machine, ts string) db.Session {
	sess := syncSession(id, project, "first "+id, ts, 2)
	sess.Agent = agent
	sess.Machine = machine
	return sess
}

func rollupWrite(sess db.Session, msgs []db.Message, events ...db.UsageEvent) db.SessionBatchWrite {
	return db.SessionBatchWrite{
		Session: sess, Messages: msgs, UsageEvents: events,
		DataVersion: 1, ReplaceMessages: true,
	}
}

func out(n int) string { return fmt.Sprintf(`{"input_tokens":100,"output_tokens":%d}`, n) }

// rollupFixture seeds sessions whose dedup groups cover every rollup rule:
// groups finalized in one session and bucket, groups split across buckets,
// sessions, and models, a group sharing a Cursor account identity, Copilot
// session costs, rows without a timestamp, and 2010 rows in a zone that then
// changed offset one minute after a bucket boundary.
func rollupFixture(t *testing.T) *db.DB {
	t.Helper()
	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern: "test-opus", InputPerMTok: money.MustParseDollars("2"), OutputPerMTok: money.MustParseDollars("10"),
			CacheCreationPerMTok: money.MustParseDollars("2.5"), CacheReadPerMTok: money.MustParseDollars("0.2"),
		},
		{ModelPattern: "test-haiku", InputPerMTok: money.MustParseDollars("1"), OutputPerMTok: money.MustParseDollars("5")},
	}))
	a := "claude:a"
	b := "claude:b"
	c := "codex:c"
	d := "copilot:d"
	cost := func(s string) *money.Money { v := money.MustParseDollars(s); return &v }
	noTime := rollupSession("claude:e", "claude", "alpha", "m1", "")
	noTime.StartedAt, noTime.EndedAt = nil, nil
	noTime.CreatedAt = "2026-03-08T00:00:00Z"
	noTimeMsg := rollupMessage("claude:e", 0, "", "test-opus", "e1", "", out(9))
	noTimeMsg.Timestamp = ""
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{
		rollupWrite(rollupSession(a, "claude", "alpha", "m1", "2026-03-07T23:40:00Z"), []db.Message{
			// Two snapshots of one request in one bucket: finalized.
			rollupMessage(a, 0, "2026-03-07T23:50:05Z", "test-opus", "c1", "", out(10)),
			rollupMessage(a, 1, "2026-03-07T23:50:20Z", "test-opus", "c1", "", out(40)),
			// One request across a bucket and UTC day boundary: exception.
			rollupMessage(a, 2, "2026-03-07T23:59:58Z", "test-opus", "c2", "", out(5)),
			rollupMessage(a, 3, "2026-03-08T00:00:03Z", "test-opus", "c2", "", out(7)),
			// One request answered by two models in one bucket: finalized,
			// filtered by its winner's model.
			rollupMessage(a, 4, "2026-03-08T04:31:00Z", "test-opus", "c3", "", out(3)),
			rollupMessage(a, 5, "2026-03-08T04:32:00Z", "test-haiku", "c3", "", out(30)),
			// Shared with claude:b: exception.
			rollupMessage(a, 6, "2026-03-08T05:10:00Z", "test-opus", "c4", "", out(11)),
			// No dedup identity, near Kolkata midnight.
			rollupMessage(a, 7, "2026-03-08T18:29:00Z", "test-haiku", "", "", out(13)),
			rollupMessage(a, 8, "2026-03-08T12:00:00Z", "test-opus", "c5", "",
				`{"input_tokens":1000,"output_tokens":200,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"server_tool_use":{"web_search_requests":2}}`),
		}),
		rollupWrite(rollupSession(b, "claude", "beta", "m2", "2026-03-08T05:00:00Z"), []db.Message{
			rollupMessage(b, 0, "2026-03-08T05:11:00Z", "test-opus", "c4", "", out(50)),
			rollupMessage(b, 1, "2026-03-08T06:00:00Z", "test-haiku", "c6", "", out(17)),
		}),
		rollupWrite(rollupSession(c, "codex", "alpha", "m2", "2026-03-08T07:00:00Z"), []db.Message{
			// One source row in two models: exception.
			rollupMessage(c, 0, "2026-03-08T07:01:00Z", "test-opus", "", "u1", out(21)),
			rollupMessage(c, 1, "2026-03-08T07:02:00Z", "test-haiku", "", "u1", out(22)),
			// One source row repeated in one model and bucket: finalized.
			rollupMessage(c, 2, "2026-03-08T07:03:00Z", "test-opus", "", "u2", out(23)),
			rollupMessage(c, 3, "2026-03-08T07:04:00Z", "test-opus", "", "u2", out(24)),
		},
			db.UsageEvent{Source: "turn", Model: "test-opus", InputTokens: 5, OutputTokens: 6, OccurredAt: "2026-03-08T07:20:00Z", DedupKey: "k1"},
			// Shares its identity with a Cursor account row: exception.
			db.UsageEvent{Source: "turn", Model: "test-haiku", InputTokens: 7, OutputTokens: 8, OccurredAt: "2026-03-08T07:30:00Z", DedupKey: "k2"},
		),
		rollupWrite(rollupSession(d, "copilot", "beta", "m1", "2026-03-08T08:00:00Z"), nil,
			db.UsageEvent{Source: "shutdown", Model: "test-opus", InputTokens: 10, Cost: cost("0.30"), CostStatus: "exact", CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-03-08T09:00:00Z", DedupKey: "first"},
			db.UsageEvent{Source: "shutdown", Model: "test-haiku", InputTokens: 20, Cost: cost("0.50"), CostStatus: "exact", CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-03-08T10:00:00Z", DedupKey: "latest"},
			db.UsageEvent{Source: "turn", Model: "test-opus", InputTokens: 30, OutputTokens: 4, OccurredAt: "2026-03-08T09:30:00Z", DedupKey: "t1"},
		),
		rollupWrite(noTime, []db.Message{noTimeMsg}),
		rollupWrite(rollupSession("claude:f", "claude", "gamma", "m1", "2010-03-14T03:20:00Z"), []db.Message{
			rollupMessage("claude:f", 0, "2010-03-14T03:35:00Z", "test-opus", "f1", "", out(14)),
		}),
		rollupWrite(rollupSession("claude:h", "claude", "gamma", "m2", "2026-03-08T11:00:00Z"), []db.Message{
			rollupMessage("claude:h", 0, "2026-03-08T11:05:00Z", "test-opus", "h1", "", out(15)),
		}),
	})
	require.NoError(t, err)
	require.NoError(t, local.InsertCursorUsageEvents(ctx, []db.CursorUsageEvent{
		{Agent: "cursor", OccurredAt: "2026-03-08T07:31:00Z", Model: "test-haiku", InputTokens: 9, OutputTokens: 9, DedupKey: c + ":turn:k2"},
		{Agent: "cursor", OccurredAt: "2026-03-08T13:00:00Z", Model: "test-opus", InputTokens: 9, OutputTokens: 9, Charged: money.MustParseDollars("0.01"), DedupKey: "solo"},
	}))
	return local
}

type rollupReads struct {
	daily  db.DailyUsageResult
	top    []db.TopSessionEntry
	counts db.UsageSessionCounts
}

func readRollupCase(t *testing.T, store *Store, f db.UsageFilter) rollupReads {
	t.Helper()
	daily, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	top, err := store.GetTopSessionsByCost(t.Context(), f, 100)
	require.NoError(t, err)
	counts, err := store.GetUsageSessionCounts(t.Context(), f)
	require.NoError(t, err)
	return rollupReads{daily, top, counts}
}

// readLive reads with the rollup's metadata hidden, which is how a mirror
// without a rollup reads.
func readLive(t *testing.T, syncer *Sync, store *Store, f db.UsageFilter) rollupReads {
	t.Helper()
	var meta string
	require.NoError(t, syncer.DB().QueryRowContext(t.Context(),
		`SELECT value FROM sync_metadata WHERE key = ?`, usageRollupMetadataKey).Scan(&meta))
	_, err := syncer.DB().ExecContext(t.Context(), `DELETE FROM sync_metadata WHERE key = ?`, usageRollupMetadataKey)
	require.NoError(t, err)
	defer func() {
		_, err := syncer.DB().ExecContext(t.Context(),
			`INSERT INTO sync_metadata (key, value) VALUES (?, ?)`, usageRollupMetadataKey, meta)
		require.NoError(t, err)
	}()
	return readRollupCase(t, store, f)
}

func rollupFilters() []db.UsageFilter {
	var out []db.UsageFilter
	windows := [][2]string{{"2026-03-07", "2026-03-08"}, {"2026-03-08", "2026-03-08"}, {"", ""}, {"2026-03-08", ""}, {"", "2026-03-07"}}
	for _, zone := range []string{"UTC", "America/New_York", "Asia/Kolkata", "Asia/Kathmandu"} {
		for _, w := range windows {
			out = append(out, db.UsageFilter{From: w[0], To: w[1], Timezone: zone, Breakdowns: true})
		}
	}
	for _, f := range []db.UsageFilter{
		{Model: "test-opus"},
		{ExcludeModel: "test-haiku"},
		{Project: "alpha"},
		{Agent: "claude"},
		{Machine: "m2"},
		{ExcludeAgent: "copilot"},
		{Model: "test-haiku", Breakdowns: true},
	} {
		for _, w := range [][2]string{{"2026-03-07", "2026-03-08"}, {"", ""}} {
			f.From, f.To, f.Timezone = w[0], w[1], "America/New_York"
			out = append(out, f)
		}
	}
	return out
}

func assertRollupMatchesLive(t *testing.T, syncer *Sync, store *Store) {
	t.Helper()
	for _, f := range rollupFilters() {
		name := fmt.Sprintf("%+v", f)
		plan, err := store.usageRollupPlan(t.Context(), f, "", true)
		require.NoError(t, err)
		require.NotNil(t, plan, "%s should read the rollup", name)
		live := readLive(t, syncer, store, f)
		got := readRollupCase(t, store, f)
		require.Equal(t, live.daily, got.daily, name)
		require.ElementsMatch(t, live.top, got.top, name)
		require.Equal(t, live.counts, got.counts, name)
	}
}

func rollupSessionsIn(t *testing.T, syncer *Sync, table string) []string {
	t.Helper()
	rows, err := syncer.DB().QueryContext(t.Context(), `SELECT DISTINCT session_id FROM `+table+` ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func TestUsageRollupMatchesLiveReads(t *testing.T) {
	ctx := t.Context()
	local := rollupFixture(t)
	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	_, err = syncer.buildUsageRollup(ctx)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	require.Equal(t, []string{"claude:a", "claude:b", "codex:c", "copilot:d"},
		rollupSessionsIn(t, syncer, "usage_rollup_exceptions"))
	require.Equal(t, []string{"claude:a", "claude:b", "claude:e", "claude:f", "claude:h", "codex:c", "copilot:d"},
		rollupSessionsIn(t, syncer, "usage_rollup"))
	assertRollupMatchesLive(t, syncer, store)

	// claude:b now repeats a request claude:a finished in one bucket, a new
	// session repeats another, a Cursor row now shares codex:c's finalized
	// usage key, one session is trashed, and one is deleted.
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{
		rollupWrite(rollupSession("claude:b", "claude", "beta", "m2", "2026-03-08T05:00:00Z"), []db.Message{
			rollupMessage("claude:b", 0, "2026-03-08T05:11:00Z", "test-opus", "c4", "", out(50)),
			rollupMessage("claude:b", 1, "2026-03-08T06:00:00Z", "test-haiku", "c6", "", out(17)),
			rollupMessage("claude:b", 2, "2026-03-07T23:52:00Z", "test-opus", "c1", "", out(90)),
		}),
		rollupWrite(rollupSession("claude:g", "claude", "beta", "m1", "2026-03-08T04:00:00Z"), []db.Message{
			rollupMessage("claude:g", 0, "2026-03-08T04:20:00Z", "test-opus", "c3", "", out(60)),
		}),
	})
	require.NoError(t, err)
	require.NoError(t, local.InsertCursorUsageEvents(ctx, []db.CursorUsageEvent{
		{Agent: "cursor", OccurredAt: "2026-03-08T07:22:00Z", Model: "test-opus", InputTokens: 1, OutputTokens: 1, DedupKey: "codex:c:turn:k1"},
	}))
	require.NoError(t, local.SoftDeleteSession(ctx, "claude:e"))
	require.NoError(t, local.DeleteSession(ctx, "claude:h"))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)

	plan, err := store.usageRollupPlan(ctx, db.UsageFilter{Timezone: "UTC"}, "", true)
	require.NoError(t, err)
	require.True(t, plan.changed)
	require.Len(t, plan.units, 3, "the c1, c3, and k1 units are read live")
	assertRollupMatchesLive(t, syncer, store)
}

func TestUsageRollupFallsBackForUnalignedZones(t *testing.T) {
	ctx := t.Context()
	local := rollupFixture(t)
	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	_, err = syncer.buildUsageRollup(ctx)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	// St. John's moved its clocks at 03:31 UTC on 2010-03-14, inside a bucket.
	for _, f := range []db.UsageFilter{
		{Timezone: "America/St_Johns"},
		{From: "2010-03-13", To: "2010-03-14", Timezone: "America/St_Johns"},
	} {
		plan, err := store.usageRollupPlan(ctx, f, "", true)
		require.NoError(t, err)
		require.Nil(t, plan, "%+v", f)
		require.Equal(t, readLive(t, syncer, store, f), readRollupCase(t, store, f))
	}
	// Since 2011 it changes at 02:00 local, on bucket boundaries.
	plan, err := store.usageRollupPlan(ctx, db.UsageFilter{From: "2026-03-08", To: "2026-03-08", Timezone: "America/St_Johns"}, "", true)
	require.NoError(t, err)
	require.NotNil(t, plan)
}

func TestUsageRollupRebuildsWhenChangedRowsReachShare(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	var writes []db.SessionBatchWrite
	for i := range 60 {
		id := fmt.Sprintf("s-%02d", i)
		writes = append(writes, rollupWrite(rollupSession(id, "claude", "alpha", "m1", "2026-03-08T00:00:00Z"),
			[]db.Message{rollupMessage(id, 0, "2026-03-08T00:01:00Z", "claude-test", "", "", out(1))}))
	}
	_, err := local.WriteSessionBatchAtomic(ctx, writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	rebuilt, err := syncer.refreshUsageRollup(ctx)
	require.NoError(t, err)
	require.True(t, rebuilt, "a mirror without a rollup builds one")

	rewrite := func(id string) {
		_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{rollupWrite(
			rollupSession(id, "claude", "alpha", "m1", "2026-03-08T00:00:00Z"),
			[]db.Message{rollupMessage(id, 0, "2026-03-08T00:02:00Z", "claude-test", "", "", out(2))})})
		require.NoError(t, err)
		_, err = syncer.pushEverything(ctx, nil)
		require.NoError(t, err)
	}
	// One changed row of 60 is under 1/50.
	rewrite("s-00")
	rebuilt, err = syncer.refreshUsageRollup(ctx)
	require.NoError(t, err)
	require.False(t, rebuilt)
	// Two changed rows of 60 reach it.
	rewrite("s-01")
	rebuilt, err = syncer.refreshUsageRollup(ctx)
	require.NoError(t, err)
	require.True(t, rebuilt)
	rebuilt, err = syncer.refreshUsageRollup(ctx)
	require.NoError(t, err)
	require.False(t, rebuilt, "a fresh build has no changed rows")
}

func TestUsageRollupPartsSurviveRestart(t *testing.T) {
	ctx := t.Context()
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local := rollupFixture(t)
	path := filepath.Join(t.TempDir(), "rollup.duckdb")
	livePath := filepath.Join(t.TempDir(), "live.duckdb")
	for _, p := range []string{path, livePath} {
		_, err := rebuildMirror(ctx, p, local, "test-machine", storage.MirrorPushOptions{}, "test", nil)
		require.NoError(t, err)
	}
	writeUsageMirror(t, livePath, `DELETE FROM sync_metadata WHERE key = '`+usageRollupMetadataKey+`'`)
	f := db.UsageFilter{From: "2026-03-01", To: "2026-03-31", Timezone: "America/New_York", Breakdowns: true}

	liveStore, err := NewStore(ctx, livePath)
	require.NoError(t, err)
	wantDaily, err := liveStore.GetDailyUsage(ctx, f)
	require.NoError(t, err)
	wantTop, err := liveStore.GetTopSessionsByCost(ctx, f, 10)
	require.NoError(t, err)
	require.NoError(t, liveStore.Close())

	store := openUsagePartsStore(t, path)
	plan, err := store.usageRollupPlan(ctx, f, "", true)
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.False(t, plan.changed)
	_, err = store.GetDailyUsage(ctx, f)
	require.NoError(t, err)
	_, err = store.GetTopSessionsByCost(ctx, f, 10)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	// The restarted store answers from the parts the rollup read saved,
	// pricing provenance included.
	restarted := openUsagePartsStore(t, path)
	defer restarted.Close()
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	gotDaily, err := restarted.GetDailyUsage(ctx, f)
	require.NoError(t, err)
	gotTop, err := restarted.GetTopSessionsByCost(ctx, f, 10)
	require.NoError(t, err)
	require.Zero(t, reads)
	require.NotNil(t, wantDaily.Pricing)
	require.Equal(t, wantDaily, gotDaily)
	require.Equal(t, wantTop, gotTop)
}

// A mirror replacement in the middle of a rollup read sends the read to the
// new mirror's rows, even when the new mirror's pricing counts name records
// the old mirror did not have.
func TestUsageRollupReadSurvivesReplacementWithNewRecords(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	dir := t.TempDir()
	path, next := filepath.Join(dir, "rollup.duckdb"), filepath.Join(dir, "next.duckdb")
	local := rollupFixture(t)
	_, err := rebuildMirror(ctx, path, local, "test-machine", storage.MirrorPushOptions{}, "test", nil)
	require.NoError(t, err)
	// A model without a price adds a pricing record without changing the
	// prices the read loaded before the replacement.
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{
		rollupWrite(rollupSession("claude:n", "claude", "alpha", "m1", "2026-03-08T14:00:00Z"), []db.Message{
			rollupMessage("claude:n", 0, "2026-03-08T14:05:00Z", "test-unpriced", "n1", "", out(60)),
		}),
	})
	require.NoError(t, err)
	_, err = rebuildMirror(ctx, next, local, "test-machine", storage.MirrorPushOptions{}, "test", nil)
	require.NoError(t, err)

	store := openUsagePartsStore(t, path)
	defer store.Close()
	previous := usageRollupRecordsReadObserver
	t.Cleanup(func() { usageRollupRecordsReadObserver = previous })
	swapped := false
	usageRollupRecordsReadObserver = func() {
		usageRollupRecordsReadObserver = nil
		require.NoError(t, swapMirrorFile(next, path))
		store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
		swapped = true
	}
	f := db.UsageFilter{From: "2026-03-08", To: "2026-03-08", Timezone: "UTC"}
	got, err := store.GetDailyUsage(ctx, f)
	require.NoError(t, err)
	require.True(t, swapped, "the read never reached the rollup's pricing records")
	require.Len(t, got.Daily, 1)
	require.Contains(t, got.Daily[0].ModelsUsed, "test-unpriced")
	want, err := NewStoreFromDB(store.DB()).GetDailyUsage(ctx, f)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// Past a zone's last listed transition Go derives its offsets from the
// zone's rule string, and on December 31 of a leap year it reports the
// current period as ending at or before the instant asked about. Archived
// rows reach that rule from 2038 on, and Windows test runs, which use Go's
// embedded slim zone data, reach it years earlier. The zone check must still
// finish, and every New York offset fits a bucket.
func TestUsageRollupZoneAlignedPastLeapYearEnd(t *testing.T) {
	from := time.Date(2040, 11, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2041, 2, 1, 0, 0, 0, 0, time.UTC)
	meta := usageRollupMeta{MinBucket: &from, MaxBucket: &to}
	done := make(chan bool, 1)
	go func() { done <- usageRollupZoneAligned(db.UsageFilter{Timezone: "America/New_York"}, meta) }()
	select {
	case aligned := <-done:
		require.True(t, aligned)
	case <-time.After(time.Minute):
		require.FailNow(t, "the zone check did not finish")
	}
}
