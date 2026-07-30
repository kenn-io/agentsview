package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
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

// MarshalCanonical renders v as canonical JSON. It honors json struct tags,
// omitempty, and custom JSON marshalers before recursively ordering object
// keys and normalizing numbers.
func MarshalCanonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical JSON input: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, fmt.Errorf("decode canonical JSON input: %w", err)
	}

	var b bytes.Buffer
	if err := writeCanonicalJSON(&b, reflect.ValueOf(normalized)); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
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

func writeCanonicalJSON(b *bytes.Buffer, v reflect.Value) error {
	if !v.IsValid() {
		b.WriteString("null")
		return nil
	}
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			b.WriteString("null")
			return nil
		}
		return writeCanonicalJSON(b, v.Elem())
	}
	if v.CanInterface() {
		if number, ok := v.Interface().(json.Number); ok {
			return writeCanonicalJSONNumber(b, number)
		}
	}
	switch v.Kind() {
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("canonical JSON only supports string map keys")
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool {
			return utf16Less(keys[i].String(), keys[j].String())
		})
		b.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, key.String())
			b.WriteByte(':')
			if err := writeCanonicalJSON(b, v.MapIndex(key)); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case reflect.Slice, reflect.Array:
		b.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonicalJSON(b, v.Index(i)); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case reflect.String:
		writeJSONString(b, v.String())
	case reflect.Bool:
		if v.Bool() {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("canonical JSON cannot encode non-finite number")
		}
		b.WriteString(formatCanonicalJSONFloat(f, v.Type().Bits()))
	default:
		return fmt.Errorf("canonical JSON unsupported type %s", v.Type())
	}
	return nil
}

func writeCanonicalJSONNumber(b *bytes.Buffer, number json.Number) error {
	value := number.String()
	if !strings.ContainsAny(value, ".eE") {
		integer, ok := new(big.Int).SetString(value, 10)
		if !ok {
			return fmt.Errorf("canonical JSON invalid number %q", value)
		}
		b.WriteString(integer.String())
		return nil
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("canonical JSON invalid number %q", value)
	}
	b.WriteString(formatCanonicalJSONFloat(f, 64))
	return nil
}

func writeJSONString(b *bytes.Buffer, s string) {
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	b.Write(bytes.TrimSpace(encoded.Bytes()))
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

func formatCanonicalJSONFloat(f float64, bits int) string {
	if f == 0 {
		return "0"
	}
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		return normalizeCanonicalExponent(strconv.FormatFloat(f, 'e', -1, bits))
	}
	return strconv.FormatFloat(f, 'f', -1, bits)
}

func normalizeCanonicalExponent(s string) string {
	mantissa, exponent, ok := strings.Cut(s, "e")
	if !ok {
		return s
	}
	sign := ""
	switch {
	case strings.HasPrefix(exponent, "+"):
		sign = "+"
		exponent = strings.TrimPrefix(exponent, "+")
	case strings.HasPrefix(exponent, "-"):
		sign = "-"
		exponent = strings.TrimPrefix(exponent, "-")
	}
	exponent = strings.TrimLeft(exponent, "0")
	if exponent == "" {
		exponent = "0"
	}
	return mantissa + "e" + sign + exponent
}

func utf16Less(a, b string) bool {
	au := utf16.Encode([]rune(a))
	bu := utf16.Encode([]rune(b))
	for i := 0; i < len(au) && i < len(bu); i++ {
		if au[i] != bu[i] {
			return au[i] < bu[i]
		}
	}
	return len(au) < len(bu)
}
