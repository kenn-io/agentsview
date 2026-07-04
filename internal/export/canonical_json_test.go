package export

import (
	"crypto/sha256"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	changed[0].Rates.OutputPerMTok = 16

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
		"sha256:2fefcc08c76a96e8972e858bf83d9f90e394ba1558c9d71a4a488ebcc6e925f1",
		digest,
	)
}

func TestEffectivePricingDigestRejectsNonFiniteRates(t *testing.T) {
	_, err := EffectivePricingDigest([]EffectivePricingRow{{
		ModelPattern: "bad-model",
		Rates: ModelRates{
			InputPerMTok: math.NaN(),
		},
	}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical")
}

func digestFixtureRows(t *testing.T) []EffectivePricingRow {
	t.Helper()

	updatedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	return []EffectivePricingRow{
		{
			ModelPattern: "claude-*",
			Rates: ModelRates{
				InputPerMTok:      3,
				OutputPerMTok:     15,
				CacheWritePerMTok: 3.75,
				CacheReadPerMTok:  0.30,
				UpdatedAt:         &updatedAt,
				Source:            PricingRowSourceEmbedded,
			},
		},
		{
			ModelPattern: "gpt-*",
			Rates: ModelRates{
				InputPerMTok:      1,
				OutputPerMTok:     5,
				CacheWritePerMTok: 1.25,
				CacheReadPerMTok:  0.10,
				Source:            PricingRowSourceCustom,
			},
		},
	}
}
