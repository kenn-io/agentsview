package export

import (
	"crypto/sha256"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestCanonicalPricingJSONOrdersObjectKeys(t *testing.T) {
	got, err := canonicalPricingJSON(map[string]any{
		"b": "second",
		"a": "first",
	})
	require.NoError(t, err)

	assert.Equal(t, `{"a":"first","b":"second"}`, string(got))
}

func TestCanonicalPricingJSONDoesNotEscapeHTMLCharacters(t *testing.T) {
	got, err := canonicalPricingJSON(map[string]any{
		"text": "<tag>&value",
	})
	require.NoError(t, err)

	assert.Equal(t, `{"text":"<tag>&value"}`, string(got))
}

func TestCanonicalPricingJSONFormatsNumbers(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		want  string
	}{
		{name: "negative zero", value: math.Copysign(0, -1), want: `{"n":0}`},
		{name: "large exponent", value: 1e21, want: `{"n":1e+21}`},
		{name: "small exponent", value: 1e-7, want: `{"n":1e-7}`},
		{name: "plain decimal", value: 0.000001, want: `{"n":0.000001}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := canonicalPricingJSON(map[string]any{"n": tt.value})
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestMarshalCanonicalHonorsJSONContract(t *testing.T) {
	type fixture struct {
		Ignored string         `json:"-"`
		Second  string         `json:"second"`
		First   map[string]any `json:"first"`
		Empty   string         `json:"empty,omitempty"`
	}

	got, err := MarshalCanonical(fixture{
		Ignored: "private",
		Second:  "<visible>",
		First: map[string]any{
			"integer": int64(9_007_199_254_740_993),
			"decimal": 0.000001,
		},
	})
	require.NoError(t, err)

	assert.Equal(
		t,
		`{"first":{"decimal":0.000001,"integer":9007199254740993},"second":"<visible>"}`,
		string(got),
	)
}

func TestMarshalCanonicalVectors(t *testing.T) {
	tests := []struct {
		name   string
		input  any
		bytes  string
		digest string
	}{
		{
			name: "UTF-16 key order",
			input: map[string]any{
				"\ue000":     "bmp",
				"\U00010000": "astral",
			},
			bytes:  `{"𐀀":"astral","":"bmp"}`,
			digest: "sha256:5e72745dd500f8b8d997ef851679707b89099da29d2aca4b93dfd85810ebaa20",
		},
		{
			name:   "string escaping",
			input:  map[string]any{"text": "<>&\b\f\u2028\u2029"},
			bytes:  "{\"text\":\"<>&\\b\\f\u2028\u2029\"}",
			digest: "sha256:e5de86e813b24e83a074160c5e30141ab85fb400e09f0e10a1594ce2cfedeccf",
		},
		{
			name:   "exact integer above double range",
			input:  map[string]any{"n": int64(9_007_199_254_740_993)},
			bytes:  `{"n":9007199254740993}`,
			digest: "sha256:4ac8309cc76123ef6c5325ef925fc873e9b5856ec4f844ef1462f9303960378a",
		},
		{
			name: "floating point forms",
			input: map[string]any{
				"large":         1e21,
				"negative_zero": math.Copysign(0, -1),
				"plain":         1e-6,
				"small":         1e-7,
			},
			bytes:  `{"large":1e+21,"negative_zero":0,"plain":0.000001,"small":1e-7}`,
			digest: "sha256:940f129aabf5afc6800add24fbf597727e9dc6316f6ae10adbc78a3362b1c483",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canonical, err := MarshalCanonical(tt.input)
			require.NoError(t, err)
			digest, err := DigestCanonical(tt.input)
			require.NoError(t, err)

			assert.Equal(t, tt.bytes, string(canonical))
			assert.Equal(t, tt.digest, digest)
		})
	}
}

func TestEffectivePricingDigestIgnoresPricingRowInsertionOrder(t *testing.T) {
	rows := digestFixtureRows(t)
	reversed := []EffectivePricingRow{rows[1], rows[0]}

	digest, err := EffectivePricingDigest(rows)
	require.NoError(t, err)
	reversedDigest, err := EffectivePricingDigest(reversed)
	require.NoError(t, err)

	assert.Equal(t, digest, reversedDigest)
}

func TestEffectivePricingDigestChangesWhenRateChanges(t *testing.T) {
	rows := digestFixtureRows(t)
	changed := digestFixtureRows(t)
	changed[0].Rates.OutputPerMTok = money.MustParseDollars("16")

	digest, err := EffectivePricingDigest(rows)
	require.NoError(t, err)
	changedDigest, err := EffectivePricingDigest(changed)
	require.NoError(t, err)

	assert.NotEqual(t, digest, changedDigest)
}

func TestEffectivePricingDigestChangesWhenPricingBandChanges(t *testing.T) {
	rows := digestFixtureRows(t)
	rows[0].Rates.Bands = []PricingBand{{
		AboveInputTokens: 200_000,
		InputPerMTok:     money.MustParseDollars("6"),
	}}
	changed := digestFixtureRows(t)
	changed[0].Rates.Bands = []PricingBand{{
		AboveInputTokens: 200_000,
		InputPerMTok:     money.MustParseDollars("7"),
	}}

	digest, err := EffectivePricingDigest(rows)
	require.NoError(t, err)
	changedDigest, err := EffectivePricingDigest(changed)
	require.NoError(t, err)

	assert.NotEqual(t, digest, changedDigest)
}

func TestEffectivePricingDigestFixture(t *testing.T) {
	rows := digestFixtureRows(t)
	canonical, err := canonicalPricingJSON(canonicalPricingRows(rows))
	require.NoError(t, err)
	sum := sha256.Sum256(canonical)
	digest, err := EffectivePricingDigest(rows)
	require.NoError(t, err)

	require.Equal(t, "sha256:"+fmt.Sprintf("%x", sum), digest)
	assert.Equal(t,
		"sha256:247836888d2c78a5fda3d0e391bbc28a7fd58b4fb3af9b0d5a0e037a9f3faf0b",
		digest,
	)
}

func digestFixtureRows(t *testing.T) []EffectivePricingRow {
	t.Helper()

	updatedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	return []EffectivePricingRow{
		{
			ModelPattern: "claude-*",
			Rates: ModelRates{
				InputPerMTok:      money.MustParseDollars("3"),
				OutputPerMTok:     money.MustParseDollars("15"),
				CacheWritePerMTok: money.MustParseDollars("3.75"),
				CacheReadPerMTok:  money.MustParseDollars("0.30"),
				UpdatedAt:         &updatedAt,
				Source:            PricingRowSourceEmbedded,
			},
		},
		{
			ModelPattern: "gpt-*",
			Rates: ModelRates{
				InputPerMTok:      money.MustParseDollars("1"),
				OutputPerMTok:     money.MustParseDollars("5"),
				CacheWritePerMTok: money.MustParseDollars("1.25"),
				CacheReadPerMTok:  money.MustParseDollars("0.10"),
				Source:            PricingRowSourceCustom,
			},
		},
	}
}
