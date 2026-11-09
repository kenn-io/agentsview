//go:build pgtest

package postgres

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/usagefacts"
)

type usageCacheProbe struct {
	*pgUsageCacheSource
	mu             sync.Mutex
	extracted      []string
	before         func(context.Context, []string) error
	after          func() error
	beforeAccounts func(context.Context) error
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

func requireCachedUsageMatchesDirect(t *testing.T, store *Store, filter db.UsageFilter) {
	t.Helper()
	direct := &Store{pg: store.pg}
	store.runtimeState().pricingMu.Lock()
	direct.SetCustomPricing(store.runtimeState().customPricing)
	store.runtimeState().pricingMu.Unlock()
	wantDaily, err := direct.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	gotDaily, err := store.usageCache.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, wantDaily, gotDaily, "daily usage")
	wantTop, err := direct.GetTopSessionsByCost(t.Context(), filter, 20)
	require.NoError(t, err)
	gotTop, err := store.GetTopSessionsByCost(t.Context(), filter, 20)
	require.NoError(t, err)
	require.Equal(t, wantTop, gotTop, "top sessions")
	wantCounts, err := direct.GetUsageSessionCounts(t.Context(), filter)
	require.NoError(t, err)
	gotCounts, err := store.usageCache.GetUsageSessionCounts(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, wantCounts, gotCounts, "session counts")
	wantMatching, err := direct.GetUsageMatchingSessionCount(t.Context(), filter)
	require.NoError(t, err)
	gotMatching, err := store.usageCache.GetUsageMatchingSessionCount(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, wantMatching, gotMatching, "matching sessions")
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
	for _, mutation := range []string{
		`UPDATE cursor_usage_events SET charged_microdollars=500,input_tokens=80 WHERE id=100`,
		`DELETE FROM cursor_usage_events WHERE id=100; INSERT INTO cursor_usage_events(id,agent,occurred_at,model,input_tokens,output_tokens,charged_microdollars,is_headless,dedup_key) VALUES(150,'cursor','2026-01-01','paid-a',40,50,300,false,'three')`,
		`UPDATE model_pricing SET output_microdollars_per_mtok=20000000 WHERE model_pattern='paid-a'`,
		`DELETE FROM model_pricing WHERE model_pattern='paid-a'`,
	} {
		_, err := store.pg.ExecContext(ctx, mutation)
		require.NoError(t, err)
		requireCachedUsageMatchesDirect(t, store, filter)
		require.Empty(t, probe.takeExtracted(), "account or pricing changes must reuse session facts")
	}
	require.NoError(t, store.usageCache.Close())
	probe = attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, filter)
	require.Empty(t, probe.takeExtracted())
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
 ('first',0,'assistant','','2026-01-01 09:00:00.000001','paid-a','{"input_tokens":2,"output_tokens":900.5}','snapshot','request'),
 ('second',0,'assistant','','2026-01-01 09:00:00.000002','paid-a','{"input_tokens":4,"output_tokens":10}','snapshot','request')`)
	require.NoError(t, err)
	directory := t.TempDir()
	probe := attachUsageCacheProbe(t, store, directory)
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", Project: "first-project"}
	requireCachedUsageMatchesDirect(t, store, filter)
	daily, err := store.usageCache.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, 10, daily.Totals.OutputTokens, "SQL ranking ignores a fraction that Go billing truncates")
	probe.takeExtracted()
	for _, mutation := range []string{`UPDATE sessions SET deleted_at=clock_timestamp() WHERE id='second'`, `UPDATE sessions SET deleted_at=NULL WHERE id='second'`, `UPDATE sessions SET group_key='job',session_name='daily job' WHERE id IN ('first','second')`} {
		_, err := store.pg.ExecContext(ctx, mutation)
		require.NoError(t, err)
		requireCachedUsageMatchesDirect(t, store, filter)
	}
	grouped := db.UsageFilter{Timezone: "UTC", TopSessionsByGroup: true}
	requireCachedUsageMatchesDirect(t, store, grouped)
	require.NoError(t, store.usageCache.Close())
	attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, grouped)
	_, err = store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at) VALUES('activity-midnight','activity-project','test-machine','codex','2026-01-01'); INSERT INTO messages(session_id,ordinal,role,content,timestamp,model) VALUES('activity-midnight',0,'assistant','','2026-01-02 00:30:00+00','activity')`)
	require.NoError(t, err)
	requireCachedUsageMatchesDirect(t, store, db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "America/New_York"})
}

