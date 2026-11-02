package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// The independent model scores matching selectors rather than following the
// implementation's lookup order. Removing device precedence must break it.
func TestRawRolloutPrecedence(t *testing.T) {
	requireHegel(t)
	hegel.Test(t, func(ht *hegel.T) {
		if hegel.Draw(ht, hegel.Booleans()) {
			require.Equal(ht, "active", effectiveRawRollout(nil, "device-a", "codex"))
			return
		}
		var scopes []RawRolloutScope
		want, rank := "active", -1
		for i, key := range [][2]string{{"", ""}, {"", "codex"}, {"device-a", ""}, {"device-a", "codex"}, {"other", "codex"}, {"", "other"}} {
			if i > 0 && !hegel.Draw(ht, hegel.Booleans()) {
				continue
			}
			state := "shadow"
			if hegel.Draw(ht, hegel.Booleans()) {
				state = "active"
			}
			scopes = append(scopes, RawRolloutScope{DeviceID: key[0], Provider: key[1], State: state})
			score := 0
			if key[0] == "other" || key[1] == "other" {
				continue
			}
			if key[0] != "" {
				score += 2
			}
			if key[1] != "" {
				score++
			}
			if score > rank {
				want, rank = state, score
			}
		}
		require.Equal(ht, want, effectiveRawRollout(scopes, "device-a", "codex"))
	}, hegel.WithTestCases(2000))
}
