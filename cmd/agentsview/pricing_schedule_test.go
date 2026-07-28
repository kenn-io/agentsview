package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
)

type pricingCatalogTransport struct {
	requests chan *http.Request
}

func (t pricingCatalogTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	t.requests <- req
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{
			"scheduled-model": {
				"input_cost_per_token": 0.000002,
				"litellm_provider": "test"
			}
		}`)),
	}, nil
}

func TestRunPeriodicPricingRefreshFetchesAfterRecentAttempt(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	previousAttempt := time.Now().Add(-10 * time.Minute).UTC().Format(
		time.RFC3339,
	)
	require.NoError(t, database.SetPricingMeta(
		"_litellm_last_attempt", previousAttempt,
	))

	requests := make(chan *http.Request, 1)
	originalTransport := http.DefaultTransport
	http.DefaultTransport = pricingCatalogTransport{requests: requests}
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
	})

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		runPeriodicPricingRefresh(ctx, ticks, database)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		require.Eventually(t, func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		}, time.Second, time.Millisecond)
	})

	ticks <- time.Now()
	require.Eventually(t, func() bool {
		price, err := database.GetModelPricing("scheduled-model")
		return err == nil && price != nil
	}, time.Second, time.Millisecond)

	request := <-requests
	require.Equal(t,
		"https://raw.githubusercontent.com/BerriAI/litellm/main/"+
			"model_prices_and_context_window.json",
		request.URL.String(),
	)
	currentAttempt, err := database.GetPricingMeta("_litellm_last_attempt")
	require.NoError(t, err)
	require.NotEqual(t, previousAttempt, currentAttempt)
}

func TestRunPricingRefreshLoopContinuesAfterFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 2)
	done := make(chan struct{})
	var attempts atomic.Int32

	go func() {
		runPricingRefreshLoop(ctx, ticks, func(context.Context) error {
			if attempts.Add(1) == 1 {
				return errors.New("temporary pricing failure")
			}
			return nil
		})
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		require.Eventually(t, func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		}, time.Second, time.Millisecond)
	})

	ticks <- time.Time{}
	require.Eventually(t, func() bool {
		return attempts.Load() == 1
	}, time.Second, time.Millisecond)

	ticks <- time.Time{}
	require.Eventually(t, func() bool {
		return attempts.Load() == 2
	}, time.Second, time.Millisecond)
}
