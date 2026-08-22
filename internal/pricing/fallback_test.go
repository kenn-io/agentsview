package pricing

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func testRate(dollars string) money.Money {
	rate, err := money.ParseDollars(dollars)
	if err != nil {
		panic(err)
	}
	return rate
}

func assertFlatPricing(t *testing.T, want, got ModelPricing) {
	t.Helper()
	assert.Equal(t, want.ModelPattern, got.ModelPattern)
	assert.Equal(t, want.InputPerMTok, got.InputPerMTok)
	assert.Equal(t, want.OutputPerMTok, got.OutputPerMTok)
	assert.Equal(t, want.CacheCreationPerMTok, got.CacheCreationPerMTok)
	assert.Equal(t, want.CacheReadPerMTok, got.CacheReadPerMTok)
	assert.Empty(t, got.Bands)
}

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
		InputPerMTok:         testRate("5"),
		OutputPerMTok:        testRate("25"),
		CacheCreationPerMTok: testRate("6.25"),
		CacheReadPerMTok:     testRate("0.50"),
	}
	assertFlatPricing(t, want, *got)
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
		InputPerMTok:         testRate("5"),
		OutputPerMTok:        testRate("25"),
		CacheCreationPerMTok: testRate("6.25"),
		CacheReadPerMTok:     testRate("0.50"),
	}
	assertFlatPricing(t, want, *got)
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
		InputPerMTok:         testRate("5"),
		OutputPerMTok:        testRate("25"),
		CacheCreationPerMTok: testRate("6.25"),
		CacheReadPerMTok:     testRate("0.50"),
	}
	assertFlatPricing(t, want, *got)
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
		InputPerMTok:         testRate("10"),
		OutputPerMTok:        testRate("50"),
		CacheCreationPerMTok: testRate("12.50"),
		CacheReadPerMTok:     testRate("1"),
	}
	assertFlatPricing(t, want, *got)
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
	assert.Equal(t, testRate("5"), gpt.InputPerMTok)
	assert.Equal(t, testRate("30"), gpt.OutputPerMTok)
	assert.Equal(t, testRate("0.50"), gpt.CacheReadPerMTok)

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

func TestFallbackPricing_DeepClonesPricingBands(t *testing.T) {
	original := fallbackPricing
	fallbackPricing = []ModelPricing{{
		ModelPattern: "banded-model",
		Bands: []PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     testRate("2"),
		}},
	}}
	t.Cleanup(func() {
		fallbackPricing = original
	})

	first := FallbackPricing()
	first[0].Bands[0].AboveInputTokens = 1
	second := FallbackPricing()

	assert.Equal(t, 200_000, second[0].Bands[0].AboveInputTokens)
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
		input  money.Money
		output money.Money
	}{
		{"gpt-5.4-mini", testRate("0.75"), testRate("4.50")},
		{"gpt-5.4-nano", testRate("0.20"), testRate("1.25")},
		{"claude-haiku-3-5-20241022", testRate("0.80"), testRate("4")},
		{"mistral-medium-3.5", testRate("1.5"), testRate("7.5")},
		{"gpt-5.3-codex", testRate("1.75"), testRate("14")},
	}

	for _, tc := range cases {
		got, ok := byPattern[tc.model]
		require.True(t, ok, "%s missing", tc.model)
		assert.Equal(t, tc.input, got.InputPerMTok, "%s input rate", tc.model)
		assert.Equal(t, tc.output, got.OutputPerMTok, "%s output rate", tc.model)
	}
}

func TestDecodeFallbackSnapshotFromFS(t *testing.T) {
	snapshot := []byte(`{
		"version": "litellm-test",
		"source_ref": "551e5d097c11f08fd2400a25a651b1844fcf89c2",
		"models": [
			{"ModelPattern": "z-model", "InputPerMTok": {"microdollars": 2000000}},
			{"ModelPattern": "a-model", "InputPerMTok": {"microdollars": 1000000}}
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

func TestDecodeFallbackSnapshotFromFS_SortsPricingBands(t *testing.T) {
	snapshot := []byte(`{
		"version": "litellm-test",
		"source_ref": "551e5d097c11f08fd2400a25a651b1844fcf89c2",
		"models": [{
			"ModelPattern": "banded-model",
			"InputPerMTok": {"microdollars": 1000000},
			"Bands": [
				{"above_input_tokens": 272000, "input_per_mtok": {"microdollars": 3000000}},
				{"above_input_tokens": 200000, "input_per_mtok": {"microdollars": 2000000}}
			]
		}]
	}`)
	fys := fstest.MapFS{
		"snapshot/litellm_snapshot.json.gz": &fstest.MapFile{Data: gzipData(t, snapshot)},
	}

	got, err := decodeFallbackSnapshotFromFS(fys)
	require.NoError(t, err)
	require.Len(t, got.Models, 1)
	require.Len(t, got.Models[0].Bands, 2)
	assert.Equal(t, 200_000, got.Models[0].Bands[0].AboveInputTokens)
	assert.Equal(t, 272_000, got.Models[0].Bands[1].AboveInputTokens)
}

func TestDecodeFallbackSnapshotFromFS_RejectsInvalidPricingBands(t *testing.T) {
	tests := []struct {
		name  string
		bands string
		want  string
	}{
		{
			name:  "non-positive threshold",
			bands: `[{"above_input_tokens": 0}]`,
			want:  "pricing threshold must be positive",
		},
		{
			name:  "duplicate threshold",
			bands: `[{"above_input_tokens": 200000}, {"above_input_tokens": 200000}]`,
			want:  "duplicate pricing threshold 200000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := []byte(`{"version":"litellm-test","source_ref":"551e5d097c11f08fd2400a25a651b1844fcf89c2","models":[{"ModelPattern":"model","Bands":` + tt.bands + `}]}`)
			fys := fstest.MapFS{
				"snapshot/litellm_snapshot.json.gz": &fstest.MapFile{Data: gzipData(t, snapshot)},
			}

			_, err := decodeFallbackSnapshotFromFS(fys)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
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
		"source_ref": "551e5d097c11f08fd2400a25a651b1844fcf89c2",
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

func TestDecodeFallbackSnapshotFromFSRejectsInvalidSourceRef(t *testing.T) {
	for _, sourceRef := range []string{"", "main", "ABCDEF"} {
		t.Run(sourceRef, func(t *testing.T) {
			snapshot := []byte(`{"version":"litellm-test","source_ref":"` + sourceRef + `","models":[{"ModelPattern":"model"}]}`)
			fys := fstest.MapFS{
				"snapshot/litellm_snapshot.json.gz": &fstest.MapFile{Data: gzipData(t, snapshot)},
			}

			_, err := decodeFallbackSnapshotFromFS(fys)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "missing immutable LiteLLM source ref")
		})
	}
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
