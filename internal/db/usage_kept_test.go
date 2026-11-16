//go:build fts5

package db

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestDailyUsageKept(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	first, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 10, first.Totals.InputTokens)
	require.NotEmpty(t, first.Daily)
	first.Daily[0].InputTokens = 999
	other := filter
	other.From = "2026-08-09"
	_, err = database.GetDailyUsage(t.Context(), other)
	require.NoError(t, err)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 10, got.Totals.InputTokens)
	assert.Equal(t, 10, got.Daily[0].InputTokens)
	assert.EqualValues(t, 2, prepares.Load())

	appendLiveWriteMessage(t, database, "reported", "2026-08-10T10:00:00Z", 1, 7)
	got, err = database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 17, got.Totals.InputTokens)
	require.NoError(t, database.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{OccurredAt: "2026-08-10T09:00:00Z", Model: "cursor-model", InputTokens: 3, DedupKey: "cursor"}}))
	got, err = database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 20, got.Totals.InputTokens)
	assert.EqualValues(t, 4, prepares.Load())
}

func TestDailyUsageKeptPricing(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 1000000)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	readCost := func() int64 {
		got, err := database.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		return got.Totals.TotalCost.Microdollars
	}
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("1")}}))
	assert.EqualValues(t, 1000000, readCost())
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("2")}}))
	assert.EqualValues(t, 2000000, readCost())
	database.SetCustomPricing(map[string]config.CustomModelRate{"model-a": {InputMicrodollarsPerMTok: 3000000}})
	assert.EqualValues(t, 3000000, readCost())
	database.SetEffectivePricing(map[string]export.ModelRates{"model-a": {InputPerMTok: money.MustParseDollars("4")}})
	assert.EqualValues(t, 4000000, readCost())
	before := prepares.Load()
	database.SetEmptyCatalogPricing(nil)
	assert.EqualValues(t, 4000000, readCost())
	assert.Equal(t, before+1, prepares.Load())
}

func TestDailyUsageKeptWriteDuringRead(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	var prepares, reads atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	started, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	database.usageCache.usageWindowObserver.afterRead = func(bool) {
		if reads.Add(1) == 1 {
			close(started)
			<-resume
		}
	}
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	done := make(chan error, 1)
	go func() {
		_, err := database.GetDailyUsage(t.Context(), filter)
		done <- err
	}()
	<-started
	appendLiveWriteMessage(t, database, "reported", "2026-08-10T10:00:00Z", 1, 7)
	newer, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 17, newer.Totals.InputTokens)
	release()
	require.NoError(t, <-done)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 17, got.Totals.InputTokens)
	assert.EqualValues(t, 2, prepares.Load(), "the older read must not evict the newer result")
}

func TestDailyUsageKeptSharedWindowAfterWrite(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	ctx := WithUsageWindowMemo(t.Context())
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
	_, err := database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	filter.Breakdowns, filter.PairwiseSides, filter.Model = false, false, "model-a"
	_, err = database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.EqualValues(t, 1, prepares.Load())
	appendLiveWriteMessage(t, database, "reported", "2026-08-10T10:00:00Z", 1, 7)
	shared, err := database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 17, shared.Totals.InputTokens)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 17, got.Totals.InputTokens)
	assert.EqualValues(t, 2, prepares.Load())
}

func TestDailyUsageKeptMovingClockBypass(t *testing.T) {
	database := testDB(t)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	for range 2 {
		_, err := database.GetDailyUsage(t.Context(), UsageFilter{Timezone: "UTC", Termination: "active"})
		require.NoError(t, err)
	}
	assert.EqualValues(t, 2, prepares.Load())
}

func TestDailyUsageKeptCountsAfterOmission(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", SkipSessionCounts: true}
	first, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, UsageSessionCounts{}, first.SessionCounts)
	filter.SkipSessionCounts = false
	second, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 1, second.SessionCounts.Total)
	filter.SkipSessionCounts = true
	third, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, UsageSessionCounts{}, third.SessionCounts)
	assert.EqualValues(t, 1, prepares.Load())
}

func TestDailyUsageKeptPairwiseAfterSummary(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "one", "project,one", "2026-08-10T09:00:00Z", 0, 10)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", Breakdowns: true}
	_, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	filter.PairwiseSides, filter.SkipSessionCounts = true, true
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NotNil(t, got.Pairwise)
	assert.Equal(t, UsageSessionCounts{}, got.SessionCounts)
	model, ok := got.Pairwise.Side("model", []string{"model-a"})
	require.True(t, ok)
	assert.Equal(t, 10, model.Totals.InputTokens)
	assert.Equal(t, 1, model.SessionCounts.Total)
	project, ok := got.Pairwise.Side("project", []string{"project,one"})
	require.True(t, ok)
	assert.Equal(t, 10, project.Totals.InputTokens)
	assert.Equal(t, 1, project.SessionCounts.Total)
	assert.EqualValues(t, 1, prepares.Load())
}

