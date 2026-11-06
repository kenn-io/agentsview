package db

import (
	"errors"
	"fmt"
	"strings"
)

// UsageTokenTypes selects token counters with distinct pricing economics.
// Its zero value intentionally means all token types so existing store callers
// retain the historical total-token behavior.
type UsageTokenTypes uint8

const (
	UsageTokenTypeInput UsageTokenTypes = 1 << iota
	UsageTokenTypeCacheWrite
	UsageTokenTypeCacheRead
	UsageTokenTypeOutput

	UsageTokenTypesAll = UsageTokenTypeInput |
		UsageTokenTypeCacheWrite |
		UsageTokenTypeCacheRead |
		UsageTokenTypeOutput
)

// ParseUsageTokenTypes parses the top-sessions token_types query value.
// An omitted value selects every token type.
func ParseUsageTokenTypes(raw string) (UsageTokenTypes, error) {
	if strings.TrimSpace(raw) == "" {
		return UsageTokenTypesAll, nil
	}
	var selected UsageTokenTypes
	for value := range strings.SplitSeq(raw, ",") {
		switch strings.TrimSpace(value) {
		case "input":
			selected |= UsageTokenTypeInput
		case "cache_write":
			selected |= UsageTokenTypeCacheWrite
		case "cache_read":
			selected |= UsageTokenTypeCacheRead
		case "output":
			selected |= UsageTokenTypeOutput
		default:
			return 0, fmt.Errorf("unknown token type %q", value)
		}
	}
	if selected == 0 {
		return 0, errors.New("at least one token type is required")
	}
	return selected, nil
}

func (t UsageTokenTypes) normalized() UsageTokenTypes {
	if t == 0 {
		return UsageTokenTypesAll
	}
	return t
}

// Total sums only counters selected by this set.
func (t UsageTokenTypes) Total(
	input, output, cacheWrite, cacheRead int,
) int {
	t = t.normalized()
	total := 0
	for _, counter := range usageTokenCounters(input, output, cacheWrite, cacheRead) {
		if t&counter.bit != 0 {
			total += counter.value
		}
	}
	return total
}

// SQLTotal sums the selected counters using the same selection as Total.
func (t UsageTokenTypes) SQLTotal(input, output, cacheWrite, cacheRead string) string {
	columns := []string{"0"}
	t = t.normalized()
	for _, counter := range usageTokenCounters(input, output, cacheWrite, cacheRead) {
		if t&counter.bit != 0 {
			columns = append(columns, counter.value)
		}
	}
	return strings.Join(columns, "+")
}

type usageTokenCounter[T int | string] struct {
	bit   UsageTokenTypes
	value T
}

func usageTokenCounters[T int | string](input, output, cacheWrite, cacheRead T) [4]usageTokenCounter[T] {
	return [4]usageTokenCounter[T]{
		{UsageTokenTypeInput, input},
		{UsageTokenTypeOutput, output},
		{UsageTokenTypeCacheWrite, cacheWrite},
		{UsageTokenTypeCacheRead, cacheRead},
	}
}
