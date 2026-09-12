package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/poller"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

// Resync may spend up to five seconds draining SQLite connections before a
// swap, and the surrounding work can take longer on loaded Windows runners.
const pricingResyncTestTimeout = 30 * time.Second

// pricingCatalogTransport serves fixed catalogs and records fetch attempts.
type pricingCatalogTransport struct {
	requests chan *http.Request
	fail     func(*http.Request) bool
}

func (t pricingCatalogTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	t.requests <- req
	if t.fail != nil && t.fail(req) {
		return nil, errors.New("simulated pricing catalog transport failure")
	}
	var body string
	switch req.URL.String() {
	case "https://raw.githubusercontent.com/pydantic/genai-prices/main/prices/new_data/v2/data.json":
		// GenAI Prices rejects catalogs without providers.
		body = `[{
			"id": "test-provider",
			"model_match": {"starts_with": "test-model"},
			"models": [{
				"id": "test-model",
				"match": {"equals": "test-model"},
				"prices": {"input_mtok": 1}
			}]
		}]`
	case "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json":
		body = `{
			"scheduled-model": {
				"input_cost_per_token": 0.000002,
				"litellm_provider": "test"
			}
		}`
	case "https://openrouter.ai/api/v1/models":
		body = `{"data": []}`
	default:
		return nil, fmt.Errorf("unexpected pricing URL: %s", req.URL)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func withPricingCatalogTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = original
	})
}

func TestPricingRefreshStartsDespiteRecentAttemptAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		previousAttempt := time.Now().Add(-10 * time.Minute).UTC().Format(
			time.RFC3339,
		)
		require.NoError(t, database.SetPricingMeta(
			"_litellm_last_attempt", previousAttempt,
		))

		failing := true
		withPricingCatalogTransport(t, pricingCatalogTransport{
			requests: make(chan *http.Request, 6),
			fail:     func(*http.Request) bool { return failing },
		})

		ctx, cancel := context.WithCancel(t.Context())
		sched := poller.Start(ctx, pricingRefreshJob(database, nil))
		t.Cleanup(func() {
			cancel()
			sched.Wait()
		})
		synctest.Wait()
		require.Contains(t, sched.Status()[0].LastError, "simulated pricing catalog transport failure")

		failing = false
		require.NoError(t, sched.TriggerNow(pricingRefreshJobName))

		price, err := database.GetModelPricing("scheduled-model")
		require.NoError(t, err)
		require.NotNil(t, price)
		assert.Equal(t, int64(2_000_000), price.InputPerMTok.Microdollars)
	})
}

func TestPricingWritesWaitForResyncSwap(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		seed        bool
	}{
		{name: "refresh", model: "scheduled-model"},
		{name: "fallback seed", model: "gpt-5.5", seed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(database, agentsync.EngineConfig{})
			t.Cleanup(engine.Close)
			dbtest.EnsureTestDBAt(t, engine.ResyncTempPath())

			swapEntered := make(chan error, 1)
			releaseSwap := make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseSwap) })
			t.Cleanup(release)
			swapDone := make(chan error, 1)
			go func() {
				swapDone <- engine.RunExclusive(func() error {
					swapEntered <- nil
					<-releaseSwap
					if _, err := engine.SwapResyncDatabase(engine.ResyncTempPath()); err != nil {
						return err
					}
					return engine.ResetCachesAfterSwap()
				})
			}()
			awaitPricingResult(t, swapEntered)

			requests := make(chan *http.Request, 3)
			withPricingCatalogTransport(t, pricingCatalogTransport{requests: requests})
			runDone := make(chan error, 1)
			go func() {
				if tc.seed {
					seedPricing(database, engine)
					runDone <- nil
				} else {
					job := pricingRefreshJob(database, engine)
					runDone <- job.Run(t.Context())
				}
			}()
			assert.Never(t, func() bool {
				return len(requests) > 0 || len(runDone) > 0
			}, time.Second, time.Millisecond)

			release()
			awaitPricingResult(t, swapDone)
			awaitPricingResult(t, runDone)
			price, err := database.GetModelPricing(tc.model)
			require.NoError(t, err)
			require.NotNil(t, price, "pricing must be written to the replacement database")
		})
	}
}

func awaitPricingResult(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(pricingResyncTestTimeout):
		t.Fatal("pricing operation did not finish")
	}
}
