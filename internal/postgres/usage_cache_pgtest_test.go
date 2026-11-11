//go:build pgtest

package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
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

func requireCachedUsageMatchesDirect(t *testing.T, store *Store, filter db.UsageFilter) {
	t.Helper()
	direct := &Store{pg: store.pg}
	store.runtimeState().pricingMu.Lock()
	direct.SetCustomPricing(store.runtimeState().customPricing)
	store.runtimeState().pricingMu.Unlock()
	require.NoError(t, completeUsageParity(t.Context(), direct, store.usageCache, filter, "", false))
}

// readWarmedUsage reads daily usage from a configured cache, waiting once
// for the background pass that a deferring read queues.
func readWarmedUsage(t *testing.T, store *Store, filter db.UsageFilter) db.DailyUsageResult {
	t.Helper()
	got, err := store.usageCache.GetDailyUsage(t.Context(), filter)
	if errors.Is(err, db.ErrUsageCacheWarming) {
		require.NoError(t, store.usageCache.WaitWarm(t.Context()))
		got, err = store.usageCache.GetDailyUsage(t.Context(), filter)
	}
	require.NoError(t, err)
	return got
}

func TestPGUsageCacheWindowRestartAndSmallPush(t *testing.T) {
	local, syncer, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at) VALUES('older','old-project','test-machine','codex','2025-01-01'); INSERT INTO usage_events(session_id,source,model,input_tokens,output_tokens,occurred_at,dedup_key) VALUES('older','provider','paid-a',2,100,'2025-01-01','old'); INSERT INTO cursor_usage_events(id,agent,occurred_at,model,output_tokens,charged_microdollars,dedup_key) VALUES(1,'cursor','2026-01-01','paid-a',2,200,'persisted-account')`)
	require.NoError(t, err)
	directory := t.TempDir()
	probe := attachUsageCacheProbe(t, store, directory)
	bounded := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	var extractions atomic.Int64
	probe.before = func(ctx context.Context, _ []string) error {
		extractions.Add(1)
		_, err := store.pg.ExecContext(ctx, `UPDATE sessions SET display_name=COALESCE(display_name,'')||'!' WHERE id='older'`)
		return err
	}
	_, err = store.GetDailyUsage(ctx, bounded)
	require.NoError(t, err)
	require.Equal(t, []string{"paid-a", "paid-b", "plan"}, probe.takeExtracted())
	require.Equal(t, int64(1), extractions.Load(), "unrelated writes must reuse the completed extraction")
	requireCachedUsageMatchesDirect(t, store, bounded)
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
	store.SetCustomPricing(map[string]config.CustomModelRate{"paid-a": {OutputMicrodollarsPerMTok: 23000000}})
	requireCachedUsageMatchesDirect(t, store, all)
	require.NoError(t, store.usageCache.Close())
	probe = attachUsageCacheProbe(t, store, directory)
	requireCachedUsageMatchesDirect(t, store, all)
	require.Empty(t, probe.takeExtracted())

	for i, stamp := range []string{"2026-09-06T05:00:00Z", "2026-09-07T15:00:00Z"} {
		id := fmt.Sprintf("santiago-%d", i)
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Project: "project-a", Machine: "test-machine", Agent: "codex", StartedAt: new(stamp)}))
		require.NoError(t, local.ReplaceSessionUsageEvents(ctx, id, []db.UsageEvent{{SessionID: id, Source: "provider", Model: "paid-a", OutputTokens: 20 * (i + 1), DedupKey: id, OccurredAt: stamp}}))
	}
	pushed, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, pushed.Errors)
	for _, restart := range []bool{false, true} {
		if restart {
			require.NoError(t, store.usageCache.Close())
			attachUsageCacheProbe(t, store, directory)
		}
		for _, window := range []struct{ from, to string }{{"2026-09-06", "2026-09-06"}, {"2026-09-05", "2026-09-07"}, {"2026-08-10", "2026-09-08"}} {
			t.Run(fmt.Sprintf("%s..%s/restart=%t", window.from, window.to, restart), func(t *testing.T) {
				requireCachedUsageMatchesDirect(t, store, db.UsageFilter{From: window.from, To: window.to, Timezone: "America/Santiago"})
			})
		}
	}
}

func TestPGUsageCacheRestartDefaultPairwiseParity(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	directory := t.TempDir()
	attachUsageCacheProbe(t, store, directory)
	req := service.UsagePairwiseComparisonRequest{
		UsageRequest:      service.UsageRequest{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", IncludeOneShot: true},
		FallbackToDefault: true,
	}
	direct := &Store{pg: store.pg}
	want, err := service.NewReadOnlyBackend(direct).UsagePairwiseComparison(t.Context(), req)
	require.NoError(t, err)
	require.NotEmpty(t, want.Left.Value)
	require.NotEmpty(t, want.Right.Value)
	_, err = store.usageCache.GetDailyUsage(t.Context(), db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"})
	require.NoError(t, err)
	require.NoError(t, store.usageCache.Close())
	reopened := &Store{pg: store.pg, ownedDB: store.DB()}
	attachUsageCacheProbe(t, reopened, directory)
	t.Cleanup(func() { require.NoError(t, reopened.usageCache.Close()) })
	got, err := service.NewReadOnlyBackend(reopened).UsagePairwiseComparison(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.NoError(t, reopened.usageCache.Close())
	reopened = &Store{pg: store.pg, ownedDB: store.DB()}
	attachUsageCacheProbe(t, reopened, directory)
	backend := service.NewReadOnlyBackend(reopened)
	_, err = backend.UsageSummary(t.Context(), req.UsageRequest)
	require.NoError(t, err)
	got, err = backend.UsagePairwiseComparison(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, want, got)
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
	for _, mutation := range []string{
		`UPDATE cursor_usage_events SET charged_microdollars=500,input_tokens=80 WHERE id=100`,
		`DELETE FROM cursor_usage_events WHERE id=100; INSERT INTO cursor_usage_events(id,agent,occurred_at,model,input_tokens,output_tokens,charged_microdollars,is_headless,dedup_key) VALUES(150,'cursor','2026-01-01','paid-a',40,50,300,false,'three')`,
		`UPDATE model_pricing SET output_microdollars_per_mtok=20000000 WHERE model_pattern='paid-a'`,
		`DELETE FROM model_pricing WHERE model_pattern='paid-a'`,
		`INSERT INTO source_project_identity_observations(source_archive_id,project,machine,observed_at) VALUES('account-journal','project-a','test-machine',now())`,
	} {
		before, err := probe.pgUsageCacheSource.CaptureUsage(ctx, db.UsageSourceRequest{})
		require.NoError(t, err)
		accountReads.Store(0)
		_, err = store.pg.ExecContext(ctx, mutation)
		require.NoError(t, err)
		requireCachedUsageMatchesDirect(t, store, filter)
		require.Empty(t, probe.takeExtracted(), "account or metadata changes must reuse session facts")
		after, err := probe.pgUsageCacheSource.CaptureUsage(ctx, db.UsageSourceRequest{})
		require.NoError(t, err)
		assert.Greater(t, after.CursorHighWater, before.CursorHighWater)
		assert.Equal(t, int64(1), accountReads.Load())
	}
	store.SetCustomPricing(map[string]config.CustomModelRate{"paid-a": {OutputMicrodollarsPerMTok: 23000000}})
	requireCachedUsageMatchesDirect(t, store, filter)
	store.SetCustomPricing(nil)
	requireCachedUsageMatchesDirect(t, store, filter)
}

func TestPGUsageCacheSnapshotPeersAndRanking(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at) VALUES('first','first-project','test-machine','claude','2026-01-01'),('second','second-project','test-machine','claude','2026-01-01');
 INSERT INTO messages(session_id,ordinal,role,content,timestamp,model,token_usage,claude_message_id,claude_request_id) VALUES
 ('first',0,'assistant','','2026-01-01 09:00:00.000001','paid-a','{"input_tokens":2,"output_tokens":900.0}','snapshot','request'),
 ('second',0,'assistant','','2026-01-01 09:00:00.000002','paid-b','{"input_tokens":4,"output_tokens":10}','snapshot','request')`)
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

