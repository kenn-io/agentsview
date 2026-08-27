package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestParseLiteLLMPricingBandsNormalizesSupportedThresholdFields(t *testing.T) {
	data := []byte(`{
		"banded-model": {
			"input_cost_per_token": 0.000001,
			"output_cost_per_token": 0.000002,
			"cache_creation_input_token_cost": 0.0000005,
			"cache_read_input_token_cost": 0.0000001,
			"input_cost_per_token_above_272k_tokens": 0.000004,
			"output_cost_per_token_above_272k_tokens": 0.000005,
			"cache_read_input_token_cost_above_272k_tokens": 0,
			"input_cost_per_token_above_200000_tokens": 0.000002,
			"cache_creation_input_token_cost_above_200000_tokens": 0.000001
		}
	}`)

	prices, err := ParseLiteLLMPricing(data)
	require.NoError(t, err)
	require.Len(t, prices, 1)

	assert.Equal(t, []PricingBand{
		{
			AboveInputTokens:     200_000,
			InputPerMTok:         money.Money{Microdollars: 2_000_000},
			OutputPerMTok:        money.Money{Microdollars: 2_000_000},
			CacheCreationPerMTok: money.Money{Microdollars: 1_000_000},
			CacheReadPerMTok:     money.Money{Microdollars: 100_000},
		},
		{
			AboveInputTokens:     272_000,
			InputPerMTok:         money.Money{Microdollars: 4_000_000},
			OutputPerMTok:        money.Money{Microdollars: 5_000_000},
			CacheCreationPerMTok: money.Money{Microdollars: 500_000},
			CacheReadPerMTok:     money.Money{},
		},
	}, prices[0].Bands)
}

func TestParseLiteLLMPricingReads1hCacheCreationRate(t *testing.T) {
	data := []byte(`{
		"claude-fable-5": {
			"input_cost_per_token": 0.00001,
			"output_cost_per_token": 0.00005,
			"cache_creation_input_token_cost": 0.0000125,
			"cache_creation_input_token_cost_above_1hr": 0.00002,
			"cache_read_input_token_cost": 0.000001
		}
	}`)

	prices, err := ParseLiteLLMPricing(data)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	assert.Equal(t, money.Money{Microdollars: 12_500_000},
		prices[0].CacheCreationPerMTok)
	assert.Equal(t, money.Money{Microdollars: 20_000_000},
		prices[0].CacheCreation1hPerMTok)
}

func TestParseLiteLLMPricingBands1hCacheCreationCompanion(t *testing.T) {
	data := []byte(`{
		"banded-model": {
			"input_cost_per_token": 0.000001,
			"output_cost_per_token": 0.000002,
			"cache_creation_input_token_cost": 0.0000005,
			"cache_creation_input_token_cost_above_1hr": 0.0000008,
			"input_cost_per_token_above_200k_tokens": 0.000002,
			"cache_creation_input_token_cost_above_1hr_above_200k_tokens": 0.0000016
		}
	}`)

	prices, err := ParseLiteLLMPricing(data)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	require.Len(t, prices[0].Bands, 1)
	band := prices[0].Bands[0]
	assert.Equal(t, money.Money{Microdollars: 500_000},
		band.CacheCreationPerMTok,
		"band inherits the base 5m rate when no companion overrides it")
	assert.Equal(t, money.Money{Microdollars: 1_600_000},
		band.CacheCreation1hPerMTok)
}

func TestParseLiteLLMPricingBands1hCacheCreationInherited(t *testing.T) {
	data := []byte(`{
		"banded-model": {
			"input_cost_per_token": 0.000001,
			"output_cost_per_token": 0.000002,
			"cache_creation_input_token_cost_above_1hr": 0.0000008,
			"input_cost_per_token_above_200k_tokens": 0.000002
		}
	}`)

	prices, err := ParseLiteLLMPricing(data)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	require.Len(t, prices[0].Bands, 1)
	assert.Equal(t, money.Money{Microdollars: 800_000},
		prices[0].Bands[0].CacheCreation1hPerMTok,
		"band inherits the base 1h rate")
}

func TestParseLiteLLMPricingBandsIgnoresUnanchoredPricingMetadata(t *testing.T) {
	data := []byte(`{
		"flat-model": {
			"input_cost_per_token": 0.000001,
			"output_cost_per_token": 0.000002,
			"input_cost_per_token_above_272k_tokens_priority": 0.000009,
			"input_cost_per_token_above_272k_tokens_batch": 0.0000005,
			"input_cost_per_token_above_200k_tokens_us_east": 0.000008,
			"cache_creation_input_token_cost_above_1hr": 0.000007,
			"max_input_tokens": 272000,
			"tiered_pricing": [{"threshold": 200000, "input_cost_per_token": 0.000006}]
		}
	}`)

	prices, err := ParseLiteLLMPricing(data)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	assert.Empty(t, prices[0].Bands)
}

func TestParseLiteLLMPricingBandsRejectsInvalidThresholds(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "zero", key: "input_cost_per_token_above_0_tokens"},
		{name: "overflow", key: "input_cost_per_token_above_18446744073709551615k_tokens"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(`{"model":{"input_cost_per_token":0.000001,"` + tt.key + `":0.000002}}`)

			_, err := ParseLiteLLMPricing(data)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "model")
			assert.Contains(t, err.Error(), "threshold")
		})
	}
}

func TestParseLiteLLMPricingBandsRejectsDuplicateNormalizedThreshold(t *testing.T) {
	data := []byte(`{
		"model": {
			"input_cost_per_token": 0.000001,
			"input_cost_per_token_above_1k_tokens": 0.000002,
			"input_cost_per_token_above_1000_tokens": 0.000003
		}
	}`)

	_, err := ParseLiteLLMPricing(data)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate pricing threshold 1000")
}

func TestFetchLiteLLMPricingHonorsCanceledContext(t *testing.T) {
	var requested atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		requested.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetchLiteLLMPricing(ctx, server.Client(), server.URL)

	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, requested.Load())
}
