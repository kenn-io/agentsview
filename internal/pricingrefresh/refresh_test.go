package pricingrefresh

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

func (f *fetchRecorder) fetch() (pricing.Catalog, error) {
	f.calls++
	return pricing.Catalog{LiteLLM: f.rows}, f.err
}

func TestEnsureSeedsFallbackAndFetchedModel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	fetcher := &fetchRecorder{rows: []pricing.ModelPricing{{
		ModelPattern:  "new-model",
		InputPerMTok:  money.MustParseDollars("2"),
		OutputPerMTok: money.MustParseDollars("8"),
	}}}

	refreshed, err := Ensure(
		database, false, fetcher.fetch, pricingTestNow(),
	)
	require.NoError(err)
	assert.True(refreshed)
	assert.Equal(1, fetcher.calls)

	fallback, err := database.GetModelPricing("gpt-5.5")
	require.NoError(err)
	require.NotNil(fallback)
	fetched, err := database.GetModelPricing("new-model")
	require.NoError(err)
	require.NotNil(fetched)
	assert.Equal(money.MustParseDollars("8"), fetched.OutputPerMTok)
}

func TestSeedFallbackReseedsBandsWhenStorageVersionIsMissing(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	fallback := pricing.FallbackPricing()
	var gpt pricing.ModelPricing
	for _, model := range fallback {
		if model.ModelPattern == "gpt-5.5" {
			gpt = model
			break
		}
	}
	require.NotEmpty(gpt.Bands)
	require.NoError(database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         gpt.ModelPattern,
		InputPerMTok:         gpt.InputPerMTok,
		OutputPerMTok:        gpt.OutputPerMTok,
		CacheCreationPerMTok: gpt.CacheCreationPerMTok,
		CacheReadPerMTok:     gpt.CacheReadPerMTok,
	}}))
	require.NoError(database.SetPricingMeta(
		fallbackVersionMetaKey,
		pricing.FallbackVersion,
	))

	require.NoError(SeedFallback(database))
	got, err := database.GetModelPricing("gpt-5.5")
	require.NoError(err)
	require.NotNil(got)

	assert.NotEmpty(t, got.Bands)
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
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	now := pricingTestNow()
	seedPricingAttempt(t, database, now, 2*time.Hour)
	fetcher := &fetchRecorder{rows: []pricing.ModelPricing{{
		ModelPattern:  "new-model",
		InputPerMTok:  money.MustParseDollars("1.25"),
		OutputPerMTok: money.MustParseDollars("10"),
		Bands: []pricing.PricingBand{{
			AboveInputTokens:     200_000,
			InputPerMTok:         money.MustParseDollars("2.50"),
			OutputPerMTok:        money.MustParseDollars("15"),
			CacheCreationPerMTok: money.MustParseDollars("3.125"),
			CacheReadPerMTok:     money.MustParseDollars("0.25"),
		}},
	}}}

	refreshed, err := RefreshIfStale(
		database, fetcher.fetch, time.Hour, now,
	)

	require.NoError(err)
	assert.True(refreshed)
	price, err := database.GetModelPricing("new-model")
	require.NoError(err)
	require.NotNil(price)
	assert.Equal(money.MustParseDollars("10"), price.OutputPerMTok)
	require.Len(price.Bands, 1)
	assert.Equal(200_000, price.Bands[0].AboveInputTokens)
	assert.Equal(money.MustParseDollars("15"), price.Bands[0].OutputPerMTok)
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
	assert := assert.New(t)

	database := testDB(t)
	now := pricingTestNow()
	wantErr := errors.New("network down")
	fetcher := &fetchRecorder{err: wantErr}

	refreshed, err := RefreshIfStale(
		database, fetcher.fetch, time.Hour, now,
	)

	assert.ErrorIs(err, wantErr)
	assert.False(refreshed)
	assertPricingAttemptMeta(t, database, now.Format(time.RFC3339))

	second := &fetchRecorder{}
	_, err = RefreshIfStale(
		database, second.fetch, time.Hour, now.Add(time.Minute),
	)
	require.NoError(t, err)
	assert.Zero(second.calls)
}