func TestPGUsageCacheConcurrentHandlesCannotPublishPartialWindow(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	directory := t.TempDir()
	probe := attachUsageCacheProbe(t, store, directory)
	started, release := make(chan struct{}), make(chan struct{})
	once := sync.Once{}
	probe.before = func(ctx context.Context, _ []string) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
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
		t.Fatal("fill did not reach extraction")
	}
	select {
	case err := <-done:
		t.Fatalf("partial request returned before all shards finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	concurrent := &Store{pg: store.pg, ownedDB: store.DB()}
	second := attachUsageCacheProbe(t, concurrent, directory)
	t.Cleanup(func() { require.NoError(t, concurrent.usageCache.Close()) })
	requireCachedUsageMatchesDirect(t, concurrent, filter)
	finishRelease()
	require.NoError(t, <-done)
	require.NotEmpty(t, second.takeExtracted())
}

func TestPGUsageCacheTopRankingControlsAndGroupedRemainder(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,started_at,group_key)
 SELECT 'expensive-'||n,'project-a','test-machine','codex','2026-01-01'::timestamptz,CASE WHEN n<=2 THEN 'shared' ELSE '' END FROM generate_series(1,4)n;
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
			normalizeUsageTopTimes(want)
			public, err := store.GetTopSessionsByCost(ctx, filter, 1)
			require.NoError(t, err)
			normalizeUsageTopTimes(public)
			require.Equal(t, want, public)
		}
	}
	// Limit changes share the same selection but need their complete ranked rows.
	filter.TopSessionsSort = "tokens"
	filter.TopSessionsTokenTypes = db.UsageTokenTypeOutput
	want, err := direct.GetTopSessionsByCost(ctx, filter, 3)
	require.NoError(t, err)
	got, err := store.GetTopSessionsByCost(ctx, filter, 3)
	require.NoError(t, err)
	normalizeUsageTopTimes(want)
	normalizeUsageTopTimes(got)
	require.Equal(t, want, got)
	require.Len(t, got, 4)
	require.Equal(t, "shared", got[0].GroupKey)
	require.Equal(t, 1200, got[0].OutputTokens)
	require.Empty(t, got[2].GroupKey)
	require.Equal(t, 600, got[2].OutputTokens)
	require.Empty(t, got[3].SessionID)
	require.Equal(t, 1100, got[3].OutputTokens)
}