func TestDailyUsageKeptSummaryAfterPairwise(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "one", "project", "2026-08-10T09:00:00Z", 0, 10)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
	ranking, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NotNil(t, ranking.Pairwise)
	filter.PairwiseSides = false
	_, err = database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.EqualValues(t, 1, prepares.Load())
}

func TestDailyUsageKeptReopen(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, 10, got.Totals.InputTokens)
	replacement := testDB(t)
	seedLiveWriteSession(t, replacement, "other", "project", "2026-08-10T09:00:00Z", 0, 20)
	require.NoError(t, replacement.Close())
	require.NoError(t, database.CloseConnections(t.Context()))
	require.NoError(t, os.Rename(replacement.Path(), database.Path()))
	require.NoError(t, database.Reopen())
	got, err = database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 20, got.Totals.InputTokens)
}

func TestDailyUsageKeptProjectIdentityChange(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	_, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NoError(t, database.UpsertProjectIdentityObservation(t.Context(), export.ProjectIdentityObservation{
		SessionID: "reported", Project: "project", Machine: "local", RootPath: "/workspace/project", ObservedAt: time.Now().UTC(),
	}))
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 10, got.Totals.InputTokens)
	assert.EqualValues(t, 2, prepares.Load())
}

// The read looks up its memo after the version query, then Flights.Do calls
// Done after registering the waiter. Signal at that point.
type flightWaitContext struct {
	context.Context
	armed   atomic.Bool
	once    sync.Once
	waiting chan struct{}
}

func (ctx *flightWaitContext) Value(key any) any {
	if _, ok := key.(usageWindowMemoKey); ok {
		ctx.armed.Store(true)
	}
	return ctx.Context.Value(key)
}

func (ctx *flightWaitContext) Done() <-chan struct{} {
	if ctx.armed.Load() {
		ctx.once.Do(func() { close(ctx.waiting) })
	}
	return ctx.Context.Done()
}

func (ctx *flightWaitContext) wait(t *testing.T) {
	t.Helper()
	select {
	case <-ctx.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not join its flight")
	}
}

func TestDailyUsageKeptConcurrentMissesShareBuild(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "one", "project", "2026-08-10T09:00:00Z", 0, 10)
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", Breakdowns: true}
	_, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	appendLiveWriteMessage(t, database, "one", "2026-08-10T10:00:00Z", 1, 7)
	var prepares atomic.Int32
	started, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	database.SetUsageWindowPrepareForTest(func(context.Context) {
		if prepares.Add(1) == 1 {
			close(started)
			<-resume
		}
	})
	type response struct {
		result DailyUsageResult
		err    error
	}
	first, second := make(chan response, 1), make(chan response, 1)
	ctx1 := WithUsageWindowMemo(t.Context())
	ctx2 := &flightWaitContext{Context: WithUsageWindowMemo(t.Context()), waiting: make(chan struct{})}
	go func() { result, err := database.GetDailyUsage(ctx1, filter); first <- response{result, err} }()
	<-started
	ranking := filter
	ranking.PairwiseSides, ranking.SkipSessionCounts = true, true
	go func() { result, err := database.GetDailyUsage(ctx2, ranking); second <- response{result, err} }()
	ctx2.wait(t)
	release()
	a, b := <-first, <-second
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	assert.Equal(t, 17, a.result.Totals.InputTokens)
	assert.Equal(t, a.result.Totals, b.result.Totals)
	assert.Equal(t, 1, a.result.SessionCounts.Total)
	assert.Equal(t, UsageSessionCounts{}, b.result.SessionCounts)
	require.NotNil(t, b.result.Pairwise)
	require.NotEmpty(t, a.result.Daily)
	require.NotEmpty(t, b.result.Daily)
	a.result.Daily[0].InputTokens = 999
	assert.Equal(t, 17, b.result.Daily[0].InputTokens)
	assert.EqualValues(t, 1, prepares.Load())
}

