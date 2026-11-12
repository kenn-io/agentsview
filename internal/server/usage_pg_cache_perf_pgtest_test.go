//go:build fts5 && pgtest

package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/storage"
)

// TestUsagePGCachePerf compares Usage page loads served by the persistent
// PostgreSQL usage cache against direct PostgreSQL reads. It is a measuring
// tool that reports heap usage: set AGENTSVIEW_USAGE_PG_PERF=1 and TEST_PG_URL.
// AGENTSVIEW_BENCH_SESSIONS, AGENTSVIEW_BENCH_MESSAGES,
// AGENTSVIEW_BENCH_ACCOUNTS and AGENTSVIEW_BENCH_REPEATS size the fixture.
func TestUsagePGCachePerf(t *testing.T) {
	if os.Getenv("AGENTSVIEW_USAGE_PG_PERF") == "" {
		t.Skip("set AGENTSVIEW_USAGE_PG_PERF=1 to measure the PostgreSQL usage cache")
	}
	pgURL := os.Getenv("TEST_PG_URL")
	require.NotEmpty(t, pgURL, "TEST_PG_URL is required")
	sessions := perfEnvInt(t, "AGENTSVIEW_BENCH_SESSIONS", 3000)
	messages := perfEnvInt(t, "AGENTSVIEW_BENCH_MESSAGES", 20)
	accounts := perfEnvInt(t, "AGENTSVIEW_BENCH_ACCOUNTS", 100_000)
	repeats := perfEnvInt(t, "AGENTSVIEW_BENCH_REPEATS", 3)
	only := os.Getenv("AGENTSVIEW_USAGE_PG_PERF_ONLY")

	if only == "" || only == "sessions" {
		t.Run("sessions", func(t *testing.T) {
			fixture := newUsagePerfFixture(t, pgURL, sessions, messages, accounts)
			fixture.measureSessions(t, repeats)
		})
	}
	if only == "" || only == "accounts" {
		t.Run("accounts", func(t *testing.T) {
			fixture := newUsagePerfFixture(t, pgURL, 3, messages, accounts)
			fixture.measureAccounts(t, repeats)
		})
	}
}

func perfEnvInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	require.NoError(t, err)
	require.Positive(t, value)
	return value
}

type usagePerfFixture struct {
	pgURL, schema string
	local         *db.DB
	syncer        *postgres.Sync
	sessions      int
	nextAccount   int
	priceStep     int
	pushStep      int
}

func newUsagePerfFixture(t *testing.T, pgURL string, sessions, messages, accounts int) *usagePerfFixture {
	t.Helper()
	ctx := t.Context()
	suffix := make([]byte, 6)
	_, err := rand.Read(suffix)
	require.NoError(t, err)
	schema := "agentsview_usage_perf_" + hex.EncodeToString(suffix)
	local, err := db.Open(ctx, filepath.Join(t.TempDir(), "perf.db"))
	require.NoError(t, err)
	syncer, err := postgres.New(ctx, pgURL, schema, local, "perf-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = syncer.Close()
		_ = local.Close()
		admin, err := sql.Open("pgx", pgURL)
		if err == nil {
			_, _ = admin.ExecContext(context.WithoutCancel(ctx), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			_ = admin.Close()
		}
	})
	require.NoError(t, syncer.EnsureSchema(ctx))
	f := &usagePerfFixture{pgURL: pgURL, schema: schema, local: local, syncer: syncer, sessions: sessions}
	started := time.Now()
	f.seed(t, messages, accounts)
	t.Logf("seeded sessions=%d messages/session=%d accounts=%d in %s", sessions, messages, accounts, time.Since(started).Round(time.Millisecond))
	return f
}

var usagePerfModels = []string{"perf-cheap", "perf-premium", "perf-reported", "perf-unpriced"}

// Sessions spread evenly from 2026-05-01 through 2026-08-10, so the page
// windows cover roughly 7%, 30% and all of them.
func usagePerfDay(i, count int) time.Time {
	start := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	span := 101
	return start.AddDate(0, 0, (i*span)/max(count, 1))
}