func TestPGUsageCacheSessionFilters(t *testing.T) {
	for _, tc := range []struct {
		name, mutation string
		filter         db.UsageFilter
		wantTokens     int
	}{
		{"termination", `UPDATE sessions SET termination_status=CASE WHEN id='paid-a' THEN 'clean' ELSE NULL END`, db.UsageFilter{Termination: "clean", Timezone: "UTC"}, 500},
		{"recency", `UPDATE sessions SET created_at=NULL,started_at=NULL,ended_at=NULL WHERE id!='paid-a'`, db.UsageFilter{ActiveSince: "2026-01-01T00:00:00Z", Timezone: "UTC"}, 500},
		{"local activity day", `INSERT INTO sessions(id,project,machine,agent,started_at) VALUES('activity-midnight','activity-project','test-machine','codex','2026-01-01'); INSERT INTO messages(session_id,ordinal,role,content,timestamp,model) VALUES('activity-midnight',0,'assistant','','2026-01-02 00:30:00+00','activity')`, db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "America/New_York"}, 1250},
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
			require.Equal(t, tc.wantTokens, want.Totals.OutputTokens)
			requireCachedUsageMatchesDirect(t, store, tc.filter)
		})
	}
}

func TestPGUsageCacheChronologicalTimestamps(t *testing.T) {
	for _, tc := range []struct {
		name, toOnly string
		warm         bool
	}{
		{name: "chronological"},
		{name: "to-only ordinary/cold", toOnly: "ordinary"},
		{name: "to-only ordinary/warm", toOnly: "ordinary", warm: true},
		{name: "to-only exceptions/cold", toOnly: "exceptions"},
		{name: "to-only exceptions/warm", toOnly: "exceptions", warm: true},
		{name: "to-only undated billed session/cold", toOnly: "undated billed session"},
		{name: "to-only undated billed session/warm", toOnly: "undated billed session", warm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, syncer, store := newUsageJournalFixture(t)
			ctx := t.Context()
			require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "model-a", OutputPerMTok: money.MustParseDollars("10")}}))
			if tc.toOnly != "" {
				require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session-a", Project: "project-a", Machine: "test-machine", Agent: "copilot"}))
				messages := []db.Message{
					{SessionID: "session-a", Ordinal: 0, Role: "assistant", Content: "undated", Model: "model-a", TokenUsage: []byte(`{"output_tokens":10}`)},
					{SessionID: "session-a", Ordinal: 1, Role: "user", Content: "dated", Timestamp: "2026-01-01T09:00:00Z"},
				}
				if tc.toOnly != "undated billed session" {
					messages[1].Role, messages[1].Model, messages[1].TokenUsage = "assistant", "model-a", []byte(`{"output_tokens":30}`)
				}
				if tc.toOnly == "exceptions" {
					// A shared ID with mixed models keeps the undated rows on the
					// exception tier; the dated row stays an ordinary rollup.
					messages[0].SourceUUID, messages[1].SourceUUID = "undated-uuid", "dated-uuid"
					messages = append(messages, db.Message{SessionID: "session-a", Ordinal: 2, Role: "assistant", Content: "undated duplicate", Model: "model-b", TokenUsage: []byte(`{"output_tokens":10}`), SourceUUID: "undated-uuid"})
				}
				require.NoError(t, local.InsertMessages(ctx, messages))
			} else {
				for _, session := range []struct{ id, started string }{
					{"undated", ""}, {"dated", "2026-01-01T09:00:00.000001Z"},
					{"fallback-a", "2026-01-01T09:00:00Z"}, {"fallback-b", "2026-01-01T09:00:00.000001Z"},
					{"fraction-a", "2026-01-01T09:00:00Z"}, {"fraction-b", "2026-01-01T09:00:00.000001Z"},
					{"fractional", "2026-01-01T09:00:00Z"}, {"reported", ""},
				} {
					var started *string
					if session.started != "" {
						started = new(session.started)
					}
					require.NoError(t, local.UpsertSession(ctx, db.Session{ID: session.id, Project: "project-a", Machine: "test-machine", Agent: "copilot", StartedAt: started}))
				}
				require.NoError(t, local.InsertMessages(ctx, []db.Message{
					{SessionID: "undated", Ordinal: 0, Role: "assistant", Model: "model-a", TokenUsage: []byte(`{"output_tokens":10}`), SourceUUID: "shared-uuid"},
					{SessionID: "dated", Ordinal: 0, Role: "assistant", Model: "model-a", Timestamp: "2026-01-01T09:00:00.000001Z", TokenUsage: []byte(`{"output_tokens":30}`), SourceUUID: "shared-uuid"},
					{SessionID: "fallback-a", Ordinal: 0, Role: "assistant", Model: "model-a", TokenUsage: []byte(`{"output_tokens":10}`), SourceUUID: "fallback-uuid"},
					{SessionID: "fallback-b", Ordinal: 0, Role: "assistant", Model: "model-a", Timestamp: "2026-01-01T09:00:00.000001Z", TokenUsage: []byte(`{"output_tokens":30}`), SourceUUID: "fallback-uuid"},
					{SessionID: "fraction-a", Ordinal: 0, Role: "assistant", Model: "model-a", Timestamp: "2026-01-01T09:00:00Z", TokenUsage: []byte(`{"output_tokens":10}`), SourceUUID: "fraction-uuid"},
					{SessionID: "fraction-b", Ordinal: 0, Role: "assistant", Model: "model-a", Timestamp: "2026-01-01T09:00:00.000001Z", TokenUsage: []byte(`{"output_tokens":30}`), SourceUUID: "fraction-uuid"},
				}))
				require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "fractional", []db.UsageEvent{
					{SessionID: "fractional", Source: "session", Model: "model-a", Cost: new(money.Money{Microdollars: 100}), CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-01-01T09:00:00Z", DedupKey: "first-cost"},
					{SessionID: "fractional", Source: "session", Model: "model-a", Cost: new(money.Money{Microdollars: 200}), CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-01-01T09:00:00.000001Z", DedupKey: "last-cost"},
				}))
				require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "reported", []db.UsageEvent{
					{SessionID: "reported", Source: "message", Model: "model-a", OutputTokens: 10, Cost: new(money.Money{Microdollars: 100}), CostSource: db.CopilotReportedCostSource, DedupKey: "undated"},
					{SessionID: "reported", Source: "message", Model: "model-a", OutputTokens: 30, Cost: new(money.Money{Microdollars: 200}), CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-01-01T09:00:00Z", DedupKey: "dated"},
				}))
			}
			pushed, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			require.Empty(t, pushed.Errors)
			filter := db.UsageFilter{Timezone: "UTC"}
			if tc.toOnly != "" {
				filter.To = "2026-01-01"
			}
			want, err := store.GetDailyUsage(ctx, filter)
			require.NoError(t, err)
			if tc.toOnly == "undated billed session" {
				require.Zero(t, want.Totals.OutputTokens)
			}
			attachUsageCacheProbe(t, store, t.TempDir())
			if tc.warm {
				all, err := store.usageCache.GetDailyUsage(ctx, db.UsageFilter{Timezone: "UTC"})
				require.NoError(t, err)
				require.Equal(t, want.Totals.OutputTokens+10, all.Totals.OutputTokens)
			}
			requireCachedUsageMatchesDirect(t, store, filter)
		})
	}
}

