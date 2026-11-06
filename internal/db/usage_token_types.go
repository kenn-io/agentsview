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

// weights returns 1 for each selected counter, in Total's argument order.
func (t UsageTokenTypes) weights() [4]int {
	t = t.normalized()
	var w [4]int
	for i, bit := range []UsageTokenTypes{UsageTokenTypeInput, UsageTokenTypeOutput, UsageTokenTypeCacheWrite, UsageTokenTypeCacheRead} {
		if t&bit != 0 {
			w[i] = 1
		}
	}
	return w
}

// Total sums only counters selected by this set.
func (t UsageTokenTypes) Total(
	input, output, cacheWrite, cacheRead int,
) int {
	w := t.weights()
	return w[0]*input + w[1]*output + w[2]*cacheWrite + w[3]*cacheRead
}
