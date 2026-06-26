package pricing

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFallbackPricing_Opus46Rates(t *testing.T) {
	prices := requireEmbeddedFallbackPricing(t)
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-opus-4-6" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-opus-4-6 entry missing from FallbackPricing")

	// Source: https://claude.com/pricing — Opus 4.5/4.6 tier.
	want := ModelPricing{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.50,
	}
	assert.Equal(t, want, *got)
}

func TestFallbackPricing_Opus47Rates(t *testing.T) {
	prices := requireEmbeddedFallbackPricing(t)
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-opus-4-7" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-opus-4-7 entry missing from FallbackPricing")

	// Opus 4.7 shares the Opus 4.6/4.8 tier. LiteLLM already lists
	// it, but the fallback ships it too so offline and fresh-seed
	// pricing covers the whole current Opus generation.
	want := ModelPricing{
		ModelPattern:         "claude-opus-4-7",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.50,
	}
	assert.Equal(t, want, *got)
}

func TestFallbackPricing_Opus48Rates(t *testing.T) {
	prices := requireEmbeddedFallbackPricing(t)
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-opus-4-8" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-opus-4-8 entry missing from FallbackPricing")

	// Opus 4.8 launched at the same rates as Opus 4.6/4.7 and is
	// not yet in the LiteLLM catalog, so the shipped fallback must
	// price it at the current Opus tier.
	want := ModelPricing{
		ModelPattern:         "claude-opus-4-8",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.50,
	}
	assert.Equal(t, want, *got)
}

func TestFallbackPricing_Fable5Rates(t *testing.T) {
	prices := requireEmbeddedFallbackPricing(t)
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-fable-5" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-fable-5 entry missing from FallbackPricing")

	// Source: https://platform.claude.com/docs/en/about-claude/pricing
	// Fable 5 launched at double the Opus 4.8 rates and is not yet in
	// the LiteLLM catalog, so the shipped fallback must price it.
	want := ModelPricing{
		ModelPattern:         "claude-fable-5",
		InputPerMTok:         10.0,
		OutputPerMTok:        50.0,
		CacheCreationPerMTok: 12.50,
		CacheReadPerMTok:     1.0,
	}
	assert.Equal(t, want, *got)
}

func TestFallbackPricing_HermesModels(t *testing.T) {
	byPattern := make(map[string]ModelPricing)
	for _, p := range requireEmbeddedFallbackPricing(t) {
		byPattern[p.ModelPattern] = p
	}

	// gpt-5.5 (Hermes). Source: https://developers.openai.com/api/docs/pricing
	// standard tier — input $5.00, cached input $0.50, output $30.00 per MTok.
	gpt, ok := byPattern["gpt-5.5"]
	require.True(t, ok, "gpt-5.5 entry missing from FallbackPricing")
	assert.Equal(t, 5.0, gpt.InputPerMTok)
	assert.Equal(t, 30.0, gpt.OutputPerMTok)
	assert.Equal(t, 0.50, gpt.CacheReadPerMTok)

	// openrouter/owl-alpha is a free model: a known $0 (present with
	// zero rates) rather than an unpriced/unknown model.
	owl, ok := byPattern["openrouter/owl-alpha"]
	require.True(t, ok, "openrouter/owl-alpha entry missing from FallbackPricing")
	assert.Zero(t, owl.InputPerMTok)
	assert.Zero(t, owl.OutputPerMTok)
}

func TestFallbackPricing_Deterministic(t *testing.T) {
	first := requireEmbeddedFallbackPricing(t)
	second := FallbackPricing()

	assert.Equal(t, first, second, "FallbackPricing should be deterministic")
}

func TestFallbackPricing_SortedByModelPattern(t *testing.T) {
	prices := requireEmbeddedFallbackPricing(t)
	require.Greater(t, len(prices), 0, "FallbackPricing returned empty")

	for i := 1; i < len(prices); i++ {
		prev := prices[i-1].ModelPattern
		cur := prices[i].ModelPattern
		assert.False(
			t,
			strings.Compare(prev, cur) > 0,
			"fallback pricing should be sorted for model pattern: %q before %q", prev, cur,
		)
	}
}

func TestFallbackVersion_TracksEmbeddedSnapshot(t *testing.T) {
	snapshot := requireEmbeddedFallbackSnapshot(t)
	assert.Equal(t, snapshot.Version, FallbackVersion)
	assert.NotEmpty(t, FallbackVersion, "fallback version should not be empty")
	assert.True(t, strings.HasPrefix(FallbackVersion, "litellm-"),
		"fallback version should start with litellm- prefix, got %q", FallbackVersion)
	assert.Len(t, FallbackVersion, len("litellm-")+12,
		"fallback version should be litellm- plus 12-char hex digest")
}