func TestRefreshIfStaleStoresDegradedCatalog(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	wantErr := errors.New("openrouter down")
	fetcher := &fetchRecorder{
		rows: []pricing.ModelPricing{{
			ModelPattern:  "degraded-model",
			InputPerMTok:  money.MustParseDollars("1"),
			OutputPerMTok: money.MustParseDollars("2"),
		}},
		err: wantErr,
	}

	refreshed, err := RefreshIfStale(
		database, fetcher.fetch, time.Hour, pricingTestNow(),
	)

	assert.ErrorIs(err, wantErr,
		"the degradation is reported alongside the refresh")
	assert.True(refreshed)
	stored, priceErr := database.GetModelPricing("degraded-model")
	require.NoError(priceErr)
	require.NotNil(stored, "LiteLLM rows stored despite the error")
	assert.Equal(money.MustParseDollars("1"), stored.InputPerMTok)
}

func TestRefreshIfStaleStoresGenAIDocumentWhenLiteLLMFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	raw := []byte(`[
  {
    "id":"test-provider",
    "model_match":{"starts_with":"test-model"},
    "models":[{
      "id":"test-model",
      "match":{"equals":"test-model"},
      "prices":{"input_mtok":1}
    }]
  }
]`)
	prices, err := pricing.ParseGenAIPrices(raw)
	require.NoError(err)
	document, err := pricing.NewGenAIDocument(prices, "upstream-main")
	require.NoError(err)
	wantErr := errors.New("LiteLLM unavailable")

	refreshed, err := RefreshIfStale(database, func() (pricing.Catalog, error) {
		return pricing.Catalog{GenAI: &document}, wantErr
	}, time.Hour, pricingTestNow())

	assert.ErrorIs(err, wantErr)
	assert.True(refreshed)
	stored, readErr := database.GetGenAIPricing(t.Context())
	require.NoError(readErr)
	require.NotNil(stored)
	assert.Equal(document.Version, stored.Version)
	assert.Equal("upstream-main", stored.SourceRef)
	assert.Equal(db.GenAIPricingSourceFetched, stored.Source)
	assert.Equal(raw, stored.Data)
}

func TestEnsureFetchFailurePreservesFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	wantErr := errors.New("network down")
	fetcher := &fetchRecorder{err: wantErr}

	refreshed, err := Ensure(
		database, false, fetcher.fetch, pricingTestNow(),
	)

	assert.ErrorIs(err, wantErr)
	assert.False(refreshed)
	fallback, priceErr := database.GetModelPricing("gpt-5.5")
	require.NoError(priceErr)
	require.NotNil(fallback)
}

func TestEnsureSkipsFetchWithinCooldown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	now := pricingTestNow()
	seedPricingAttempt(t, database, now, 10*time.Minute)
	fetcher := &fetchRecorder{rows: []pricing.ModelPricing{{
		ModelPattern:  "network-only-model",
		InputPerMTok:  money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("1"),
	}}}

	refreshed, err := Ensure(database, false, fetcher.fetch, now)

	require.NoError(err)
	assert.False(refreshed)
	assert.Zero(fetcher.calls)
	fallback, err := database.GetModelPricing("gpt-5.5")
	require.NoError(err)
	require.NotNil(fallback)
	networkOnly, err := database.GetModelPricing("network-only-model")
	require.NoError(err)
	assert.Nil(networkOnly)
}

func TestEnsureOfflineSeedsFallbackWithoutFetch(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	fetch := func() (pricing.Catalog, error) {
		t.Fatal("offline ensure must not fetch")
		return pricing.Catalog{}, nil
	}

	refreshed, err := Ensure(database, true, fetch, pricingTestNow())

	require.NoError(err)
	assert.False(t, refreshed)
	fallback, err := database.GetModelPricing("gpt-5.5")
	require.NoError(err)
	require.NotNil(fallback)
}

