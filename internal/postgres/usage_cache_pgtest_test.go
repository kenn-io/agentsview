//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/usagefacts"
)

type usageCacheProbe struct {
	*pgUsageCacheSource
	mu             sync.Mutex
	extracted      []string
	captures       int
	before         func(context.Context, []string) error
	after          func() error
	beforeAccounts func(context.Context) error
}

func (probe *usageCacheProbe) CaptureUsage(ctx context.Context, request db.UsageSourceRequest) (db.UsageSourceSnapshot, error) {
	probe.mu.Lock()
	probe.captures++
	probe.mu.Unlock()
	return probe.pgUsageCacheSource.CaptureUsage(ctx, request)
}

func (probe *usageCacheProbe) ReadUsageAccounts(ctx context.Context) (string, int64, []db.UsageSourceAccount, error) {
	if probe.beforeAccounts != nil {
		if err := probe.beforeAccounts(ctx); err != nil {
			return "", 0, nil, err
		}
	}
	return probe.pgUsageCacheSource.ReadUsageAccounts(ctx)
}

func (probe *usageCacheProbe) StreamUsage(ctx context.Context, ids []string, emit func(string, usagefacts.Fact) error) (db.UsageSourceSnapshot, error) {
	probe.mu.Lock()
	probe.extracted = append(probe.extracted, ids...)
	before, after := probe.before, probe.after
	probe.mu.Unlock()
	if before != nil {
		if err := before(ctx, ids); err != nil {
			return db.UsageSourceSnapshot{}, err
		}
	}
	snapshot, err := probe.pgUsageCacheSource.StreamUsage(ctx, ids, emit)
	if err == nil && after != nil {
		err = after()
	}
	return snapshot, err
}

func (probe *usageCacheProbe) takeExtracted() []string {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	ids := slices.Clone(probe.extracted)
	probe.extracted = nil
	slices.Sort(ids)
	return slices.Compact(ids)
}

func attachUsageCacheProbe(t *testing.T, store *Store, directory string) *usageCacheProbe {
	t.Helper()
	probe := &usageCacheProbe{pgUsageCacheSource: &pgUsageCacheSource{Store: store, target: "cache-test-target"}}
	reader, err := db.NewUsageCacheReader(directory, probe)
	require.NoError(t, err)
	store.usageCache = reader
	return probe
}

type usageAggregateReader interface {
	GetDailyUsage(context.Context, db.UsageFilter) (db.DailyUsageResult, error)
	GetTopSessionsByCost(context.Context, db.UsageFilter, int) ([]db.TopSessionEntry, error)
	GetUsageSessionCounts(context.Context, db.UsageFilter) (db.UsageSessionCounts, error)
	GetUsageMatchingSessionCount(context.Context, db.UsageFilter) (int, error)
}

func normalizeUsageTopTimes(rows []db.TopSessionEntry) {
	for i := range rows {
		if stamp, err := time.Parse(time.RFC3339Nano, rows[i].StartedAt); err == nil {
			rows[i].StartedAt = stamp.UTC().Format(time.RFC3339Nano)
		}
	}
}

func requireUsageReadsMatch(t *testing.T, direct, reader usageAggregateReader, filter db.UsageFilter) {
	t.Helper()
	wantDaily, err := direct.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	gotDaily, err := reader.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, wantDaily, gotDaily, "daily usage")
	wantTop, err := direct.GetTopSessionsByCost(t.Context(), filter, 20)
	require.NoError(t, err)
	gotTop, err := reader.GetTopSessionsByCost(t.Context(), filter, 20)
	require.NoError(t, err)
	normalizeUsageTopTimes(wantTop)
	normalizeUsageTopTimes(gotTop)
	require.Equal(t, wantTop, gotTop, "top sessions")
	wantCounts, err := direct.GetUsageSessionCounts(t.Context(), filter)
	require.NoError(t, err)
	gotCounts, err := reader.GetUsageSessionCounts(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, wantCounts, gotCounts, "session counts")
	wantMatching, err := direct.GetUsageMatchingSessionCount(t.Context(), filter)
	require.NoError(t, err)
	gotMatching, err := reader.GetUsageMatchingSessionCount(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, wantMatching, gotMatching, "matching sessions")
}