func TestPGUsageCacheDirectFallbackAndRecovery(t *testing.T) {
	for _, name := range []string{"granted role", "initial denial", "cancelled caller", "extraction failure", "busy sessions", "busy accounts", "pricing mutation", "late journal installation"} {
		t.Run(name, func(t *testing.T) {
			_, syncer, owner, counter := keptUsageFixture(t)
			ctx := t.Context()
			reader, roleURL := owner, testPGURL(t)
			if name == "initial denial" || name == "granted role" {
				roleURL = newUsageJournalRole(t, syncer, `GRANT SELECT ON ALL TABLES IN SCHEMA `+syncer.schema+` TO ROLE`)
				var err error
				reader, err = NewStore(ctx, roleURL, syncer.schema, true)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reader.Close()) })
				if name == "initial denial" {
					var role string
					require.NoError(t, reader.pg.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
					_, err = owner.pg.ExecContext(ctx, `REVOKE SELECT ON usage_session_changes FROM `+role)
					require.NoError(t, err)
				}
			}
			if name == "late journal installation" {
				_, err := owner.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
				require.NoError(t, err)
				_, err = owner.pg.ExecContext(ctx, `INSERT INTO cursor_usage_events(id,agent,occurred_at,model,input_tokens,output_tokens,charged_microdollars,dedup_key) VALUES (2,'cursor','2026-01-01','paid-a',1,7,77,'old-account-a'), (1000000,'cursor','2026-01-01','paid-a',1,11,111,'old-account-b')`)
				require.NoError(t, err)
			}
			directory := t.TempDir()
			filter := db.UsageFilter{Timezone: "UTC"}
			if name == "cancelled caller" {
				attachUsageCacheProbe(t, reader, directory)
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				counter.mu.Lock()
				before := maps.Clone(counter.queries)
				counter.mu.Unlock()
				_, err := reader.GetUsageSessionCounts(cancelled, filter)
				require.ErrorIs(t, err, context.Canceled)
				counter.mu.Lock()
				after := maps.Clone(counter.queries)
				counter.mu.Unlock()
				require.Equal(t, before, after, "cancelled cache requests must not start direct queries")
				return
			}
			if name == "busy sessions" || name == "busy accounts" || name == "pricing mutation" {
				_, err := reader.pg.ExecContext(ctx, `INSERT INTO cursor_usage_events(id,agent,occurred_at,model,output_tokens,charged_microdollars,dedup_key) VALUES(1,'cursor','2026-01-01','paid-a',2,3,'account-one')`)
				require.NoError(t, err)
				probe := attachUsageCacheProbe(t, reader, directory)
				var mutations atomic.Int64
				mutation := func(ctx context.Context) error {
					mutations.Add(1)
					query := `UPDATE usage_events SET output_tokens=output_tokens+1 WHERE session_id='paid-a'`
					if name == "busy accounts" {
						query = `UPDATE cursor_usage_events SET charged_microdollars=charged_microdollars+1 WHERE id=1`
					} else if name == "pricing mutation" {
						query = `UPDATE model_pricing SET output_microdollars_per_mtok=17000000 WHERE model_pattern='paid-a'`
					}
					_, err := reader.pg.ExecContext(ctx, query)
					return err
				}
				if name == "busy accounts" {
					probe.beforeAccounts = mutation
				} else {
					probe.before = func(ctx context.Context, _ []string) error { return mutation(ctx) }
				}
				read := reader.GetDailyUsage
				if name == "pricing mutation" {
					read = reader.usageCache.GetDailyUsage
				}
				got, err := read(ctx, filter)
				require.NoError(t, err)
				require.Positive(t, mutations.Load(), "the source mutation must occur during the read")
				probe.before, probe.beforeAccounts = nil, nil
				want, err := (&Store{pg: reader.pg}).GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				require.Equal(t, want, got)
				if name == "pricing mutation" {
					require.Equal(t, int64(12253), got.Totals.TotalCost.Microdollars)
				}
				return
			}
			if name == "extraction failure" {
				probe := attachUsageCacheProbe(t, reader, directory)
				requireCachedUsageMatchesDirect(t, reader, filter)
				_, err := owner.pg.ExecContext(ctx, `UPDATE usage_events SET output_tokens=5000 WHERE session_id='paid-a'`)
				require.NoError(t, err)
				// A fill can stop while its caller remains active. Classify the caller,
				// rather than the fill error, when deciding whether to read directly.
				var output bytes.Buffer
				previous, flags := log.Writer(), log.Flags()
				log.SetOutput(&output)
				log.SetFlags(0)
				t.Cleanup(func() { log.SetOutput(previous); log.SetFlags(flags) })
				for _, failure := range []error{errors.New("injected remote extraction failure"), context.Canceled, context.DeadlineExceeded} {
					probe.before = func(context.Context, []string) error { return failure }
					_, err := reader.usageCache.GetDailyUsage(ctx, filter)
					require.ErrorIs(t, err, failure)
					cacheErr := err
					output.Reset()
					active := &Store{pg: reader.pg, usageCache: reader.usageCache}
					got, err := active.GetDailyUsage(ctx, filter)
					require.NoError(t, err)
					require.Equal(t, "postgres usage cache: falling back to direct read: "+cacheErr.Error()+"\n", output.String())
					require.Equal(t, 5750, got.Totals.OutputTokens)
					for _, grouped := range []bool{false, true} {
						filter.TopSessionsByGroup = grouped
						require.NoError(t, completeUsageParity(t.Context(), &Store{pg: reader.pg}, active, filter, "paid-a", false))
					}
				}
				probe.before = nil
				requireCachedUsageMatchesDirect(t, reader, filter)
				return
			}
			if name == "granted role" {
				counter := &usageReadCounter{storeExecutor: reader.pg, queries: map[string]int{}}
				reader.pg = counter
				first, err := reader.GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				queries := maps.Clone(counter.queries)
				second, err := reader.GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				require.Equal(t, first, second)
				require.Equal(t, queries, counter.queries)
				attachUsageCacheProbe(t, reader, directory)
				requireCachedUsageMatchesDirect(t, reader, filter)
				require.NoError(t, reader.usageCache.Close())
				probe := attachUsageCacheProbe(t, reader, directory)
				requireCachedUsageMatchesDirect(t, reader, filter)
				require.Empty(t, probe.takeExtracted(), "a read-only restart must reuse persisted facts")
				reader.usageCache.DeferWork()
				_, err = owner.pg.ExecContext(ctx, `INSERT INTO cursor_usage_events(id,agent,occurred_at,model,output_tokens,charged_microdollars,dedup_key) VALUES(1,'cursor','2026-01-01','paid-a',10,100,'read-only-account')`)
				require.NoError(t, err)
				_, err = reader.usageCache.GetDailyUsage(ctx, filter)
				require.ErrorIs(t, err, db.ErrUsageCacheWarming)
				want, err := (&Store{pg: reader.pg}).GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				got, err := reader.GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				require.Equal(t, want, got)
				require.NoError(t, reader.usageCache.WaitWarm(ctx))
				requireCachedUsageMatchesDirect(t, reader, filter)
				var output bytes.Buffer
				previous := log.Writer()
				log.SetOutput(&output)
				t.Cleanup(func() { log.SetOutput(previous) })
				_, err = owner.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
				require.NoError(t, err)
				filter.From = "2026-01-01"
				got, err = reader.GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				want, err = (&Store{pg: reader.pg}).GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				require.Equal(t, want, got)
				assert.NotContains(t, output.String(), "falling back to direct read")
				return
			}
			require.NoError(t, reader.ConfigureUsageCache(ctx, directory, roleURL, syncer.schema))
			require.NotNil(t, reader.usageCache)
			if name == "late journal installation" {
				var output bytes.Buffer
				previous := log.Writer()
				log.SetOutput(&output)
				t.Cleanup(func() { log.SetOutput(previous) })
				want, err := (&Store{pg: reader.pg}).GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				for range 10 {
					got, err := reader.GetDailyUsage(ctx, filter)
					require.NoError(t, err)
					require.Equal(t, want, got)
				}
				require.NoError(t, reader.usageCache.WaitWarm(ctx))
				assert.NotContains(t, output.String(), "falling back to direct read")
				assert.NotContains(t, output.String(), "warming usage cache")
			}
			_, err := reader.usageCache.GetDailyUsage(ctx, filter)
			require.ErrorIs(t, err, db.ErrUsageSourceUnavailable)
			for _, grouped := range []bool{false, true} {
				filter.TopSessionsByGroup = grouped
				require.NoError(t, completeUsageParity(t.Context(), &Store{pg: reader.pg}, reader, filter, "paid-a", false))
			}
			if name == "late journal installation" {
				require.NoError(t, EnsureSchema(ctx, syncer.pg, syncer.schema))
				for _, restart := range []bool{false, true} {
					if restart {
						require.NoError(t, reader.usageCache.Close())
						reader.usageCache = nil
						require.NoError(t, reader.ConfigureUsageCache(ctx, directory, roleURL, syncer.schema))
					}
					got := readWarmedUsage(t, reader, filter)
					require.Equal(t, 1268, got.Totals.OutputTokens)
					require.Equal(t, int64(8938), got.Totals.TotalCost.Microdollars)
					requireCachedUsageMatchesDirect(t, reader, filter)
				}
			}
		})
	}
}