func TestStoreCatalogRetiresShadowedOpenRouterRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	// A stale row from an earlier LiteLLM catalog that the live catalog
	// no longer lists.
	require.NoError(database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "acme/Stale-Model",
		InputPerMTok: money.MustParseDollars("1"),
	}}))
	openrouter := []pricing.ModelPricing{
		{
			ModelPattern: "minimax/minimax-m3",
			InputPerMTok: money.MustParseDollars("9"),
		},
		{
			ModelPattern: "acme/stale-model",
			InputPerMTok: money.MustParseDollars("8"),
		},
	}
	require.NoError(storeCatalog(database, pricing.Catalog{
		OpenRouter: openrouter,
	}))
	meta, err := database.GetPricingMeta(pricing.OpenRouterModelsMetaKey)
	require.NoError(err)
	assert.Equal(`["minimax/minimax-m3"]`, meta,
		"stored row from another source shadows OpenRouter's spelling")
	shadowed, err := database.GetModelPricing("acme/stale-model")
	require.NoError(err)
	assert.Nil(shadowed)

	// LiteLLM now lists the model under its own spelling.
	require.NoError(storeCatalog(database, pricing.Catalog{
		LiteLLM: []pricing.ModelPricing{{
			ModelPattern: "minimax/MiniMax-M3",
			InputPerMTok: money.MustParseDollars("2"),
		}},
		OpenRouter: openrouter,
	}))

	stale, err := database.GetModelPricing("minimax/minimax-m3")
	require.NoError(err)
	assert.Nil(stale, "shadowed OpenRouter row retired")
	current, err := database.GetModelPricing("minimax/MiniMax-M3")
	require.NoError(err)
	require.NotNil(current)
	assert.Equal(money.MustParseDollars("2"), current.InputPerMTok)
	meta, err = database.GetPricingMeta(pricing.OpenRouterModelsMetaKey)
	require.NoError(err)
	assert.Equal(`[]`, meta)
}

func TestStoreFallbackReconcilesOpenRouterOwnership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	fallback := pricing.FallbackPricing()
	require.NotEmpty(fallback)
	exact := fallback[0].ModelPattern
	var spelled string
	for _, p := range fallback {
		if upper := strings.ToUpper(p.ModelPattern); upper != p.ModelPattern {
			spelled = upper
			break
		}
	}
	require.NotEmpty(spelled, "need a fallback pattern with lowercase")
	require.NoError(storeCatalog(database, pricing.Catalog{
		OpenRouter: []pricing.ModelPricing{
			{ModelPattern: exact, InputPerMTok: money.MustParseDollars("99")},
			{ModelPattern: spelled, InputPerMTok: money.MustParseDollars("98")},
			{ModelPattern: "acme/only-openrouter"},
		},
	}))

	require.NoError(SeedFallback(database))

	shadowed, err := database.GetModelPricing(spelled)
	require.NoError(err)
	assert.Nil(shadowed, "OpenRouter spelling of a seeded model retired")
	seeded, err := database.GetModelPricing(exact)
	require.NoError(err)
	require.NotNil(seeded)
	assert.Equal(fallback[0].InputPerMTok, seeded.InputPerMTok,
		"embedded rate wins on the exact pattern")
	meta, err := database.GetPricingMeta(pricing.OpenRouterModelsMetaKey)
	require.NoError(err)
	assert.Equal(`["acme/only-openrouter"]`, meta,
		"ownership of seeded and retired patterns transferred")
}

func TestSeedFallbackWithoutSentinelWritesNone(t *testing.T) {
	database := testDB(t)
	require.NoError(t, SeedFallback(database))
	meta, err := database.GetPricingMeta(pricing.OpenRouterModelsMetaKey)
	require.NoError(t, err)
	assert.Empty(t, meta)
}