func requireCachedUsageMatchesDirect(t *testing.T, store *Store, filter db.UsageFilter) {
	t.Helper()
	direct := &Store{pg: store.pg}
	store.runtimeState().pricingMu.Lock()
	direct.SetCustomPricing(store.runtimeState().customPricing)
	store.runtimeState().pricingMu.Unlock()
	requireUsageReadsMatch(t, direct, store.usageCache, filter)
}

func TestPGUsageCacheWindowRestartAndSmallPush(t *testing.T) {
	local, syncer, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at) VALUES('older','old-project','test-machine','codex','2025-01-01'); INSERT INTO usage_events(session_id,source,model,input_tokens,output_tokens,occurred_at,dedup_key) VALUES('older','provider','paid-a',2,100,'2025-01-01','old')`)
	require.NoError(t, err)
	directory := t.TempDir()
	probe := attachUsageCacheProbe(t, store, directory)
	bounded := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	requireCachedUsageMatchesDirect(t, store, bounded)
	require.Equal(t, []string{"paid-a", "paid-b", "plan"}, probe.takeExtracted())
	require.NoError(t, store.usageCache.Close())
	probe = attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, bounded)
	require.Empty(t, probe.takeExtracted(), "restart must reuse persisted token facts")
	all := db.UsageFilter{Timezone: "UTC"}
	requireCachedUsageMatchesDirect(t, store, all)
	require.Equal(t, []string{"older"}, probe.takeExtracted(), "widening fills missing candidates")
	require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "paid-a", []db.UsageEvent{{SessionID: "paid-a", Source: "provider", Model: "paid-a", OutputTokens: 900, DedupKey: "event", OccurredAt: "2026-01-01T09:00:00Z"}}))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	requireCachedUsageMatchesDirect(t, store, all)
	require.Equal(t, []string{"paid-a"}, probe.takeExtracted(), "push refreshes only the changed session")
	require.NoError(t, store.usageCache.Close())
	probe = attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, all)
	require.Empty(t, probe.takeExtracted())
}

func TestPGUsageCacheAccountReplacementAndPricing(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `INSERT INTO cursor_usage_events(id,agent,occurred_at,model,input_tokens,output_tokens,charged_microdollars,is_headless,dedup_key) VALUES(100,'cursor','2026-01-01','paid-a',10,20,100,false,'one'),(2000000,'cursor','2026-01-01','paid-a',20,30,200,false,'two')`)
	require.NoError(t, err)
	directory := t.TempDir()
	probe := attachUsageCacheProbe(t, store, directory)
	filter := db.UsageFilter{Timezone: "UTC"}
	requireCachedUsageMatchesDirect(t, store, filter)
	probe.takeExtracted()
	var accountReads atomic.Int64
	probe.beforeAccounts = func(context.Context) error { accountReads.Add(1); return nil }
	for _, tc := range []struct {
		mutation string
		metadata bool
	}{
		{`UPDATE cursor_usage_events SET charged_microdollars=500,input_tokens=80 WHERE id=100`, false},
		{`DELETE FROM cursor_usage_events WHERE id=100; INSERT INTO cursor_usage_events(id,agent,occurred_at,model,input_tokens,output_tokens,charged_microdollars,is_headless,dedup_key) VALUES(150,'cursor','2026-01-01','paid-a',40,50,300,false,'three')`, false},
		{`UPDATE model_pricing SET output_microdollars_per_mtok=20000000 WHERE model_pattern='paid-a'`, true},
		{`DELETE FROM model_pricing WHERE model_pattern='paid-a'`, true},
		{`INSERT INTO source_project_identity_observations(source_archive_id,project,machine,observed_at) VALUES('account-journal','project-a','test-machine',now())`, true},
	} {
		before, err := probe.pgUsageCacheSource.CaptureUsage(ctx, db.UsageSourceRequest{})
		require.NoError(t, err)
		accountReads.Store(0)
		_, err = store.pg.ExecContext(ctx, tc.mutation)
		require.NoError(t, err)
		requireCachedUsageMatchesDirect(t, store, filter)
		require.Empty(t, probe.takeExtracted(), "account or metadata changes must reuse session facts")
		if tc.metadata {
			after, err := probe.pgUsageCacheSource.CaptureUsage(ctx, db.UsageSourceRequest{})
			require.NoError(t, err)
			assert.Equal(t, before.CursorHighWater, after.CursorHighWater, "metadata changes must preserve the account generation")
			assert.Zero(t, accountReads.Load(), "metadata changes must not copy the account history")
		}
	}
	store.SetCustomPricing(map[string]config.CustomModelRate{"paid-a": {OutputMicrodollarsPerMTok: 23000000}})
	requireCachedUsageMatchesDirect(t, store, filter)
	require.NoError(t, store.usageCache.Close())
	probe = attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, filter)
	require.Empty(t, probe.takeExtracted())
	store.SetCustomPricing(nil)
	requireCachedUsageMatchesDirect(t, store, filter)
}

func TestPGUsageCacheSnapshotPeersAndRanking(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at) VALUES('first','first-project','test-machine','claude','2026-01-01'),('second','second-project','test-machine','claude','2026-01-01');
 INSERT INTO messages(session_id,ordinal,role,content,timestamp,model,token_usage,claude_message_id,claude_request_id) VALUES
 ('first',0,'assistant','','2026-01-01 09:00:00.000001','paid-a','{"input_tokens":2,"output_tokens":900.0}','snapshot','request'),
 ('second',0,'assistant','','2026-01-01 09:00:00.000002','paid-a','{"input_tokens":4,"output_tokens":10}','snapshot','request')`)
	require.NoError(t, err)
	attachUsageCacheProbe(t, store, t.TempDir())
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", Project: "first-project"}
	requireCachedUsageMatchesDirect(t, store, filter)
	daily, err := store.usageCache.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, 10, daily.Totals.OutputTokens, "SQL ranking rejects a decimal-formatted integer that Go billing accepts")
	for _, mutation := range []string{`UPDATE sessions SET deleted_at=clock_timestamp() WHERE id='second'`, `UPDATE sessions SET deleted_at=NULL WHERE id='second'`, `UPDATE sessions SET group_key='job',session_name='daily job' WHERE id IN ('first','second')`} {
		_, err := store.pg.ExecContext(ctx, mutation)
		require.NoError(t, err)
		requireCachedUsageMatchesDirect(t, store, filter)
	}
	grouped := db.UsageFilter{Timezone: "UTC", TopSessionsByGroup: true}
	requireCachedUsageMatchesDirect(t, store, grouped)
}