func (f *usagePerfFixture) seed(t *testing.T, messages, accounts int) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, f.local.UpsertModelPricing([]db.ModelPricing{
		{ModelPattern: "perf-cheap", InputPerMTok: money.Money{Microdollars: 500_000}, OutputPerMTok: money.Money{Microdollars: 1_000_000}},
		{ModelPattern: "perf-premium", InputPerMTok: money.Money{Microdollars: 2_000_000}, OutputPerMTok: money.Money{Microdollars: 3_000_000}},
	}))
	batch := make([]db.Message, 0, 4096)
	flush := func() {
		if len(batch) > 0 {
			require.NoError(t, f.local.InsertMessages(ctx, batch))
			batch = batch[:0]
		}
	}
	for i := range f.sessions {
		id := "perf-" + strconv.Itoa(i)
		day := usagePerfDay(i, f.sessions)
		started := day.Format(time.RFC3339)
		require.NoError(t, f.local.UpsertSession(ctx, db.Session{
			ID: id, Project: "project-" + strconv.Itoa(i%12), Machine: "perf-machine",
			Agent: []string{"claude", "codex", "cursor"}[i%3], StartedAt: &started,
			MessageCount: messages, UserMessageCount: 2,
		}))
		for ordinal := range messages {
			batch = append(batch, db.Message{
				SessionID: id, Ordinal: ordinal, Role: "assistant",
				Timestamp: day.Add(time.Duration(ordinal) * time.Minute).Format(time.RFC3339),
				Model:     usagePerfModels[(i+ordinal)%len(usagePerfModels)],
				TokenUsage: json.RawMessage(fmt.Sprintf(
					`{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":300,"cache_read_input_tokens":2400}`,
					1200+i%17+ordinal, 480+i%13+ordinal%11)),
				ClaudeMessageID: fmt.Sprintf("perf-msg-%d-%d", i, ordinal),
				ClaudeRequestID: fmt.Sprintf("perf-req-%d-%d", i, ordinal),
			})
			if len(batch) == cap(batch) {
				flush()
			}
		}
	}
	flush()
	f.addAccounts(t, accounts)
	f.push(t)
}

// Account rows carry unique billing identities, the common case for an
// imported Cursor team export.
func (f *usagePerfFixture) addAccounts(t *testing.T, count int) {
	t.Helper()
	events := make([]db.CursorUsageEvent, 0, min(count, 5000))
	for range count {
		i := f.nextAccount
		f.nextAccount++
		day := usagePerfDay(i%101, 101).Add(time.Duration(i%1440) * time.Minute)
		events = append(events, db.CursorUsageEvent{
			OccurredAt: day.Format(time.RFC3339), Model: usagePerfModels[i%2],
			Kind: "Included", InputTokens: 900 + i%31, OutputTokens: 300 + i%19,
			CacheReadTokens: 1000, Charged: money.Money{Microdollars: int64(1000 + i%97)},
			IsHeadless: i%9 == 0, DedupKey: "perf-account-" + strconv.Itoa(i),
		})
		if len(events) == cap(events) {
			require.NoError(t, f.local.InsertCursorUsageEvents(t.Context(), events))
			events = events[:0]
		}
	}
	require.NoError(t, f.local.InsertCursorUsageEvents(t.Context(), events))
}

func (f *usagePerfFixture) push(t *testing.T) {
	t.Helper()
	result, err := f.syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
}

// smallPush rewrites the newest three sessions' final message.
func (f *usagePerfFixture) smallPush(t *testing.T) {
	t.Helper()
	f.pushStep++
	step := f.pushStep
	var batch []db.Message
	for i := f.sessions - 3; i < f.sessions; i++ {
		id := "perf-" + strconv.Itoa(max(i, 0))
		day := usagePerfDay(max(i, 0), f.sessions)
		batch = append(batch, db.Message{
			SessionID: id, Ordinal: 1000 + step, Role: "assistant",
			Timestamp: day.Add(23 * time.Hour).Format(time.RFC3339), Model: "perf-cheap",
			TokenUsage: json.RawMessage(`{"input_tokens":77,"output_tokens":11}`),
		})
	}
	require.NoError(t, f.local.InsertMessages(t.Context(), batch))
	f.push(t)
}

func (f *usagePerfFixture) pricePush(t *testing.T) {
	t.Helper()
	f.priceStep++
	require.NoError(t, f.local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "perf-premium", InputPerMTok: money.Money{Microdollars: 2_000_000 + int64(f.priceStep)},
		OutputPerMTok: money.Money{Microdollars: 3_000_000},
	}}))
	f.push(t)
}

func (f *usagePerfFixture) accountPush(t *testing.T) {
	t.Helper()
	f.addAccounts(t, 10)
	f.push(t)
}

func (f *usagePerfFixture) directServer(t *testing.T) (*Server, func()) {
	t.Helper()
	store, err := postgres.NewStore(t.Context(), f.pgURL, f.schema, true)
	require.NoError(t, err)
	return &Server{db: store, sessions: service.NewReadOnlyBackend(store)}, func() { require.NoError(t, store.Close()) }
}

