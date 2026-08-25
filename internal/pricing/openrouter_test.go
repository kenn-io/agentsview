package pricing

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func rate(dollars string) money.Money {
	return money.MustParseDollars(dollars)
}

func noGenAIPricing(context.Context) (*GenAIPrices, error) {
	return nil, errors.New("GenAI Prices unavailable")
}

func TestFetchCatalogDegradesWhenOpenRouterFails(t *testing.T) {
	litellm := []ModelPricing{
		{ModelPattern: "acme/model", InputPerMTok: rate("1")},
	}
	fetchLiteLLM := func(context.Context) ([]ModelPricing, error) {
		return litellm, nil
	}
	openrouterErr := errors.New("openrouter down")
	fetchOpenRouter := func(context.Context) ([]ModelPricing, error) {
		return nil, openrouterErr
	}

	catalog, err := fetchCatalog(
		context.Background(), noGenAIPricing, fetchLiteLLM, fetchOpenRouter,
	)

	assert.ErrorIs(t, err, openrouterErr)
	assert.Equal(t, Catalog{LiteLLM: litellm}, catalog,
		"LiteLLM rows survive an OpenRouter outage")
}

func TestFetchCatalogFailsWhenLiteLLMFails(t *testing.T) {
	litellmErr := errors.New("litellm down")
	fetchLiteLLM := func(context.Context) ([]ModelPricing, error) {
		return nil, litellmErr
	}
	fetchOpenRouter := func(context.Context) ([]ModelPricing, error) {
		t.Fatal("openrouter must not be fetched after a litellm failure")
		return nil, nil
	}

	catalog, err := fetchCatalog(
		context.Background(), noGenAIPricing, fetchLiteLLM, fetchOpenRouter,
	)

	assert.ErrorIs(t, err, litellmErr)
	assert.Equal(t, Catalog{}, catalog)
}

func TestCatalogReconcile(t *testing.T) {
	litellm := []ModelPricing{
		{ModelPattern: "minimax/MiniMax-M3", InputPerMTok: rate("2")},
		{ModelPattern: "openrouter/openai/gpt-x", InputPerMTok: rate("1")},
		{ModelPattern: "free-model"},
	}
	openrouter := []ModelPricing{
		// Same model, different spelling: LiteLLM's rate wins.
		{ModelPattern: "minimax/minimax-m3", InputPerMTok: rate("9")},
		// Same canonical name under another provider prefix.
		{ModelPattern: "openai/gpt-x", InputPerMTok: rate("4")},
		// Free in LiteLLM, paid in OpenRouter: no field backfill.
		{ModelPattern: "free-model", InputPerMTok: rate("5")},
		// A stale row from an earlier LiteLLM catalog still shadows.
		{ModelPattern: "acme/stale", InputPerMTok: rate("6")},
		{ModelPattern: "acme/only-openrouter", InputPerMTok: rate("7")},
	}
	stored := []string{
		"minimax/MiniMax-M3", "acme/Stale", "acme/previous",
		// A row from another source that shadows a delisted
		// OpenRouter row stored under a different spelling.
		"acme/Old-Spelling", "acme/old-spelling",
	}

	prices, owned, retired := Catalog{
		LiteLLM: litellm, OpenRouter: openrouter,
	}.Reconcile(stored, []string{"acme/previous", "acme/old-spelling"})

	assert.Equal(t, append(litellm, openrouter[4]), prices)
	assert.Equal(t, []string{"acme/only-openrouter", "acme/previous"}, owned,
		"a delisted OpenRouter row stays tracked until something covers it")
	assert.Equal(t, []string{"acme/old-spelling"}, retired)

	byPattern := make(map[string]ModelPricing, len(prices))
	for _, p := range prices {
		byPattern[p.ModelPattern] = p
	}
	price, ok := Resolve(byPattern, "MiniMax-M3")
	require.True(t, ok, "bare lookup resolves without a tie")
	assert.Equal(t, rate("2"), price.InputPerMTok)
	price, ok = Resolve(byPattern, "only-openrouter")
	require.True(t, ok, "OpenRouter-only model resolves by bare name")
	assert.Equal(t, rate("7"), price.InputPerMTok)
}

func TestCatalogReconcileRetiresShadowedPrevious(t *testing.T) {
	c := Catalog{
		LiteLLM: []ModelPricing{
			{ModelPattern: "minimax/MiniMax-M3"},
			{ModelPattern: "acme/now-in-litellm"},
		},
		OpenRouter: []ModelPricing{
			{ModelPattern: "minimax/minimax-m3"},
			{ModelPattern: "acme/still-listed"},
		},
	}
	previous := []string{
		"acme/still-listed",
		// LiteLLM now spells this model differently: retire it.
		"minimax/minimax-m3",
		// LiteLLM adopted the exact spelling: LiteLLM owns it now.
		"acme/now-in-litellm",
		// OpenRouter dropped it and nothing replaces it: keep pricing.
		"acme/delisted",
	}

	prices, owned, retired := c.Reconcile(
		append([]string{"minimax/MiniMax-M3"}, previous...), previous,
	)

	assert.Equal(t, []ModelPricing{
		{ModelPattern: "minimax/MiniMax-M3"},
		{ModelPattern: "acme/now-in-litellm"},
		{ModelPattern: "acme/still-listed"},
	}, prices)
	assert.Equal(t, []string{"acme/delisted", "acme/still-listed"}, owned)
	assert.Equal(t, []string{"minimax/minimax-m3"}, retired)
}

func TestShadowedPatterns(t *testing.T) {
	catalog := []string{"minimax/MiniMax-M3", "acme/listed", "Bare-Model"}

	got := ShadowedPatterns(catalog, []string{
		"minimax/minimax-m3", // spelled differently: shadowed
		"acme/listed",        // listed exactly: not shadowed
		"bare-model",         // bare spelling of a covered name: shadowed
		"acme/delisted",      // nothing covers it: kept
	})

	assert.Equal(t, []string{"bare-model", "minimax/minimax-m3"}, got)
	assert.Empty(t, ShadowedPatterns(nil, []string{"acme/model"}))
}

func TestOpenRouterModelsRoundTrip(t *testing.T) {
	assert.Equal(t, "[]", EncodeOpenRouterModels(nil))
	decoded, err := DecodeOpenRouterModels(EncodeOpenRouterModels(
		[]string{"a", "b"},
	))
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, decoded)
	decoded, err = DecodeOpenRouterModels("")
	require.NoError(t, err)
	assert.Nil(t, decoded)
	_, err = DecodeOpenRouterModels("not json")
	require.Error(t, err)
}