func TestPGUsageCacheCloseFill(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	directory := t.TempDir()
	probe := attachUsageCacheProbe(t, store, directory)
	filter := db.UsageFilter{Timezone: "UTC"}
	started, release := make(chan struct{}), make(chan struct{})
	once := sync.Once{}
	probe.before = func(ctx context.Context, _ []string) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	finishRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(finishRelease)
	finished := make(chan error, 1)
	go func() { _, err := store.usageCache.GetDailyUsage(t.Context(), filter); finished <- err }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("fill did not reach extraction")
	}
	require.NoError(t, store.usageCache.Close())
	require.ErrorIs(t, <-finished, context.Canceled)
	finishRelease()
	attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, filter)
}

func TestPGUsageCacheReadOnlyRestart(t *testing.T) {
	_, syncer, _, _ := keptUsageFixture(t)
	ctx := t.Context()
	roleURL := newUsageJournalRole(t, syncer, `GRANT SELECT ON ALL TABLES IN SCHEMA `+syncer.schema+` TO ROLE`)
	reader, err := NewStore(ctx, roleURL, syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	directory := t.TempDir()
	require.NoError(t, reader.ConfigureUsageCache(ctx, directory, roleURL, syncer.schema))
	require.NotNil(t, reader.usageCache)
	filter := db.UsageFilter{Timezone: "UTC"}
	requireCachedUsageMatchesDirect(t, reader, filter)
	require.NoError(t, reader.usageCache.Close())
	reader.usageCache = nil
	require.NoError(t, reader.ConfigureUsageCache(ctx, directory, roleURL, syncer.schema))
	requireCachedUsageMatchesDirect(t, reader, filter)
}

func TestPGUsageCachePairwiseFallback(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	attachUsageCacheProbe(t, store, t.TempDir())
	ctx := t.Context()
	ranking, err := store.GetDailyUsage(ctx, db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", Breakdowns: true, PairwiseSides: true})
	require.NoError(t, err)
	require.Nil(t, ranking.Pairwise)
	request := service.UsagePairwiseComparisonRequest{UsageRequest: service.UsageRequest{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", IncludeOneShot: true}, LeftDimension: "model", LeftValue: "missing", RightDimension: "model", RightValue: "paid-a", FallbackToDefault: true}
	direct := service.NewReadOnlyBackend(&Store{pg: store.pg})
	want, err := direct.UsagePairwiseComparison(ctx, request)
	require.NoError(t, err)
	got, err := service.NewReadOnlyBackend(store).UsagePairwiseComparison(ctx, request)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, 1250, got.Left.OutputTokens+got.Right.OutputTokens)
}

func TestPGUsageCacheRecapturesMutationDuringExtraction(t *testing.T) {
	for _, tc := range []struct {
		name, mutation        string
		once, account, direct bool
	}{
		{"session metadata changed", `UPDATE sessions SET project='project-b' WHERE id='paid-a'`, true, false, false},
		{"price changed", `UPDATE model_pricing SET output_microdollars_per_mtok=17000000 WHERE model_pattern='paid-a'`, true, false, false},
		{"custom price changed", "custom-pricing", true, false, false},
		{"busy sessions", `UPDATE usage_events SET output_tokens=output_tokens+1 WHERE session_id='paid-a'`, false, false, true},
		{"busy accounts", `UPDATE cursor_usage_events SET charged_microdollars=charged_microdollars+1 WHERE id=1`, false, true, true},
		{"unrelated push", `UPDATE sessions SET display_name=display_name||'!' WHERE id='outside'`, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, store, _ := keptUsageFixture(t)
			ctx := t.Context()
			_, err := store.pg.ExecContext(ctx, `INSERT INTO cursor_usage_events(id,agent,occurred_at,model,input_tokens,output_tokens,charged_microdollars,dedup_key) VALUES(1,'cursor','2026-01-01','paid-a',1,2,3,'account-one'); INSERT INTO sessions(id,project,machine,agent,started_at,display_name) VALUES('outside','other','test-machine','codex','2025-01-01','outside')`)
			require.NoError(t, err)
			probe := attachUsageCacheProbe(t, store, t.TempDir())
			var once sync.Once
			var extractions atomic.Int64
			mutation := func(ctx context.Context) error {
				var err error
				change := func() {
					if tc.mutation == "custom-pricing" {
						store.SetCustomPricing(map[string]config.CustomModelRate{"paid-a": {OutputMicrodollarsPerMTok: 23000000}})
					} else {
						_, err = store.pg.ExecContext(ctx, tc.mutation)
					}
				}
				if tc.once {
					once.Do(change)
				} else {
					change()
				}
				return err
			}
			if tc.account {
				probe.beforeAccounts = mutation
			} else {
				probe.before = func(ctx context.Context, _ []string) error { extractions.Add(1); return mutation(ctx) }
			}
			filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
			var got db.DailyUsageResult
			if tc.direct {
				got, err = store.GetDailyUsage(ctx, filter)
			} else {
				got, err = store.usageCache.GetDailyUsage(ctx, filter)
			}
			require.NoError(t, err)
			if tc.name == "unrelated push" {
				require.Equal(t, int64(1), extractions.Load(), "unrelated writes must reuse the completed extraction")
			}
			probe.before, probe.beforeAccounts = nil, nil
			direct := &Store{pg: store.pg}
			store.runtimeState().pricingMu.Lock()
			direct.SetCustomPricing(store.runtimeState().customPricing)
			store.runtimeState().pricingMu.Unlock()
			want, err := direct.GetDailyUsage(ctx, filter)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

func TestPGUsageCacheConcurrentHandlesCannotPublishPartialWindow(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	_, err := store.pg.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,started_at) SELECT 'batch-'||lpad(n::text,4,'0'),'project-a','test-machine','codex','2026-01-01'::timestamptz FROM generate_series(0,2049)n; INSERT INTO usage_events(session_id,source,model,input_tokens,output_tokens,occurred_at,dedup_key) SELECT id,'provider','paid-a',1,2,started_at,id FROM sessions WHERE id LIKE 'batch-%'`)
	require.NoError(t, err)
	directory := t.TempDir()
	probe := attachUsageCacheProbe(t, store, directory)
	started, release := make(chan struct{}), make(chan struct{})
	once := sync.Once{}
	probe.before = func(ctx context.Context, ids []string) error {
		if slices.Contains(ids, "batch-1024") {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	finishRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(finishRelease)
	filter := db.UsageFilter{Timezone: "UTC"}
	done := make(chan error, 1)
	go func() { _, err := store.usageCache.GetDailyUsage(t.Context(), filter); done <- err }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("middle shard did not reach extraction")
	}
	select {
	case err := <-done:
		t.Fatalf("partial request returned before all shards finished: %v", err)
	default:
	}
	concurrent := &Store{pg: store.pg, ownedDB: store.DB()}
	second := attachUsageCacheProbe(t, concurrent, directory)
	t.Cleanup(func() { require.NoError(t, concurrent.usageCache.Close()) })
	requireCachedUsageMatchesDirect(t, concurrent, filter)
	finishRelease()
	require.NoError(t, <-done)
	require.NoError(t, store.usageCache.Close())
	probe = attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, filter)
	require.Empty(t, probe.takeExtracted())
	require.NotEmpty(t, second.takeExtracted())
}

func TestPGUsageCacheTopRankingControlsAndGroupedRemainder(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at,group_key)
 SELECT 'expensive-'||n,'project-a','test-machine','codex','2026-01-01'::timestamptz,'expensive-'||n FROM generate_series(1,101)n;
 INSERT INTO usage_events(session_id,source,model,input_tokens,output_tokens,occurred_at,dedup_key)
 SELECT id,'provider','paid-a',1,600,started_at,id FROM sessions WHERE id LIKE 'expensive-%'`)
	require.NoError(t, err)
	probe := attachUsageCacheProbe(t, store, t.TempDir())
	direct := &Store{pg: store.pg}
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	for _, grouped := range []bool{false, true} {
		filter.TopSessionsByGroup = grouped
		for _, selection := range []db.UsageTokenTypes{0, db.UsageTokenTypeOutput, db.UsageTokenTypeInput} {
			filter.TopSessionsSort = "cost"
			if selection != 0 {
				filter.TopSessionsSort = "tokens"
			}
			filter.TopSessionsTokenTypes = selection
			want, err := direct.GetTopSessionsByCost(ctx, filter, 1)
			require.NoError(t, err)
			normalizeUsageTopTimes(want)
			probe.mu.Lock()
			before := probe.captures
			probe.mu.Unlock()
			public, err := store.GetTopSessionsByCost(ctx, filter, 1)
			require.NoError(t, err)
			normalizeUsageTopTimes(public)
			require.Equal(t, want, public)
			probe.mu.Lock()
			captured := probe.captures
			probe.mu.Unlock()
			require.Greater(t, captured, before, "each new selection must reach the configured cache")
			repeated, err := store.GetTopSessionsByCost(ctx, filter, 1)
			require.NoError(t, err)
			normalizeUsageTopTimes(repeated)
			require.Equal(t, want, repeated)
			probe.mu.Lock()
			afterRepeat := probe.captures
			probe.mu.Unlock()
			require.Equal(t, captured, afterRepeat, "unchanged repeat visits must reuse the existing Top sessions memo")
		}
	}
	// Limit changes share the same selection but need their complete ranked rows.
	want, err := direct.GetTopSessionsByCost(ctx, filter, 3)
	require.NoError(t, err)
	got, err := store.GetTopSessionsByCost(ctx, filter, 3)
	require.NoError(t, err)
	normalizeUsageTopTimes(want)
	normalizeUsageTopTimes(got)
	require.Equal(t, want, got)
}

func TestPGUsageCacheChronologicalFractionalTimestamps(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at) VALUES
 ('earlier','project-a','test-machine','copilot','2026-01-01 09:00:00Z'),('later','project-a','test-machine','copilot','2026-01-01 09:00:00.000001Z');
 INSERT INTO messages(session_id,ordinal,role,content,timestamp,model,token_usage,source_uuid) VALUES
 ('earlier',0,'assistant','','2026-01-01 09:00:00Z','paid-a','{"output_tokens":10}','shared-uuid'),
 ('later',0,'assistant','','2026-01-01 09:00:00.000001Z','paid-a','{"output_tokens":30}','shared-uuid');
 INSERT INTO usage_events(session_id,source,model,output_tokens,cost_microdollars,cost_source,occurred_at,dedup_key) VALUES
 ('earlier','session','paid-a',0,100,'copilot-reported','2026-01-01 09:00:00Z','first-cost'),
 ('earlier','session','paid-a',0,200,'copilot-reported','2026-01-01 09:00:00.000001Z','last-cost')`)
	require.NoError(t, err)
	attachUsageCacheProbe(t, store, t.TempDir())
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	requireCachedUsageMatchesDirect(t, store, filter)
	_, err = store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at) VALUES('activity-midnight','activity-project','test-machine','codex','2026-01-01'); INSERT INTO messages(session_id,ordinal,role,content,timestamp,model) VALUES('activity-midnight',0,'assistant','','2026-01-02 00:30:00+00','activity')`)
	require.NoError(t, err)
	requireCachedUsageMatchesDirect(t, store, db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "America/New_York"})
}

func TestPGUsageCacheNullableSessionFilters(t *testing.T) {
	for _, tc := range []struct {
		name, mutation string
		filter         db.UsageFilter
	}{
		{"termination", `UPDATE sessions SET termination_status=CASE WHEN id='paid-a' THEN 'clean' ELSE NULL END`, db.UsageFilter{Termination: "clean", Timezone: "UTC"}},
		{"recency", `UPDATE sessions SET created_at=NULL,started_at=NULL,ended_at=NULL WHERE id!='paid-a'`, db.UsageFilter{ActiveSince: "2026-01-01T00:00:00Z", Timezone: "UTC"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, store, _ := keptUsageFixture(t)
			ctx := t.Context()
			_, err := store.pg.ExecContext(ctx, tc.mutation)
			require.NoError(t, err)
			attachUsageCacheProbe(t, store, t.TempDir())
			direct := &Store{pg: store.pg}
			want, err := direct.GetDailyUsage(ctx, tc.filter)
			require.NoError(t, err)
			require.Equal(t, 500, want.Totals.OutputTokens)
			requireCachedUsageMatchesDirect(t, store, tc.filter)
		})
	}
}

func TestPGUsageCachePushedNullTimestampDedup(t *testing.T) {
	for _, tc := range []struct {
		name               string
		reported, fallback bool
	}{
		{"message dedup", false, false},
		{"authoritative cost", true, false},
		{"session timestamp fallback", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, syncer, store := newUsageJournalFixture(t)
			ctx := t.Context()
			require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "model-a", OutputPerMTok: money.MustParseDollars("10")}}))
			if tc.reported {
				require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "reported", Project: "project-a", Machine: "test-machine", Agent: "copilot"}))
				require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "reported", []db.UsageEvent{
					{SessionID: "reported", Source: "session", Model: "model-a", OutputTokens: 10, Cost: new(money.Money{Microdollars: 100}), CostSource: db.CopilotReportedCostSource, DedupKey: "undated"},
					{SessionID: "reported", Source: "session", Model: "model-a", OutputTokens: 30, Cost: new(money.Money{Microdollars: 200}), CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-01-01T09:00:00Z", DedupKey: "dated"},
				}))
			} else {
				for _, id := range []string{"undated", "dated"} {
					var started *string
					if id == "dated" {
						started = new("2026-01-01T09:00:00.000001Z")
					} else if tc.fallback {
						started = new("2026-01-01T09:00:00Z")
					}
					require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Project: "project-a", Machine: "test-machine", Agent: "copilot", StartedAt: started}))
				}
				require.NoError(t, local.InsertMessages(ctx, []db.Message{
					{SessionID: "undated", Ordinal: 0, Role: "assistant", Content: "undated", Model: "model-a", TokenUsage: []byte(`{"output_tokens":10}`), SourceUUID: "shared-uuid"},
					{SessionID: "dated", Ordinal: 0, Role: "assistant", Content: "dated", Model: "model-a", Timestamp: "2026-01-01T09:00:00.000001Z", TokenUsage: []byte(`{"output_tokens":30}`), SourceUUID: "shared-uuid"},
				}))
			}
			pushed, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			require.Empty(t, pushed.Errors)
			filter := db.UsageFilter{Timezone: "UTC"}
			want, err := store.GetDailyUsage(ctx, filter)
			require.NoError(t, err)
			if tc.reported {
				require.Equal(t, int64(100), want.Totals.TotalCost.Microdollars)
			} else if tc.fallback {
				require.Equal(t, 10, want.Totals.OutputTokens)
			} else {
				require.Equal(t, 30, want.Totals.OutputTokens)
			}
			directory := t.TempDir()
			attachUsageCacheProbe(t, store, directory)
			requireCachedUsageMatchesDirect(t, store, filter)
			require.NoError(t, store.Close())
			restarted, err := NewStore(ctx, testPGURL(t), syncer.schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, restarted.Close()) })
			probe := attachUsageCacheProbe(t, restarted, directory)
			requireCachedUsageMatchesDirect(t, restarted, filter)
			require.Empty(t, probe.takeExtracted())
		})
	}
}