func TestEnsureCurrentCancellationAllowsImmediateRetry(t *testing.T) {
	database := testDB(t)
	now := pricingTestNow()
	previous := seedPricingAttempt(t, database, now, 2*time.Hour)
	ctx, cancel := context.WithCancel(t.Context())

	err := ensureCurrent(ctx, database, func(
		context.Context,
	) (pricing.Catalog, error) {
		cancel()
		return pricing.Catalog{}, ctx.Err()
	}, now)

	assert.ErrorIs(t, err, context.Canceled)
	assertPricingAttemptMeta(t, database, previous)
	retryCalls := 0
	err = ensureCurrent(t.Context(), database, func(
		context.Context,
	) (pricing.Catalog, error) {
		retryCalls++
		return pricing.Catalog{}, nil
	}, now.Add(time.Minute))

	require.NoError(t, err)
	assert.Equal(t, 1, retryCalls)
}

func TestRefreshCurrentFetchesDespiteRecentAttempt(t *testing.T) {
	database := testDB(t)
	now := pricingTestNow()
	seedPricingAttempt(t, database, now, 10*time.Minute)

	err := refreshCurrent(t.Context(), database, func(
		context.Context,
	) (pricing.Catalog, error) {
		return pricing.Catalog{LiteLLM: []pricing.ModelPricing{{
			ModelPattern: "scheduled-model",
		}}}, nil
	}, now)

	require.NoError(t, err)
	price, err := database.GetModelPricing("scheduled-model")
	require.NoError(t, err)
	require.NotNil(t, price)
	assertPricingAttemptMeta(t, database, now.Format(time.RFC3339))
}

func TestRefreshCurrentSkipsWhileEnsureCurrentInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		database := testDB(t)
		now := pricingTestNow()
		ensureFetchStarted := make(chan struct{})
		releaseEnsureFetch := make(chan struct{}, 1)
		ensureDone := make(chan error, 1)

		go func() {
			ensureDone <- ensureCurrent(t.Context(), database, func(
				context.Context,
			) (pricing.Catalog, error) {
				close(ensureFetchStarted)
				<-releaseEnsureFetch
				return pricing.Catalog{LiteLLM: []pricing.ModelPricing{{
					ModelPattern: "ensure-model",
				}}}, nil
			}, now)
		}()
		defer func() {
			releaseEnsureFetch <- struct{}{}
		}()

		synctest.Wait()
		select {
		case <-ensureFetchStarted:
		default:
			require.FailNow("ensureCurrent did not start its fetch")
		}

		var refreshFetchCalls atomic.Int32
		refreshDone := make(chan error, 1)
		go func() {
			refreshDone <- refreshCurrent(
				t.Context(), database, func(
					context.Context,
				) (pricing.Catalog, error) {
					refreshFetchCalls.Add(1)
					return pricing.Catalog{LiteLLM: []pricing.ModelPricing{{
						ModelPattern: "scheduled-model",
					}}}, nil
				}, now.Add(time.Minute),
			)
		}()

		synctest.Wait()
		var refreshErr error
		select {
		case refreshErr = <-refreshDone:
		default:
			require.FailNow("refreshCurrent did not finish while ensureCurrent was in flight")
		}
		require.NoError(refreshErr)
		assert.Zero(refreshFetchCalls.Load())
		scheduledPrice, err := database.GetModelPricing("scheduled-model")
		require.NoError(err)
		assert.Nil(scheduledPrice)

		releaseEnsureFetch <- struct{}{}
		synctest.Wait()
		var ensureErr error
		select {
		case ensureErr = <-ensureDone:
		default:
			require.FailNow("ensureCurrent did not finish after its fetch was released")
		}
		require.NoError(ensureErr)
		ensuredPrice, err := database.GetModelPricing("ensure-model")
		require.NoError(err)
		require.NotNil(ensuredPrice)
	})
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
