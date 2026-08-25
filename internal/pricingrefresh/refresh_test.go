package pricingrefresh

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func (f *fetchRecorder) fetch() (pricing.Catalog, error) {
	f.calls++
	return pricing.Catalog{LiteLLM: f.rows}, f.err
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

func TestSeedFallbackReseedsBandsWhenStorageVersionIsMissing(t *testing.T) {
	database := testDB(t)
	fallback := pricing.FallbackPricing()
	var gpt pricing.ModelPricing
	for _, model := range fallback {
		if model.ModelPattern == "gpt-5.5" {
			gpt = model
			break
		}
	}
	require.NotEmpty(t, gpt.Bands)
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         gpt.ModelPattern,
		InputPerMTok:         gpt.InputPerMTok,
		OutputPerMTok:        gpt.OutputPerMTok,
		CacheCreationPerMTok: gpt.CacheCreationPerMTok,
		CacheReadPerMTok:     gpt.CacheReadPerMTok,
	}}))
	require.NoError(t, database.SetPricingMeta(
		fallbackVersionMetaKey,
		pricing.FallbackVersion,
	))

	require.NoError(t, SeedFallback(database))
	got, err := database.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, got)

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

	require.NoError(t, err)
	assert.True(t, refreshed)
	price, err := database.GetModelPricing("new-model")
	require.NoError(t, err)
	require.NotNil(t, price)
	assert.Equal(t, money.MustParseDollars("10"), price.OutputPerMTok)
	require.Len(t, price.Bands, 1)
	assert.Equal(t, 200_000, price.Bands[0].AboveInputTokens)
	assert.Equal(t, money.MustParseDollars("15"), price.Bands[0].OutputPerMTok)
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

func TestRefreshIfStaleStoresDegradedCatalog(t *testing.T) {
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

	assert.ErrorIs(t, err, wantErr,
		"the degradation is reported alongside the refresh")
	assert.True(t, refreshed)
	stored, priceErr := database.GetModelPricing("degraded-model")
	require.NoError(t, priceErr)
	require.NotNil(t, stored, "LiteLLM rows stored despite the error")
	assert.Equal(t, money.MustParseDollars("1"), stored.InputPerMTok)
}

func TestRefreshIfStaleStoresGenAIDocumentWhenLiteLLMFails(t *testing.T) {
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
	require.NoError(t, err)
	document, err := pricing.NewGenAIDocument(prices, "upstream-main")
	require.NoError(t, err)
	wantErr := errors.New("LiteLLM unavailable")

	refreshed, err := RefreshIfStale(database, func() (pricing.Catalog, error) {
		return pricing.Catalog{GenAI: &document}, wantErr
	}, time.Hour, pricingTestNow())

	assert.ErrorIs(t, err, wantErr)
	assert.True(t, refreshed)
	stored, readErr := database.GetGenAIPricing(context.Background())
	require.NoError(t, readErr)
	require.NotNil(t, stored)
	assert.Equal(t, document.Version, stored.Version)
	assert.Equal(t, "upstream-main", stored.SourceRef)
	assert.Equal(t, db.GenAIPricingSourceFetched, stored.Source)
	assert.Equal(t, raw, stored.Data)
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
	fetch := func() (pricing.Catalog, error) {
		t.Fatal("offline ensure must not fetch")
		return pricing.Catalog{}, nil
	}

	refreshed, err := Ensure(database, true, fetch, pricingTestNow())

	require.NoError(t, err)
	assert.False(t, refreshed)
	fallback, err := database.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, fallback)
}

func TestStoreCatalogRetiresShadowedOpenRouterRows(t *testing.T) {
	database := testDB(t)
	// A stale row from an earlier LiteLLM catalog that the live catalog
	// no longer lists.
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
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
	require.NoError(t, storeCatalog(database, pricing.Catalog{
		OpenRouter: openrouter,
	}))
	meta, err := database.GetPricingMeta(pricing.OpenRouterModelsMetaKey)
	require.NoError(t, err)
	assert.Equal(t, `["minimax/minimax-m3"]`, meta,
		"stored row from another source shadows OpenRouter's spelling")
	shadowed, err := database.GetModelPricing("acme/stale-model")
	require.NoError(t, err)
	assert.Nil(t, shadowed)

	// LiteLLM now lists the model under its own spelling.
	require.NoError(t, storeCatalog(database, pricing.Catalog{
		LiteLLM: []pricing.ModelPricing{{
			ModelPattern: "minimax/MiniMax-M3",
			InputPerMTok: money.MustParseDollars("2"),
		}},
		OpenRouter: openrouter,
	}))

	stale, err := database.GetModelPricing("minimax/minimax-m3")
	require.NoError(t, err)
	assert.Nil(t, stale, "shadowed OpenRouter row retired")
	current, err := database.GetModelPricing("minimax/MiniMax-M3")
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, money.MustParseDollars("2"), current.InputPerMTok)
	meta, err = database.GetPricingMeta(pricing.OpenRouterModelsMetaKey)
	require.NoError(t, err)
	assert.Equal(t, `[]`, meta)
}