func TestDailyUsageKeptConcurrentSlotsShareWindow(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "one", "project", "2026-08-10T09:00:00Z", 0, 10)
	left := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	right := left
	right.Model = "model-b"
	_, err := database.GetDailyUsage(t.Context(), left)
	require.NoError(t, err)
	appendLiveWriteMessage(t, database, "one", "2026-08-10T10:00:00Z", 1, 7)
	var prepares atomic.Int32
	started, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	database.SetUsageWindowPrepareForTest(func(context.Context) {
		if prepares.Add(1) == 1 {
			close(started)
			<-resume
		}
	})
	type response struct {
		result DailyUsageResult
		err    error
	}
	ctx := WithUsageWindowMemo(t.Context())
	first, second := make(chan response, 1), make(chan response, 1)
	go func() { result, err := database.GetDailyUsage(ctx, left); first <- response{result, err} }()
	<-started
	rightCtx := &flightWaitContext{Context: ctx, waiting: make(chan struct{})}
	go func() { result, err := database.GetDailyUsage(rightCtx, right); second <- response{result, err} }()
	rightCtx.wait(t)
	release()
	a, b := <-first, <-second
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	assert.Equal(t, 17, a.result.Totals.InputTokens)
	assert.Equal(t, 0, b.result.Totals.InputTokens)
	assert.EqualValues(t, 1, prepares.Load())
}

func TestDailyUsageKeptJoinerSurvivesOtherCancel(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "one", "project", "2026-08-10T09:00:00Z", 0, 10)
	sideL := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	sideR := sideL
	sideR.Model = "model-a"
	var prepares atomic.Int32
	started := make(chan struct{})
	database.SetUsageWindowPrepareForTest(func(ctx context.Context) {
		if prepares.Add(1) == 1 {
			close(started)
			<-ctx.Done()
		}
	})
	deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	requestA, cancelA := context.WithCancel(WithUsageWindowMemo(deadline))
	defer cancelA()
	requestB := WithUsageWindowMemo(deadline)
	aRight, aLeft := make(chan error, 1), make(chan error, 1)
	go func() { _, err := database.GetDailyUsage(requestA, sideR); aRight <- err }()
	select {
	case <-started:
	case <-deadline.Done():
		require.FailNow(t, "prepare did not start")
	}
	leftA := &flightWaitContext{Context: requestA, waiting: make(chan struct{})}
	go func() { _, err := database.GetDailyUsage(leftA, sideL); aLeft <- err }()
	leftA.wait(t)
	type response struct {
		result DailyUsageResult
		err    error
	}
	bLeft := make(chan response, 1)
	leftB := &flightWaitContext{Context: requestB, waiting: make(chan struct{})}
	go func() { result, err := database.GetDailyUsage(leftB, sideL); bLeft <- response{result, err} }()
	leftB.wait(t)
	cancelA()
	for _, done := range []chan error{aRight, aLeft} {
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-deadline.Done():
			require.FailNow(t, "canceled read did not return")
		}
	}
	select {
	case got := <-bLeft:
		require.NoError(t, got.err)
		assert.Equal(t, 10, got.result.Totals.InputTokens)
	case <-deadline.Done():
		require.FailNow(t, "joined read did not return")
	}
	assert.EqualValues(t, 2, prepares.Load())
	result, err := database.GetDailyUsage(deadline, sideR)
	require.NoError(t, err)
	assert.Equal(t, 10, result.Totals.InputTokens)
	assert.EqualValues(t, 3, prepares.Load())
}

func TestDailyUsageKeptFailedReadThenLaterRetry(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "one", "project", "2026-08-10T09:00:00Z", 0, 10)
	var prepares atomic.Int32
	started, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	database.SetUsageWindowPrepareForTest(func(ctx context.Context) {
		if prepares.Add(1) == 1 {
			close(started)
			select {
			case <-resume:
			case <-ctx.Done():
			}
		}
	})
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	deadline, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx := WithUsageWindowMemo(deadline)
	done := make(chan error, 1)
	go func() { _, err := database.GetDailyUsage(ctx, filter); done <- err }()
	select {
	case <-started:
	case <-deadline.Done():
		require.FailNow(t, "prepare did not start")
	}
	_, err := database.getWriter().ExecContext(deadline, "ALTER TABLE model_pricing RENAME TO model_pricing_hidden")
	require.NoError(t, err)
	release()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-deadline.Done():
		require.FailNow(t, "failed read did not return")
	}
	_, err = database.getWriter().ExecContext(deadline, "ALTER TABLE model_pricing_hidden RENAME TO model_pricing")
	require.NoError(t, err)
	got, err := database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 10, got.Totals.InputTokens)
	assert.EqualValues(t, 2, prepares.Load())
}
