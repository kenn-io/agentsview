package db

import "go.kenn.io/agentsview/internal/usagefacts"

// MaxPlausibleTokens bounds a single parsed token count. Session totals may
// legitimately exceed this by summing many rows, but one row-level token field
// above this limit is treated as corrupt input.
const MaxPlausibleTokens = usagefacts.MaxPlausibleTokens

// ClampPlausibleTokens bounds a single token count to the accepted row-level
// range. Negative counts are floored at zero.
func ClampPlausibleTokens(v int64) int {
	return int(usagefacts.ClampPlausibleTokens(v))
}
