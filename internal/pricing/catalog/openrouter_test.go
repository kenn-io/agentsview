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

func TestParseOpenRouterPricing(t *testing.T) {
	data := []byte(`{
		"data": [
			{
				"id": "minimax/minimax-m3",
				"architecture": {"modality": "text+image->text"},
				"pricing": {
					"prompt": "0.000005",
					"completion": "0.000025",
					"input_cache_read": "0.0000005",
					"input_cache_write": "0.00000625"
				}
			},
			{
				"id": "provider/plain",
				"pricing": {"prompt": "0.000001", "completion": "0.000002"}
			},
			{
				"id": "provider/free",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "0", "completion": "0"}
			},
			{
				"id": "openai/image-model",
				"architecture": {"modality": "text->image"},
				"pricing": {"prompt": "0.01", "completion": "0.01"}
			},
			{
				"id": "openrouter/auto",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "-1", "completion": "-1"}
			},
			{
				"id": "provider/malformed",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "1 trailing", "completion": "NaN"}
			},
			{
				"id": "provider/unpriced",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "", "completion": ""}
			}
		]
	}`)

	prices, err := ParseOpenRouterPricing(data)
	require.NoError(t, err)

	assert.Equal(t, []ModelPricing{
		{
			ModelPattern:         "minimax/minimax-m3",
			InputPerMTok:         money.Money{Microdollars: 5_000_000},
			OutputPerMTok:        money.Money{Microdollars: 25_000_000},
			CacheCreationPerMTok: money.Money{Microdollars: 6_250_000},
			CacheReadPerMTok:     money.Money{Microdollars: 500_000},
		},
		{
			ModelPattern:  "provider/plain",
			InputPerMTok:  money.Money{Microdollars: 1_000_000},
			OutputPerMTok: money.Money{Microdollars: 2_000_000},
		},
		{ModelPattern: "provider/free"},
	}, prices, "text-output models kept; non-text, negative, "+
		"malformed, and unpriced entries skipped")
}

func TestParseOpenRouterPricingRejectsInvalidJSON(t *testing.T) {
	_, err := ParseOpenRouterPricing([]byte(`<html>`))
	require.Error(t, err)
}

func TestFetchOpenRouterPricing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		_, _ = w.Write([]byte(`{"data": [{
			"id": "provider/model",
			"pricing": {"prompt": "0.000001", "completion": "0.000002"}
		}]}`))
	}))
	t.Cleanup(server.Close)

	prices, err := fetchOpenRouterPricing(
		context.Background(), server.Client(), server.URL,
	)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	assert.Equal(t, "provider/model", prices[0].ModelPattern)
}

func TestFetchOpenRouterPricingRejectsErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	_, err := fetchOpenRouterPricing(
		context.Background(), server.Client(), server.URL,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 503")
}