func TestStoreFallbackReconcilesOpenRouterOwnership(t *testing.T) {
	database := testDB(t)
	fallback := pricing.FallbackPricing()
	require.NotEmpty(t, fallback)
	exact := fallback[0].ModelPattern
	var spelled string
	for _, p := range fallback {
		if upper := strings.ToUpper(p.ModelPattern); upper != p.ModelPattern {
			spelled = upper
			break
		}
	}
	require.NotEmpty(t, spelled, "need a fallback pattern with lowercase")
	require.NoError(t, storeCatalog(database, pricing.Catalog{
		OpenRouter: []pricing.ModelPricing{
			{ModelPattern: exact, InputPerMTok: money.MustParseDollars("99")},
			{ModelPattern: spelled, InputPerMTok: money.MustParseDollars("98")},
			{ModelPattern: "acme/only-openrouter"},
		},
	}))

	require.NoError(t, SeedFallback(database))

	shadowed, err := database.GetModelPricing(spelled)
	require.NoError(t, err)
	assert.Nil(t, shadowed, "OpenRouter spelling of a seeded model retired")
	seeded, err := database.GetModelPricing(exact)
	require.NoError(t, err)
	require.NotNil(t, seeded)
	assert.Equal(t, fallback[0].InputPerMTok, seeded.InputPerMTok,
		"embedded rate wins on the exact pattern")
	meta, err := database.GetPricingMeta(pricing.OpenRouterModelsMetaKey)
	require.NoError(t, err)
	assert.Equal(t, `["acme/only-openrouter"]`, meta,
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
	ctx, cancel := context.WithCancel(context.Background())

	err := ensureCurrent(ctx, database, func(
		context.Context,
	) (pricing.Catalog, error) {
		cancel()
		return pricing.Catalog{}, ctx.Err()
	}, now)

	assert.ErrorIs(t, err, context.Canceled)
	assertPricingAttemptMeta(t, database, previous)
	retryCalls := 0
	err = ensureCurrent(context.Background(), database, func(
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

	err := refreshCurrent(context.Background(), database, func(
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
	database := testDB(t)
	now := pricingTestNow()
	ensureFetchStarted := make(chan struct{})
	releaseEnsureFetch := make(chan struct{}, 1)
	ensureDone := make(chan error, 1)

	go func() {
		ensureDone <- ensureCurrent(context.Background(), database, func(
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

	require.Eventually(t, func() bool {
		select {
		case <-ensureFetchStarted:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)

	var refreshFetchCalls atomic.Int32
	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- refreshCurrent(
			context.Background(), database, func(
				context.Context,
			) (pricing.Catalog, error) {
				refreshFetchCalls.Add(1)
				return pricing.Catalog{LiteLLM: []pricing.ModelPricing{{
					ModelPattern: "scheduled-model",
				}}}, nil
			}, now.Add(time.Minute),
		)
	}()

	var refreshErr error
	require.Eventually(t, func() bool {
		select {
		case refreshErr = <-refreshDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.NoError(t, refreshErr)
	assert.Zero(t, refreshFetchCalls.Load())
	scheduledPrice, err := database.GetModelPricing("scheduled-model")
	require.NoError(t, err)
	assert.Nil(t, scheduledPrice)

	releaseEnsureFetch <- struct{}{}
	var ensureErr error
	require.Eventually(t, func() bool {
		select {
		case ensureErr = <-ensureDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.NoError(t, ensureErr)
	ensuredPrice, err := database.GetModelPricing("ensure-model")
	require.NoError(t, err)
	require.NotNil(t, ensuredPrice)
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