func (f *usagePerfFixture) cachedServer(t *testing.T, dir string) (*Server, func()) {
	t.Helper()
	store, err := postgres.NewStore(t.Context(), f.pgURL, f.schema, true)
	require.NoError(t, err)
	require.NoError(t, store.ConfigureUsageCache(t.Context(), dir, f.pgURL, f.schema))
	return &Server{db: store, sessions: service.NewReadOnlyBackend(store)}, func() { require.NoError(t, store.Close()) }
}

// waitWarm waits for the background preparation that earlier reads queued
// and reports its duration and peak heap.
func waitWarm(t *testing.T, s *Server) usagePerfSample {
	t.Helper()
	store, ok := s.db.(*postgres.Store)
	require.True(t, ok)
	heap := startHeapPeak()
	started := time.Now()
	require.NoError(t, store.WaitUsageCacheWarm(t.Context()))
	elapsed := time.Since(started)
	return usagePerfSample{elapsed: elapsed, heap: heap.finish()}
}

func logUsageWarm(t *testing.T, scenario, window string, warm usagePerfSample) {
	t.Helper()
	// Compare PostgreSQL and SQLite process peaks in the page benchmark;
	// this in-process heap sample is diagnostic only.
	t.Logf("%-26s %-12s background warm=%8s heap=%6.1fMiB", scenario, window,
		warm.elapsed.Round(time.Millisecond), float64(warm.heap)/(1<<20))
}

// heapPeak samples the live heap until stopped and reports the highest value.
type heapPeak struct {
	stop chan struct{}
	done chan struct{}
	peak atomic.Uint64
}

