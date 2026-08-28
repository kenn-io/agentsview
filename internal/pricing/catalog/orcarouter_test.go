package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestParseOrcaRouterPricing(t *testing.T) {
	data := []byte(`{
		"data": [
			{
				"id": "orcarouter/fusion",
				"pricing": {
					"prompt": "0.000010",
					"completion": "0.000050",
					"prompt_per_million": "10.000000",
					"completion_per_million": "50.000000"
				}
			},
			{
				"id": "anthropic/claude-fable-5",
				"pricing": {"prompt": "0.000005", "completion": "0.000025"}
			},
			{
				"id": "orcarouter/free",
				"pricing": {"prompt": "0", "completion": "0"}
			},
			{
				"id": "orcarouter/unpriced",
				"pricing": {"prompt": "", "completion": ""}
			}
		]
	}`)

	prices, err := ParseOpenRouterPricing(data)
	require.NoError(t, err)

	assert.Equal(t, []ModelPricing{
		{
			ModelPattern:  "orcarouter/fusion",
			InputPerMTok:  money.Money{Microdollars: 10_000_000},
			OutputPerMTok: money.Money{Microdollars: 50_000_000},
		},
		{
			ModelPattern:  "anthropic/claude-fable-5",
			InputPerMTok:  money.Money{Microdollars: 5_000_000},
			OutputPerMTok: money.Money{Microdollars: 25_000_000},
		},
		{ModelPattern: "orcarouter/free"},
	}, prices, "priced entries kept, unpriced entries skipped; "+
		"prompt_per_million fields are ignored")
}

func TestFetchOrcaRouterPricing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		assert.Equal(t, "GET", r.Method)
		_, _ = w.Write([]byte(`{"data": [{
			"id": "orcarouter/fusion",
			"pricing": {"prompt": "0.000010", "completion": "0.000050"}
		}]}`))
	}))
	t.Cleanup(server.Close)

	// FetchOrcaRouterPricingContext hits the live endpoint; exercise the
	// shared fetch path (which it delegates to) against the fixture.
	prices, err := fetchPricingCatalog(
		context.Background(), server.Client(), server.URL,
	)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	assert.Equal(t, "orcarouter/fusion", prices[0].ModelPattern)
}

func TestFetchOrcaRouterPricingRejectsErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	_, err := fetchPricingCatalog(
		context.Background(), server.Client(), server.URL,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 503")
}