func TestPGUsageCacheCancelledWaiterAndRetry(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	directory := t.TempDir()
	probe := attachUsageCacheProbe(t, store, directory)
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
	filter := db.UsageFilter{Timezone: "UTC"}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() { _, err := store.usageCache.GetDailyUsage(ctx, filter); finished <- err }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("fill did not reach extraction")
	}
	cancel()
	require.ErrorIs(t, <-finished, context.Canceled)
	second := make(chan error, 1)
	go func() { _, err := store.usageCache.GetDailyUsage(t.Context(), filter); second <- err }()
	finishRelease()
	require.NoError(t, <-second)
	requireCachedUsageMatchesDirect(t, store, filter)
	require.NoError(t, store.usageCache.Close())
	probe = attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, filter)
	require.Empty(t, probe.takeExtracted(), "cancelled waiter must leave shared completed work reusable")
	_, err := store.pg.ExecContext(t.Context(), `UPDATE sessions SET updated_at=clock_timestamp() WHERE id='paid-a'`)
	require.NoError(t, err)
	failure := errors.New("injected remote extraction failure")
	probe.before = func(context.Context, []string) error { return failure }
	_, err = store.usageCache.GetDailyUsage(t.Context(), filter)
	require.ErrorIs(t, err, failure)
	probe.before = nil
	requireCachedUsageMatchesDirect(t, store, filter)
}