func startHeapPeak() *heapPeak {
	runtime.GC()
	h := &heapPeak{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		var stats runtime.MemStats
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			runtime.ReadMemStats(&stats)
			if stats.HeapAlloc > h.peak.Load() {
				h.peak.Store(stats.HeapAlloc)
			}
			select {
			case <-h.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return h
}

func (h *heapPeak) finish() uint64 {
	close(h.stop)
	<-h.done
	return h.peak.Load()
}

type usagePerfSample struct {
	elapsed time.Duration
	heap    uint64
}

func measurePage(t *testing.T, s *Server, from string, concurrent int) (usagePerfSample, [3][]byte) {
	t.Helper()
	heap := startHeapPeak()
	started := time.Now()
	var wg sync.WaitGroup
	results := make([][3][]byte, concurrent)
	errs := make([]error, concurrent)
	for i := range concurrent {
		wg.Go(func() { results[i], errs[i] = readUsageDayTotalsPage(t.Context(), s, from) })
	}
	wg.Wait()
	elapsed := time.Since(started)
	peak := heap.finish()
	for _, err := range errs {
		require.NoError(t, err)
	}
	return usagePerfSample{elapsed: elapsed, heap: peak}, results[0]
}

func medianSample(samples []usagePerfSample) usagePerfSample {
	slices.SortFunc(samples, func(a, b usagePerfSample) int { return int(a.elapsed - b.elapsed) })
	heap := uint64(0)
	for _, s := range samples {
		heap = max(heap, s.heap)
	}
	return usagePerfSample{elapsed: samples[len(samples)/2].elapsed, heap: heap}
}

func logUsagePerf(t *testing.T, scenario, window string, direct, cached usagePerfSample) {
	t.Helper()
	ratio := float64(direct.elapsed) / float64(max(cached.elapsed, time.Microsecond))
	verdict := "ok"
	if cached.elapsed > direct.elapsed+direct.elapsed/10 {
		verdict = "SLOWER"
	}
	t.Logf("%-26s %-12s direct=%8s cached=%8s speedup=%5.2fx heap direct=%6.1fMiB cached=%6.1fMiB %s",
		scenario, window, direct.elapsed.Round(time.Millisecond), cached.elapsed.Round(time.Millisecond), ratio,
		float64(direct.heap)/(1<<20), float64(cached.heap)/(1<<20), verdict)
}

// directSample measures a fresh direct store, the cost every restart, push or
// price change pays on main.
func (f *usagePerfFixture) directSample(t *testing.T, from string, concurrent int) (usagePerfSample, [3][]byte) {
	t.Helper()
	s, closeStore := f.directServer(t)
	defer closeStore()
	return measurePage(t, s, from, concurrent)
}

func requireSamePage(t *testing.T, scenario string, want, got [3][]byte) {
	t.Helper()
	for i := range want {
		require.JSONEq(t, string(want[i]), string(got[i]), "%s response %d", scenario, i)
	}
}

func (f *usagePerfFixture) measureSessions(t *testing.T, repeats int) {
	for _, window := range []struct{ name, from string }{
		{"all-history", "2026-05-01"}, {"30-days", "2026-07-12"}, {"7-days", "2026-08-04"},
	} {
		if only := os.Getenv("AGENTSVIEW_USAGE_PG_PERF_WINDOW"); only != "" && only != window.name {
			continue
		}
		var cold, restart, repeat, push, price, account, directs []usagePerfSample
		var coldWarm, pushWarm, priceWarm, accountWarm []usagePerfSample
		for range repeats {
			d, want := f.directSample(t, window.from, 1)
			directs = append(directs, d)

			dir := t.TempDir()
			s, closeStore := f.cachedServer(t, dir)
			sample, got := measurePage(t, s, window.from, 1)
			cold = append(cold, sample)
			requireSamePage(t, "cold", want, got)
			coldWarm = append(coldWarm, waitWarm(t, s))
			sample, got = measurePage(t, s, window.from, 1)
			repeat = append(repeat, sample)
			requireSamePage(t, "repeat", want, got)
			closeStore()

			s, closeStore = f.cachedServer(t, dir)
			sample, got = measurePage(t, s, window.from, 1)
			restart = append(restart, sample)
			requireSamePage(t, "restart", want, got)
			waitWarm(t, s)

			f.smallPush(t)
			sample, got = measurePage(t, s, window.from, 1)
			push = append(push, sample)
			_, want = f.directSample(t, window.from, 1)
			requireSamePage(t, "push", want, got)
			pushWarm = append(pushWarm, waitWarm(t, s))

			f.pricePush(t)
			sample, got = measurePage(t, s, window.from, 1)
			price = append(price, sample)
			_, want = f.directSample(t, window.from, 1)
			requireSamePage(t, "price", want, got)
			priceWarm = append(priceWarm, waitWarm(t, s))

			f.accountPush(t)
			sample, got = measurePage(t, s, window.from, 1)
			account = append(account, sample)
			_, want = f.directSample(t, window.from, 1)
			requireSamePage(t, "account", want, got)
			accountWarm = append(accountWarm, waitWarm(t, s))
			sample, got = measurePage(t, s, window.from, 1)
			requireSamePage(t, "account warmed", want, got)
			closeStore()
		}
		direct := medianSample(directs)
		logUsagePerf(t, "empty cache", window.name, direct, medianSample(cold))
		logUsageWarm(t, "empty cache", window.name, medianSample(coldWarm))
		logUsagePerf(t, "after restart", window.name, direct, medianSample(restart))
		logUsagePerf(t, "repeat visit", window.name, direct, medianSample(repeat))
		logUsagePerf(t, "after small push", window.name, direct, medianSample(push))
		logUsageWarm(t, "after small push", window.name, medianSample(pushWarm))
		logUsagePerf(t, "after price change", window.name, direct, medianSample(price))
		logUsageWarm(t, "after price change", window.name, medianSample(priceWarm))
		logUsagePerf(t, "after account push", window.name, direct, medianSample(account))
		logUsageWarm(t, "after account push", window.name, medianSample(accountWarm))
	}
}

// measureAccounts reproduces the account-heavy archive: few sessions, many
// account rows, four concurrent page loads after each kind of push.
func (f *usagePerfFixture) measureAccounts(t *testing.T, repeats int) {
	const concurrent = 4
	from := "2026-05-01"
	dir := t.TempDir()
	s, closeStore := f.cachedServer(t, dir)
	defer closeStore()
	_, _ = measurePage(t, s, from, 1)
	waitWarm(t, s)
	for _, push := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"after price change", f.pricePush},
		{"after account push", f.accountPush},
		{"after small push", f.smallPush},
	} {
		var directs, cached, warmed, warms []usagePerfSample
		for range repeats {
			push.run(t)
			sample, got := measurePage(t, s, from, concurrent)
			cached = append(cached, sample)
			d, want := f.directSample(t, from, concurrent)
			directs = append(directs, d)
			requireSamePage(t, push.name, want, got)
			warms = append(warms, waitWarm(t, s))
			sample, got = measurePage(t, s, from, concurrent)
			warmed = append(warmed, sample)
			requireSamePage(t, push.name+" warmed", want, got)
		}
		logUsagePerf(t, "accounts "+push.name, "all-history", medianSample(directs), medianSample(cached))
		logUsageWarm(t, "accounts "+push.name, "all-history", medianSample(warms))
		logUsagePerf(t, "accounts warmed "+push.name, "all-history", medianSample(directs), medianSample(warmed))
	}
}
