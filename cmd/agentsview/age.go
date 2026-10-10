package main

import (
	"fmt"
	"strconv"
	"strings"
)

// parseAgeDuration converts a user-supplied age ("30d", "2w", "1y")
// into a whole number of days. It is the syntax behind
// `prune --age`, which resolves to `--before <today - days>`.
func parseAgeDuration(s string) (int, error) {
	trimmed := strings.TrimSpace(strings.ToLower(s))
	if trimmed == "" {
		return 0, fmt.Errorf("age is empty; use forms like 7d, 30d, 2w, or 1y")
	}
	unit := trimmed[len(trimmed)-1]
	digits := trimmed[:len(trimmed)-1]
	value, err := strconv.Atoi(digits)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf(
			"invalid age %q; use forms like 7d, 30d, 2w, or 1y", s)
	}
	switch unit {
	case 'd':
		return value, nil
	case 'w':
		return value * 7, nil
	case 'y':
		return value * 365, nil
	default:
		return 0, fmt.Errorf(
			"invalid age %q; use forms like 7d, 30d, 2w, or 1y", s)
	}
}