func TestPGUsageCacheAccountTotalsMatchDirect(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session-a", Project: "project-a", Machine: "test-machine", Agent: "copilot", StartedAt: new("2026-01-01T09:00:00Z")}))
	require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "session-a", []db.UsageEvent{{SessionID: "session-a", Source: "provider", Model: "model-a", OutputTokens: 30, Cost: new(money.Money{Microdollars: 300}), CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-01-01T09:00:00Z", DedupKey: "event"}}))
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "model-a", OutputPerMTok: money.MustParseDollars("10")}, {ModelPattern: "model-b", OutputPerMTok: money.MustParseDollars("5")}}))
	require.NoError(t, local.InsertCursorUsageEvents(ctx, []db.CursorUsageEvent{
		{OccurredAt: "2026-01-01T08:00:00Z", Model: "model-a", OutputTokens: 10, Charged: money.Money{Microdollars: 100}, DedupKey: "session-a:provider:event"},
		{OccurredAt: "2026-01-01T10:00:00Z", Model: "model-a", OutputTokens: 20, Charged: money.Money{Microdollars: 200}, DedupKey: "human-a"},
		{OccurredAt: "2026-01-01T10:00:00Z", Model: "model-a", OutputTokens: 25, Charged: money.Money{Microdollars: 250}, DedupKey: "same-time"},
		{OccurredAt: "2026-01-02T10:00:00Z", Model: "model-b", OutputTokens: 40, Charged: money.Money{Microdollars: 400}, IsHeadless: true, DedupKey: "session-b:provider:event"},
		{Agent: "other-account", OccurredAt: "2026-01-02T11:00:00Z", Model: "model-b", OutputTokens: 50, Charged: money.Money{Microdollars: 500}, DedupKey: "other-b"},
		{OccurredAt: "2026-01-03T11:00:00Z", Model: "unpriced", OutputTokens: 60, Charged: money.Money{Microdollars: 600}, DedupKey: "later"},
	}))
	pushed, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, pushed.Errors)
	attachUsageCacheProbe(t, store, t.TempDir())
	filters := []db.UsageFilter{
		{Timezone: "UTC"},
		{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", Breakdowns: true},
		{Timezone: "America/New_York"},
		{From: "2026-01-02", To: "2026-01-02", Timezone: "UTC"},
		{Agent: "cursor", Timezone: "UTC"},
		{ExcludeAgent: "cursor", Timezone: "UTC"},
		{AutomatedScope: "human", Timezone: "UTC"},
		{AutomatedScope: "automated", Timezone: "UTC"},
		{Model: "model-b", Timezone: "UTC"},
		{ExcludeModel: "model-a", Timezone: "UTC"},
	}
	for _, filter := range filters {
		requireCachedUsageMatchesDirect(t, store, filter)
	}
	top, err := store.usageCache.GetTopSessionsByCost(ctx, db.UsageFilter{Timezone: "UTC"}, 20)
	require.NoError(t, err)
	require.Len(t, top, 1)
	require.Equal(t, "session-a", top[0].SessionID)
	require.Equal(t, 30, top[0].OutputTokens)
	require.Equal(t, int64(300), top[0].Cost.Microdollars)
	// A session that later reports an account key moves that account row
	// back to request-time deduplication. The account rows stay unchanged.
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session-b", Project: "project-a", Machine: "test-machine", Agent: "copilot", StartedAt: new("2026-01-02T09:00:00Z")}))
	require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "session-b", []db.UsageEvent{{SessionID: "session-b", Source: "provider", Model: "model-b", OutputTokens: 70, OccurredAt: "2026-01-02T09:00:00Z", DedupKey: "event"}}))
	pushed, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, pushed.Errors)
	for _, filter := range filters {
		requireCachedUsageMatchesDirect(t, store, filter)
	}
}

