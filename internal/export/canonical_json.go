package export

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func EffectivePricingDigest(rows []EffectivePricingRow) (string, error) {
	canonical, err := canonicalPricingJSON(canonicalPricingRows(rows))
	if err != nil {
		return "", fmt.Errorf("canonical pricing digest: %w", err)
	}
	return digestCanonicalBytes(canonical), nil
}

func canonicalPricingRows(rows []EffectivePricingRow) map[string]any {
	copied := make([]EffectivePricingRow, len(rows))
	copy(copied, rows)
	sort.SliceStable(copied, func(i, j int) bool {
		return canonicalPricingRowLess(copied[i], copied[j])
	})
	out := make([]any, 0, len(copied))
	for _, row := range copied {
		if row.GenAI != nil {
			var updatedAt any
			if row.GenAIUpdatedAt != nil {
				updatedAt = row.GenAIUpdatedAt.UTC().Format(jsonTimeLayout)
			}
			out = append(out, map[string]any{
				"genai_prices": map[string]any{
					"source":     string(row.GenAISource),
					"updated_at": updatedAt,
					"version":    row.GenAIVersion,
				},
			})
			continue
		}
		var updatedAt any
		if row.Rates.UpdatedAt != nil {
			updatedAt = row.Rates.UpdatedAt.UTC().Format(jsonTimeLayout)
		}
		out = append(out, map[string]any{
			"bands":                canonicalPricingBands(row.Rates.Bands),
			"cache_read_per_mtok":  row.Rates.CacheReadPerMTok.Microdollars,
			"cache_write_per_mtok": row.Rates.CacheWritePerMTok.Microdollars,
			"input_per_mtok":       row.Rates.InputPerMTok.Microdollars,
			"model_pattern":        row.ModelPattern,
			"output_per_mtok":      row.Rates.OutputPerMTok.Microdollars,
			"source":               string(row.Rates.Source),
			"updated_at":           updatedAt,
		})
	}
	return map[string]any{"rows": out}
}

func canonicalPricingBands(bands []PricingBand) []any {
	copied := append([]PricingBand(nil), bands...)
	sort.SliceStable(copied, func(i, j int) bool {
		return copied[i].AboveInputTokens < copied[j].AboveInputTokens
	})
	out := make([]any, 0, len(copied))
	for _, band := range copied {
		var updatedAt any
		if band.UpdatedAt != nil {
			updatedAt = band.UpdatedAt.UTC().Format(jsonTimeLayout)
		}
		out = append(out, map[string]any{
			"above_input_tokens":   band.AboveInputTokens,
			"cache_read_per_mtok":  band.CacheReadPerMTok.Microdollars,
			"cache_write_per_mtok": band.CacheWritePerMTok.Microdollars,
			"input_per_mtok":       band.InputPerMTok.Microdollars,
			"output_per_mtok":      band.OutputPerMTok.Microdollars,
			"updated_at":           updatedAt,
		})
	}
	return out
}

const jsonTimeLayout = "2006-01-02T15:04:05Z07:00"

func canonicalPricingJSON(v any) ([]byte, error) {
	return MarshalCanonical(v)
}

// MarshalCanonical renders v using the JSON Canonicalization Scheme while
// preserving integer precision.
func MarshalCanonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical JSON input: %w", err)
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, fmt.Errorf("canonicalize JSON input: %w", err)
	}
	return canonical, nil
}

// DigestCanonical returns the SHA-256 identity of v's canonical JSON.
func DigestCanonical(v any) (string, error) {
	canonical, err := MarshalCanonical(v)
	if err != nil {
		return "", err
	}
	return digestCanonicalBytes(canonical), nil
}

func digestCanonicalBytes(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return "sha256:" + fmt.Sprintf("%x", sum)
}

func canonicalPricingRowLess(a, b EffectivePricingRow) bool {
	aValues := canonicalPricingRowSortValues(a)
	bValues := canonicalPricingRowSortValues(b)
	for i := range aValues {
		if aValues[i] != bValues[i] {
			return aValues[i] < bValues[i]
		}
	}
	return false
}

func canonicalPricingRowSortValues(row EffectivePricingRow) []string {
	if row.GenAI != nil {
		updatedAt := ""
		if row.GenAIUpdatedAt != nil {
			updatedAt = row.GenAIUpdatedAt.UTC().Format(jsonTimeLayout)
		}
		return []string{
			"", "genai_prices", row.GenAIVersion,
			string(row.GenAISource), updatedAt, "", "", "",
		}
	}
	updatedAt := ""
	if row.Rates.UpdatedAt != nil {
		updatedAt = row.Rates.UpdatedAt.UTC().Format(jsonTimeLayout)
	}
	return []string{
		row.ModelPattern,
		string(row.Rates.Source),
		strconv.FormatInt(row.Rates.InputPerMTok.Microdollars, 10),
		strconv.FormatInt(row.Rates.OutputPerMTok.Microdollars, 10),
		strconv.FormatInt(row.Rates.CacheWritePerMTok.Microdollars, 10),
		strconv.FormatInt(row.Rates.CacheReadPerMTok.Microdollars, 10),
		updatedAt,
		canonicalPricingBandsSortKey(row.Rates.Bands),
	}
}

func canonicalPricingBandsSortKey(bands []PricingBand) string {
	copied := append([]PricingBand(nil), bands...)
	sort.SliceStable(copied, func(i, j int) bool {
		return copied[i].AboveInputTokens < copied[j].AboveInputTokens
	})
	var key strings.Builder
	for _, band := range copied {
		values := []int64{
			int64(band.AboveInputTokens),
			band.InputPerMTok.Microdollars,
			band.OutputPerMTok.Microdollars,
			band.CacheWritePerMTok.Microdollars,
			band.CacheReadPerMTok.Microdollars,
		}
		for _, value := range values {
			key.WriteString(strconv.FormatInt(value, 10))
			key.WriteByte(',')
		}
		if band.UpdatedAt != nil {
			key.WriteString(band.UpdatedAt.UTC().Format(jsonTimeLayout))
		}
		key.WriteByte(';')
	}
	return key.String()
}