func TestPGUsageCacheReadOnlyAndRecreatedJournal(t *testing.T) {
	_, syncer, store, _ := keptUsageFixture(t)
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
	// Recreate only the journal while retaining its supported version and clock.
	// The old cache must not validate against the new relation identity.
	_, err = store.pg.ExecContext(ctx, `ALTER TABLE usage_session_changes RENAME TO usage_session_changes_retired; CREATE TABLE usage_session_changes (LIKE usage_session_changes_retired INCLUDING ALL); INSERT INTO usage_session_changes SELECT * FROM usage_session_changes_retired; GRANT SELECT ON usage_session_changes TO PUBLIC; DROP TABLE usage_session_changes_retired CASCADE; UPDATE usage_events SET output_tokens=1000 WHERE session_id='paid-a'`)
	require.NoError(t, err)
	requireCachedUsageMatchesDirect(t, reader, filter)
	_, err = store.pg.ExecContext(ctx, `DROP TABLE usage_session_changes CASCADE`)
	require.NoError(t, err)
	// An unavailable optional journal keeps the direct aggregate path usable.
	direct := &Store{pg: store.pg}
	store.runtimeState().pricingMu.Lock()
	direct.SetCustomPricing(store.runtimeState().customPricing)
	store.runtimeState().pricingMu.Unlock()
	want, err := direct.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	got, err := reader.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, want, got)
	for _, grouped := range []bool{false, true} {
		filter.TopSessionsByGroup = grouped
		want, err := direct.GetTopSessionsByCost(ctx, filter, 20)
		require.NoError(t, err)
		got, err := reader.GetTopSessionsByCost(ctx, filter, 20)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestPGUsageCacheRecapturesMutationDuringExtraction(t *testing.T) {
	for _, mutation := range []string{`UPDATE usage_events SET output_tokens=1100 WHERE session_id='paid-a'`, `UPDATE model_pricing SET output_microdollars_per_mtok=17000000 WHERE model_pattern='paid-a'`, "custom-pricing"} {
		t.Run(mutation, func(t *testing.T) {
			_, _, store, _ := keptUsageFixture(t)
			probe := attachUsageCacheProbe(t, store, t.TempDir())
			once := sync.Once{}
			probe.before = func(ctx context.Context, _ []string) error {
				var err error
				once.Do(func() {
					if mutation == "custom-pricing" {
						store.SetCustomPricing(map[string]config.CustomModelRate{"paid-a": {OutputMicrodollarsPerMTok: 23000000}})
					} else {
						_, err = store.pg.ExecContext(ctx, mutation)
					}
				})
				return err
			}
			filter := db.UsageFilter{Timezone: "UTC"}
			_, err := store.usageCache.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			requireCachedUsageMatchesDirect(t, store, filter)
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

func TestPGUsageCacheCloseCancelsOwnedFill(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	directory := t.TempDir()
	probe := attachUsageCacheProbe(t, store, directory)
	started := make(chan struct{})
	once := sync.Once{}
	probe.before = func(ctx context.Context, _ []string) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.usageCache.GetDailyUsage(t.Context(), db.UsageFilter{Timezone: "UTC"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("fill did not reach extraction")
	}
	require.NoError(t, store.usageCache.Close())
	require.Error(t, <-done)
	attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, db.UsageFilter{Timezone: "UTC"})
}

func TestPGUsageCacheTopRankingControlsAndGroupedRemainder(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at,group_key)
 SELECT 'expensive-'||n,'project-a','test-machine','codex','2026-01-01'::timestamptz,'expensive-'||n FROM generate_series(1,101)n;
 INSERT INTO usage_events(session_id,source,model,input_tokens,output_tokens,occurred_at,dedup_key)
 SELECT id,'provider','paid-a',1,600,started_at,id FROM sessions WHERE id LIKE 'expensive-%'`)
	require.NoError(t, err)
	attachUsageCacheProbe(t, store, t.TempDir())
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
			got, err := store.GetTopSessionsByCost(ctx, filter, 1)
			require.NoError(t, err)
			require.Equal(t, want, got)
			if selection == db.UsageTokenTypeOutput {
				require.Equal(t, "paid-b", got[0].SessionID)
			}
			if grouped {
				require.Len(t, got, 2, "omitted groups retain their totals in the remainder")
			}
		}
	}
}

func TestPGUsageCacheBusySourceReturnsCurrentDirectRead(t *testing.T) {
	for _, accounts := range []bool{false, true} {
		t.Run(map[bool]string{false: "sessions", true: "accounts"}[accounts], func(t *testing.T) {
			_, _, store, _ := keptUsageFixture(t)
			ctx := t.Context()
			_, err := store.pg.ExecContext(ctx, `INSERT INTO cursor_usage_events(id,agent,occurred_at,model,input_tokens,output_tokens,charged_microdollars,dedup_key) VALUES(1,'cursor','2026-01-01','paid-a',1,2,3,'account-one')`)
			require.NoError(t, err)
			directory := t.TempDir()
			probe := attachUsageCacheProbe(t, store, directory)
			mutation := func(ctx context.Context) error {
				query := `UPDATE usage_events SET output_tokens=output_tokens+1 WHERE session_id='paid-a'`
				if accounts {
					query = `UPDATE cursor_usage_events SET charged_microdollars=charged_microdollars+1 WHERE id=1`
				}
				_, err := store.pg.ExecContext(ctx, query)
				return err
			}
			if accounts {
				probe.beforeAccounts = mutation
			} else {
				probe.before = func(ctx context.Context, _ []string) error { return mutation(ctx) }
			}
			filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
			got, err := store.GetDailyUsage(ctx, filter)
			require.NoError(t, err)
			probe.before, probe.beforeAccounts = nil, nil
			direct := &Store{pg: store.pg}
			want, err := direct.GetDailyUsage(ctx, filter)
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.NoError(t, store.usageCache.Close())
			attachUsageCacheProbe(t, store, directory)
			requireCachedUsageMatchesDirect(t, store, filter)
		})
	}
}

func TestPGUsageCacheUnrelatedPushDuringEveryExtraction(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	_, err := store.pg.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,started_at,display_name) VALUES('outside','other','test-machine','codex','2025-01-01','outside')`)
	require.NoError(t, err)
	probe := attachUsageCacheProbe(t, store, t.TempDir())
	probe.before = func(ctx context.Context, _ []string) error {
		_, err := store.pg.ExecContext(ctx, `UPDATE sessions SET display_name=display_name||'!' WHERE id='outside'`)
		return err
	}
	_, err = store.usageCache.GetDailyUsage(t.Context(), db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"})
	require.NoError(t, err)
}

func TestPGUsageCacheChronologicalFractionalTimestamps(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "messages", true: "session-fallback"}[fallback], func(t *testing.T) {
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
			if fallback {
				_, err := store.pg.ExecContext(ctx, `UPDATE messages SET timestamp=NULL WHERE session_id='earlier'`)
				require.NoError(t, err)
			}
			directory := t.TempDir()
			attachUsageCacheProbe(t, store, directory)
			filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
			requireCachedUsageMatchesDirect(t, store, filter)
			require.NoError(t, store.usageCache.Close())
			attachUsageCacheProbe(t, store, directory)
			requireCachedUsageMatchesDirect(t, store, filter)
		})
	}
}
