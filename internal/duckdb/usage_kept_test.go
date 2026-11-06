//go:build !(windows && arm64)

package duckdb

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

func TestUsageKeptMirrorReplacementAndPricing(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 10_000_000}})
	first, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.005"), first.Totals.TotalCost)
	_, err = store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	_, err = store.GetPlanUsageSessionCounts(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 3, reads)
	next := filepath.Join(dir, "next.duckdb")
	keptActivityMirror(t, next, "session-b", 900)
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	changed, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 900, changed.Totals.OutputTokens)
	require.Equal(t, money.MustParseDollars("0.009"), changed.Totals.TotalCost)
	top, err := store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Len(t, top, 1)
	require.Equal(t, "session-b", top[0].SessionID)
	_, err = store.GetPlanUsageSessionCounts(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 6, reads)
	store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
	repriced, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.018"), repriced.Totals.TotalCost)
	top, err = store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.018"), top[0].Cost)
	_, err = store.GetPlanUsageSessionCounts(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 9, reads)
}

func TestUsageKeptSameStampReplacementDuringPublish(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	info, err := os.Stat(path)
	require.NoError(t, err)
	next := filepath.Join(dir, "next.duckdb")
	keptActivityMirror(t, next, "session-b", 900)
	nextInfo, err := os.Stat(next)
	require.NoError(t, err)
	require.Equal(t, info.Size(), nextInfo.Size())
	require.NoError(t, os.Chtimes(next, info.ModTime(), info.ModTime()))
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	stamp := store.mirrorStamp()
	paused := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	unpause := func() { resumeOnce.Do(func() { close(resume) }) }
	// Sizing runs after the read finishes, before the memo publishes it.
	store.dailyUsage.Size = func(result db.DailyUsageResult) int64 {
		if result.Totals.OutputTokens == 500 {
			close(paused)
			<-resume
		}
		return 1
	}
	var first db.DailyUsageResult
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		first, firstErr = store.GetDailyUsage(t.Context(), db.UsageFilter{})
	}()
	defer func() {
		unpause()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("paused Usage read did not finish")
		}
	}()
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("Usage read did not reach publication")
	}
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	require.NotEmpty(t, store.aliasPath, "replacement must be adopted")
	require.Equal(t, stamp, store.mirrorStamp(), "replacement must preserve the file and WAL stamp")
	unpause()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Usage read did not finish after replacement")
	}
	require.NoError(t, firstErr)
	require.Equal(t, 500, first.Totals.OutputTokens)
	changed, err := store.GetDailyUsage(t.Context(), db.UsageFilter{})
	require.NoError(t, err)
	require.Equal(t, 900, changed.Totals.OutputTokens)
}

func TestUsageKeptBorrowedHandleReadsEveryTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	borrowed := NewStoreFromDB(store.DB())
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	for range 2 {
		result, err := borrowed.GetDailyUsage(t.Context(), db.UsageFilter{})
		require.NoError(t, err)
		require.Equal(t, 500, result.Totals.OutputTokens)
		top, err := borrowed.GetTopSessionsByCost(t.Context(), db.UsageFilter{}, 10)
		require.NoError(t, err)
		require.Len(t, top, 1)
		_, err = borrowed.GetPlanUsageSessionCounts(t.Context(), db.UsageFilter{})
		require.NoError(t, err)
	}
	require.Equal(t, 6, reads)
}

func TestUsageKeptReleasesPlanProgressCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptUsageMirror(t, path)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	request := func() weak.Pointer[[1024]byte] {
		payload := new([1024]byte)
		ref := weak.Make(payload)
		counts, err := store.GetPlanUsageSessionCounts(t.Context(), db.UsageFilter{
			Progress: func(string) { runtime.KeepAlive(payload) },
		})
		require.NoError(t, err)
		require.Equal(t, 1, counts.PlanOnly)
		return ref
	}()
	require.Eventually(t, func() bool {
		runtime.GC()
		return request.Value() == nil
	}, 5*time.Second, 10*time.Millisecond)
}

func TestUsageKeptTopSessionNameOverBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET display_name = 'Short title', session_name = repeat('x', 67108865)`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	for range 2 {
		top, err := store.GetTopSessionsByCost(t.Context(), db.UsageFilter{}, 1)
		require.NoError(t, err)
		require.Len(t, top, 1)
		require.Equal(t, "Short title", top[0].DisplayName)
		require.Len(t, top[0].SessionName, (64<<20)+1)
	}
	require.Equal(t, 2, reads, "hidden session names must count toward the kept byte cap")
}

func TestUsageKeptDistinguishesEmptyProjectLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	f := db.UsageFilter{Project: "does-not-exist"}
	missing, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Zero(t, missing.Totals.OutputTokens)
	f.ProjectLabels = []string{}
	all, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 500, all.Totals.OutputTokens)
	fresh, err := NewStoreFromDB(store.DB()).GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, fresh, all)
	f.ProjectLabels = nil
	missing, err = store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Zero(t, missing.Totals.OutputTokens)
}

func keptUsageMirror(t *testing.T, path string) {
	t.Helper()
	keptActivityMirror(t, path, "session-a", 500)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions (id, project, agent, started_at, ended_at, message_count, user_message_count)
 VALUES ('plan-only', 'alpha', 'codex-cloud', TIMESTAMP '2026-01-01 00:00:00', TIMESTAMP '2026-01-01 00:01:00', 3, 1)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO session_plan_usage (session_id, has_plan_usage) VALUES ('plan-only', true)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO messages (id, session_id, ordinal, role, content, timestamp, model)
 VALUES (3, 'plan-only', 0, 'user', 'hello', TIMESTAMP '2026-01-01 00:00:00', ''),
 (4, 'plan-only', 1, 'assistant', 'answer', TIMESTAMP '2026-01-01 00:01:00', 'plan-a'),
 (5, 'plan-only', 2, 'assistant', 'answer', TIMESTAMP '2026-01-01 00:01:00', 'plan-b')`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO messages (id, session_id, ordinal, role, content, timestamp, model, token_usage, output_tokens)
 VALUES (6, 'session-a', 2, 'assistant', 'answer', TIMESTAMP '2026-01-01 00:01:00', 'other-model', '{"output_tokens":250}', 250)`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestUsageKeptArrivalOrdersAndAliases(t *testing.T) {
	for _, rankingFirst := range []bool{false, true} {
		for _, aliases := range []map[string]string{nil, {"plan-a": "group", "plan-b": "group"}} {
			t.Run(fmt.Sprintf("ranking first %t aliases %t", rankingFirst, aliases != nil), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "mirror.duckdb")
				keptUsageMirror(t, path)
				store, err := NewStore(t.Context(), path)
				require.NoError(t, err)
				defer store.Close()
				fresh := NewStoreFromDB(store.DB())
				reads := 0
				previous := analyticsQueryObserver
				analyticsQueryObserver = func(string) { reads++ }
				t.Cleanup(func() { analyticsQueryObserver = previous })
				summary := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", Breakdowns: true, ModelAliases: aliases}
				ranking := summary
				ranking.ModelAliases = nil
				ranking.SkipSessionCounts = true
				filters := []db.UsageFilter{summary, ranking}
				if rankingFirst {
					slices.Reverse(filters)
				}
				for range 2 {
					for _, f := range filters {
						got, err := store.GetDailyUsage(t.Context(), f)
						require.NoError(t, err)
						require.Equal(t, 750, got.Totals.OutputTokens)
						counts, err := store.GetPlanUsageSessionCounts(t.Context(), f)
						require.NoError(t, err)
						require.Equal(t, 1, counts.PlanOnly)
						if f.ModelAliases != nil {
							require.Equal(t, map[string]int{"group": 1}, counts.PlanOnlyByModel)
						} else {
							require.Equal(t, map[string]int{"plan-a": 1, "plan-b": 1}, counts.PlanOnlyByModel)
						}
						top, err := store.GetTopSessionsByCost(t.Context(), f, 10)
						require.NoError(t, err)
						before := reads
						folded := f
						folded.Breakdowns = false
						folded.SkipSessionCounts = true
						folded.TopSessionsSort = "tokens"
						folded.TopSessionsTokenTypes = db.UsageTokenTypeOutput
						againCounts, err := store.GetPlanUsageSessionCounts(t.Context(), folded)
						require.NoError(t, err)
						require.Equal(t, counts, againCounts)
						againTop, err := store.GetTopSessionsByCost(t.Context(), folded, 10)
						require.NoError(t, err)
						require.Equal(t, top, againTop)
						require.Equal(t, before, reads)
						want, err := fresh.GetDailyUsage(t.Context(), f)
						require.NoError(t, err)
						require.Equal(t, want, got)
						wantCounts, err := fresh.GetPlanUsageSessionCounts(t.Context(), f)
						require.NoError(t, err)
						require.Equal(t, wantCounts, counts)
						wantTop, err := fresh.GetTopSessionsByCost(t.Context(), f, 10)
						require.NoError(t, err)
						require.Equal(t, wantTop, top)
						reads = before
					}
				}
				require.Equal(t, 3, reads)
			})
		}
	}
}

func TestUsageKeptUncacheableTerminations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	f := db.UsageFilter{Termination: "active"}
	for range 2 {
		_, err := store.GetDailyUsage(t.Context(), f)
		require.NoError(t, err)
		_, err = store.GetPlanUsageSessionCounts(t.Context(), f)
		require.NoError(t, err)
		_, err = store.GetTopSessionsByCost(t.Context(), f, 10)
		require.NoError(t, err)
	}
	require.Equal(t, 6, reads)
}

func TestUsageKeptServicePairwiseModelGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptUsageMirror(t, path)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	groups := service.NewModelGroups(map[string]string{"kept-model": "paid-group", "other-model": "other-group", "plan-a": "plan-group", "plan-b": "plan-group"})
	backend := service.NewReadOnlyBackend(store, service.WithModelGroups(groups))
	var mu sync.Mutex
	reads := map[string]int{}
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(query string) { mu.Lock(); defer mu.Unlock(); reads[query]++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	req := service.UsageRequest{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", IncludeOneShot: true, GroupModels: true}
	summary, err := backend.UsageSummary(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 1, summary.PlanOnlySessions)
	require.Len(t, reads, 2)
	summaryReads := maps.Clone(reads)
	pairReq := service.UsagePairwiseComparisonRequest{UsageRequest: req, LeftDimension: "model", LeftValue: "missing", RightDimension: "model", RightValue: "paid-group", FallbackToDefault: true}
	got, err := backend.UsagePairwiseComparison(t.Context(), pairReq)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"paid-group", "other-group"}, []string{got.Left.Value, got.Right.Value})
	require.Equal(t, 750, got.Left.OutputTokens+got.Right.OutputTokens)
	for query := range summaryReads {
		require.Equal(t, 1, reads[query])
	}
	fresh := service.NewReadOnlyBackend(NewStoreFromDB(store.DB()), service.WithModelGroups(groups))
	want, err := fresh.UsagePairwiseComparison(t.Context(), pairReq)
	require.NoError(t, err)
	require.Equal(t, want, got)
	// Grouping a returned day must leave the kept raw models intact.
	rawFilter, err := service.BuildUsageFilter(req, groups)
	require.NoError(t, err)
	raw, err := store.GetDailyUsage(t.Context(), rawFilter)
	require.NoError(t, err)
	require.Contains(t, raw.Daily[0].ModelsUsed, "kept-model")
}
