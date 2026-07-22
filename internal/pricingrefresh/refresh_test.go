package pricingrefresh

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
)

const refreshAttemptMetaKeyForTest = "_litellm_last_attempt"

type fetchRecorder struct {
	calls int
	rows  []pricing.ModelPricing
	err   error
}

func (f *fetchRecorder) fetch() ([]pricing.ModelPricing, error) {
	f.calls++
	return f.rows, f.err
}

func TestEnsureSeedsFallbackAndFetchedModel(t *testing.T) {
	database := testDB(t)
	fetcher := &fetchRecorder{rows: []pricing.ModelPricing{{
		ModelPattern:  "new-model",
		InputPerMTok:  money.MustParseDollars("2"),
		OutputPerMTok: money.MustParseDollars("8"),
	}}}

	refreshed, err := Ensure(
		database, false, fetcher.fetch, pricingTestNow(),
	)
	require.NoError(t, err)
	assert.True(t, refreshed)
	assert.Equal(t, 1, fetcher.calls)

	fallback, err := database.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, fallback)
	fetched, err := database.GetModelPricing("new-model")
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, money.MustParseDollars("8"), fetched.OutputPerMTok)
}

func TestRefreshIfStaleFreshAttemptSkipsFetch(t *testing.T) {
	database := testDB(t)
	now := pricingTestNow()
	previous := seedPricingAttempt(t, database, now, 10*time.Minute)
	fetcher := &fetchRecorder{}

	refreshed, err := RefreshIfStale(
		database, fetcher.fetch, time.Hour, now,
	)

	require.NoError(t, err)
	assert.False(t, refreshed)
	assert.Zero(t, fetcher.calls)
	assertPricingAttemptMeta(t, database, previous)
}

func TestRefreshIfStaleStaleTriggersFetch(t *testing.T) {
	database := testDB(t)
	now := pricingTestNow()
	seedPricingAttempt(t, database, now, 2*time.Hour)
	fetcher := &fetchRecorder{rows: []pricing.ModelPricing{{
		ModelPattern:  "new-model",
		InputPerMTok:  money.MustParseDollars("1.25"),
		OutputPerMTok: money.MustParseDollars("10"),
	}}}

	refreshed, err := RefreshIfStale(
		database, fetcher.fetch, time.Hour, now,
	)

	require.NoError(t, err)
	assert.True(t, refreshed)
	price, err := database.GetModelPricing("new-model")
	require.NoError(t, err)
	require.NotNil(t, price)
	assert.Equal(t, money.MustParseDollars("10"), price.OutputPerMTok)
	assertPricingAttemptMeta(t, database, now.Format(time.RFC3339))
}

func TestRefreshIfStaleNeverAttemptedTriggersFetch(t *testing.T) {
	database := testDB(t)
	fetcher := &fetchRecorder{}

	refreshed, err := RefreshIfStale(
		database, fetcher.fetch, time.Hour, pricingTestNow(),
	)

	require.NoError(t, err)
	assert.True(t, refreshed)
	assert.Equal(t, 1, fetcher.calls)
}

func TestRefreshIfStaleFetchFailureRecordsAttempt(t *testing.T) {
	database := testDB(t)
	now := pricingTestNow()
	wantErr := errors.New("network down")
	fetcher := &fetchRecorder{err: wantErr}

	refreshed, err := RefreshIfStale(
		database, fetcher.fetch, time.Hour, now,
	)

	assert.ErrorIs(t, err, wantErr)
	assert.False(t, refreshed)
	assertPricingAttemptMeta(t, database, now.Format(time.RFC3339))

	second := &fetchRecorder{}
	_, err = RefreshIfStale(
		database, second.fetch, time.Hour, now.Add(time.Minute),
	)
	require.NoError(t, err)
	assert.Zero(t, second.calls)
}

func TestEnsureFetchFailurePreservesFallback(t *testing.T) {
	database := testDB(t)
	wantErr := errors.New("network down")
	fetcher := &fetchRecorder{err: wantErr}

	refreshed, err := Ensure(
		database, false, fetcher.fetch, pricingTestNow(),
	)

	assert.ErrorIs(t, err, wantErr)
	assert.False(t, refreshed)
	fallback, priceErr := database.GetModelPricing("gpt-5.5")
	require.NoError(t, priceErr)
	require.NotNil(t, fallback)
}

func TestEnsureSkipsFetchWithinCooldown(t *testing.T) {
	database := testDB(t)
	now := pricingTestNow()
	seedPricingAttempt(t, database, now, 10*time.Minute)
	fetcher := &fetchRecorder{rows: []pricing.ModelPricing{{
		ModelPattern:  "network-only-model",
		InputPerMTok:  money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("1"),
	}}}

	refreshed, err := Ensure(database, false, fetcher.fetch, now)

	require.NoError(t, err)
	assert.False(t, refreshed)
	assert.Zero(t, fetcher.calls)
	fallback, err := database.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, fallback)
	networkOnly, err := database.GetModelPricing("network-only-model")
	require.NoError(t, err)
	assert.Nil(t, networkOnly)
}

func TestEnsureOfflineSeedsFallbackWithoutFetch(t *testing.T) {
	database := testDB(t)
	fetch := func() ([]pricing.ModelPricing, error) {
		t.Fatal("offline ensure must not fetch")
		return nil, nil
	}

	refreshed, err := Ensure(database, true, fetch, pricingTestNow())

	require.NoError(t, err)
	assert.False(t, refreshed)
	fallback, err := database.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, fallback)
}

func TestEnsureCurrentCancellationAllowsImmediateRetry(t *testing.T) {
	database := testDB(t)
	now := pricingTestNow()
	previous := seedPricingAttempt(t, database, now, 2*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	err := ensureCurrent(ctx, database, func(
		context.Context,
	) ([]pricing.ModelPricing, error) {
		cancel()
		return nil, ctx.Err()
	}, now)

	assert.ErrorIs(t, err, context.Canceled)
	assertPricingAttemptMeta(t, database, previous)
	retryCalls := 0
	err = ensureCurrent(context.Background(), database, func(
		context.Context,
	) ([]pricing.ModelPricing, error) {
		retryCalls++
		return nil, nil
	}, now.Add(time.Minute))

	require.NoError(t, err)
	assert.Equal(t, 1, retryCalls)
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.OpenTestDBAt(
		t, filepath.Join(t.TempDir(), "sessions.db"),
	)
}

func pricingTestNow() time.Time {
	return time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
}

func seedPricingAttempt(
	t *testing.T,
	database *db.DB,
	now time.Time,
	age time.Duration,
) string {
	t.Helper()
	timestamp := now.Add(-age).Format(time.RFC3339)
	require.NoError(t, database.SetPricingMeta(
		refreshAttemptMetaKeyForTest, timestamp,
	))
	return timestamp
}

func assertPricingAttemptMeta(
	t *testing.T,
	database *db.DB,
	want string,
) {
	t.Helper()
	got, err := database.GetPricingMeta(refreshAttemptMetaKeyForTest)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