func TestPGUsageCacheDirectFallbackAndRecovery(t *testing.T) {
	for _, name := range []string{"initial denial", "permission revoked", "dropped journal", "malformed revision", "cache storage", "extraction failure", "extraction canceled", "extraction deadline", "late journal installation"} {
		t.Run(name, func(t *testing.T) {
			_, syncer, owner, _ := keptUsageFixture(t)
			ctx := t.Context()
			reader, roleURL := owner, testPGURL(t)
			if name == "initial denial" || name == "permission revoked" {
				roleURL = newUsageJournalRole(t, syncer, `GRANT SELECT ON ALL TABLES IN SCHEMA `+syncer.schema+` TO ROLE`)
				var err error
				reader, err = NewStore(ctx, roleURL, syncer.schema, true)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reader.Close()) })
			}
			revoke := func() {
				var role string
				require.NoError(t, reader.pg.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
				_, err := owner.pg.ExecContext(ctx, `REVOKE SELECT ON usage_session_changes FROM `+role)
				require.NoError(t, err)
			}
			if name == "initial denial" {
				revoke()
			}
			if name == "malformed revision" {
				_, err := owner.pg.ExecContext(ctx, `UPDATE sync_metadata SET value='broken' WHERE key='usage_change_revision'`)
				require.NoError(t, err)
			}
			if name == "late journal installation" {
				_, err := owner.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
				require.NoError(t, err)
				_, err = owner.pg.ExecContext(ctx, `INSERT INTO cursor_usage_events(id,agent,occurred_at,model,input_tokens,output_tokens,charged_microdollars,dedup_key) VALUES (2,'cursor','2026-01-01','paid-a',1,7,77,'old-account-a'), (1000000,'cursor','2026-01-01','paid-a',1,11,111,'old-account-b')`)
				require.NoError(t, err)
			}
			directory := t.TempDir()
			var probe *usageCacheProbe
			extraction := name == "extraction failure" || name == "extraction canceled" || name == "extraction deadline"
			if extraction || name == "cache storage" {
				probe = attachUsageCacheProbe(t, reader, directory)
			} else {
				require.NoError(t, reader.ConfigureUsageCache(ctx, directory, roleURL, syncer.schema))
			}
			configured := reader.usageCache
			require.NotNil(t, configured)
			filter := db.UsageFilter{Timezone: "UTC"}
			if name != "initial denial" && name != "late journal installation" && name != "malformed revision" {
				requireCachedUsageMatchesDirect(t, reader, filter)
				_, err := owner.pg.ExecContext(ctx, `UPDATE usage_events SET output_tokens=5000 WHERE session_id='paid-a'`)
				require.NoError(t, err)
			}
			var failure error
			switch name {
			case "permission revoked":
				revoke()
			case "dropped journal":
				_, err := owner.pg.ExecContext(ctx, `DROP TABLE usage_session_changes CASCADE`)
				require.NoError(t, err)
			case "cache storage":
				paths, err := filepath.Glob(filepath.Join(directory, "usage-cache-v*.db"))
				require.NoError(t, err)
				require.Len(t, paths, 1)
				damaged, err := sql.Open("sqlite3", paths[0])
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, damaged.Close()) })
				_, err = damaged.ExecContext(ctx, `DROP TABLE usage_rollup_timezones`)
				require.NoError(t, err)
				filter.Project, filter.From, filter.To = "project-a", "2026-01-01", "2026-01-01"
			case "extraction failure", "extraction canceled", "extraction deadline":
				failure = map[string]error{"extraction failure": errors.New("injected remote extraction failure"), "extraction canceled": context.Canceled, "extraction deadline": context.DeadlineExceeded}[name]
				probe.before = func(context.Context, []string) error { return failure }
			}
			_, err := configured.GetDailyUsage(ctx, filter)
			switch name {
			case "initial denial", "permission revoked":
				require.True(t, isInsufficientPrivilege(err), "cache must preserve the PostgreSQL permission error: %v", err)
			case "dropped journal", "late journal installation":
				require.True(t, isUndefinedTable(err), "cache must preserve the PostgreSQL missing-table error: %v", err)
			case "cache storage", "malformed revision":
				require.Error(t, err)
			default:
				require.ErrorIs(t, err, failure)
			}
			if extraction {
				// An internal fill cancellation still permits its active caller's live read.
				got, err := reader.GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				require.Equal(t, 5750, got.Totals.OutputTokens)
				probe.before = nil
				requireCachedUsageMatchesDirect(t, reader, filter)
			} else {
				for _, grouped := range []bool{false, true} {
					filter.TopSessionsByGroup = grouped
					requireUsageReadsMatch(t, &Store{pg: reader.pg}, reader, filter)
				}
			}
			if name == "late journal installation" {
				require.NoError(t, EnsureSchema(ctx, syncer.pg, syncer.schema))
				require.Same(t, configured, reader.usageCache)
				for _, restart := range []bool{false, true} {
					if restart {
						require.NoError(t, reader.usageCache.Close())
						reader.usageCache = nil
						require.NoError(t, reader.ConfigureUsageCache(ctx, directory, roleURL, syncer.schema))
					}
					got, err := reader.usageCache.GetDailyUsage(ctx, filter)
					require.NoError(t, err)
					require.Equal(t, 1268, got.Totals.OutputTokens)
					require.Equal(t, int64(8938), got.Totals.TotalCost.Microdollars)
					requireCachedUsageMatchesDirect(t, reader, filter)
				}
			}
		})
	}
}

func TestPGUsageCacheCancelledCallerSkipsDirectRead(t *testing.T) {
	_, _, store, counter := keptUsageFixture(t)
	attachUsageCacheProbe(t, store, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := counter.queryAttempts.Load()
	filter := db.UsageFilter{Timezone: "UTC"}
	_, err := store.GetDailyUsage(ctx, filter)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.GetTopSessionsByCost(ctx, filter, 20)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.GetUsageSessionCounts(ctx, filter)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.GetUsageMatchingSessionCount(ctx, filter)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, counter.queryAttempts.Load(), "cancelled cache requests must not start direct queries")
}
