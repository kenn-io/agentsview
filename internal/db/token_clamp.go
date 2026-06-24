package db

// MaxPlausibleTokens bounds a single parsed token count. Session totals may
// legitimately exceed this by summing many rows, but one row-level token field
// above this limit is treated as corrupt input.
const MaxPlausibleTokens = 2_000_000

// ClampPlausibleTokens bounds a single token count to the accepted row-level
// range. Negative counts are floored at zero.
func ClampPlausibleTokens(v int64) int {
	switch {
	case v < 0:
		return 0
	case v > MaxPlausibleTokens:
		return MaxPlausibleTokens
	default:
		return int(v)
	}
}