func TestFallbackPricing_GuaranteedModels(t *testing.T) {
	byPattern := make(map[string]struct{})
	for _, p := range requireEmbeddedFallbackPricing(t) {
		byPattern[p.ModelPattern] = struct{}{}
	}

	guaranteed := []string{
		"claude-sonnet-4-6",
		"claude-opus-4-6",
		"claude-opus-4-7",
		"claude-opus-4-8",
		"claude-fable-5",
		"claude-haiku-4-5-20251001",
		"claude-sonnet-4-20250514",
		"claude-sonnet-4-5-20250514",
		"claude-opus-4-20250514",
		"claude-haiku-3-5-20241022",
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.2-codex",
		"gpt-5.3-codex",
		"gpt-5.4-mini",
		"gpt-5.4-nano",
		"gpt-5.1-codex-max",
		"openrouter/owl-alpha",
		"mistral-large",
		"mistral-large-3",
		"mistral-medium",
		"mistral-medium-3",
		"mistral-medium-3.5",
	}

	for _, model := range guaranteed {
		_, ok := byPattern[model]
		assert.True(t, ok, "model %q must be present in FallbackPricing", model)
	}
}

func TestFallbackPricing_OverlayOnlyRates(t *testing.T) {
	byPattern := make(map[string]ModelPricing)
	for _, p := range requireEmbeddedFallbackPricing(t) {
		byPattern[p.ModelPattern] = p
	}

	cases := []struct {
		model  string
		input  float64
		output float64
	}{
		{"gpt-5.4-mini", 0.75, 4.50},
		{"gpt-5.4-nano", 0.20, 1.25},
		{"claude-haiku-3-5-20241022", 0.80, 4.0},
		{"mistral-medium-3.5", 1.5, 7.5},
		{"gpt-5.3-codex", 1.75, 14.0},
	}

	for _, tc := range cases {
		got, ok := byPattern[tc.model]
		require.True(t, ok, "%s missing", tc.model)
		assert.InDelta(t, tc.input, got.InputPerMTok, 0.001, "%s input rate", tc.model)
		assert.InDelta(t, tc.output, got.OutputPerMTok, 0.001, "%s output rate", tc.model)
	}
}

func TestDecodeFallbackSnapshotFromFS(t *testing.T) {
	snapshot := []byte(`{
		"version": "litellm-test",
		"models": [
			{"ModelPattern": "z-model", "InputPerMTok": 2},
			{"ModelPattern": "a-model", "InputPerMTok": 1}
		]
	}`)

	fsys := fstest.MapFS{
		"snapshot/litellm_snapshot.json.gz": &fstest.MapFile{
			Data: gzipData(t, snapshot),
		},
	}

	got, err := decodeFallbackSnapshotFromFS(fsys)
	require.NoError(t, err)

	assert.Equal(t, "litellm-test", got.Version)
	require.Len(t, got.Models, 2)
	assert.Equal(t, "a-model", got.Models[0].ModelPattern)
	assert.Equal(t, "z-model", got.Models[1].ModelPattern)
}

func TestDecodeFallbackSnapshotFromFS_MissingSnapshot(t *testing.T) {
	fsys := fstest.MapFS{
		"snapshot/.keep": &fstest.MapFile{
			Data: []byte("keep embed dir for generated pricing snapshot\n"),
		},
	}

	_, err := decodeFallbackSnapshotFromFS(fsys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embedded LiteLLM snapshot is missing")
	assert.Contains(t, err.Error(), "make pricing-snapshot")
}

func TestDecodeFallbackSnapshotFromFS_RejectsOversizedDecompressedPayload(t *testing.T) {
	fsys := fstest.MapFS{
		"snapshot/litellm_snapshot.json.gz": &fstest.MapFile{
			Data: gzipData(t, bytes.Repeat(
				[]byte(" "),
				maxFallbackSnapshotJSONBytes+1,
			)),
		},
	}

	_, err := decodeFallbackSnapshotFromFS(fsys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decompressed snapshot exceeds")
}

func TestDecodeFallbackSnapshotFromFS_RejectsEmptyModels(t *testing.T) {
	snapshot := []byte(`{
		"version": "litellm-test",
		"models": []
	}`)

	fsys := fstest.MapFS{
		"snapshot/litellm_snapshot.json.gz": &fstest.MapFile{
			Data: gzipData(t, snapshot),
		},
	}

	_, err := decodeFallbackSnapshotFromFS(fsys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing snapshot models")
}

func gzipData(t *testing.T, data []byte) []byte {
	t.Helper()

	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	_, err := writer.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return out.Bytes()
}

func requireEmbeddedFallbackPricing(t *testing.T) []ModelPricing {
	t.Helper()

	prices := FallbackPricing()
	require.NotEmpty(t, prices, "FallbackPricing returned empty")
	return prices
}

func requireEmbeddedFallbackSnapshot(t *testing.T) litellmFallbackSnapshot {
	t.Helper()

	snapshot, err := decodeFallbackSnapshot()
	require.NoError(t, err, "decodeFallbackSnapshot failed")
	return snapshot
}