func TestPGUsageCacheDefersAccountChange(t *testing.T) {
	_, _, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	attachUsageCacheProbe(t, store, t.TempDir())
	filter := db.UsageFilter{Timezone: "UTC"}
	requireCachedUsageMatchesDirect(t, store, filter)
	store.usageCache.DeferWork()
	before, err := store.usageCache.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	_, err = store.pg.ExecContext(ctx, `INSERT INTO cursor_usage_events(id,agent,occurred_at,model,output_tokens,charged_microdollars,dedup_key) VALUES(1,'cursor','2026-01-01','paid-a',3,300,'account-two')`)
	require.NoError(t, err)
	_, err = store.usageCache.GetDailyUsage(ctx, filter)
	require.ErrorIs(t, err, db.ErrUsageCacheWarming)
	want, err := (&Store{pg: store.pg}).GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	got, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.NoError(t, store.usageCache.WaitWarm(ctx))
	requireCachedUsageMatchesDirect(t, store, filter)
	cached, err := store.usageCache.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, want.Totals.TotalCost, cached.Totals.TotalCost)
	require.Equal(t, int64(300), cached.Totals.TotalCost.Microdollars-before.Totals.TotalCost.Microdollars)
	assert.NotContains(t, output.String(), "postgres usage cache: falling back to direct read")
}
