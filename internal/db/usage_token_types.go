package db

import (
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
		return 0, fmt.Errorf("at least one token type is required")
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
	if t&UsageTokenTypeInput != 0 {
		total += input
	}
	if t&UsageTokenTypeCacheWrite != 0 {
		total += cacheWrite
	}
	if t&UsageTokenTypeCacheRead != 0 {
		total += cacheRead
	}
	if t&UsageTokenTypeOutput != 0 {
		total += output
	}
	return total
}
