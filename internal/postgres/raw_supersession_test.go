package postgres

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawparity"
	"hegel.dev/go/hegel"
)

// The oracle counts edges in a generated logical identity graph, independently
// of physical cohorts and the implementation's connected-component matcher.
func TestRawLogicalSupersessionsModel(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		var rows []rawparity.Candidate
		var edges [3][3]bool
		var local, served [3]bool
		var copies [3]int
		previous := map[string]string{}
		active := map[string]bool{}
		for j := range 3 {
			rows = append(rows, rawparity.Candidate{ID: fmt.Sprint("legacy-", j), Agent: "codex"})
		}
		for i := range 3 {
			local[i], served[i] = hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans())
			group := fmt.Sprint("group-", i)
			active[group] = served[i]
			var aliases []string
			for j := range 3 {
				edges[i][j] = hegel.Draw(ht, hegel.Booleans())
				if edges[i][j] {
					aliases = append(aliases, fmt.Sprint("legacy-", j))
				}
			}
			copies[i] = 1
			if hegel.Draw(ht, hegel.Booleans()) {
				copies[i]++
			}
			for k := range copies[i] {
				rows = append(rows, rawparity.Candidate{ID: fmt.Sprintf("physical-%d-%d", i, k), PublicID: "irrelevant", Group: group, Agent: "codex", Aliases: aliases, SourceLocal: local[i]})
			}
		}
		for j := range 3 {
			if hegel.Draw(ht, hegel.Booleans()) {
				previous[fmt.Sprint("legacy-", j)] = fmt.Sprint("group-", hegel.Draw(ht, hegel.Integers(0, 2)))
			}
		}
		want := map[string]string{}
		for i := range 3 {
			for j := range 3 {
				degreeRaw, degreeLegacy := 0, 0
				for k := range 3 {
					if edges[i][k] {
						degreeRaw++
					}
					if edges[k][j] {
						degreeLegacy++
					}
				}
				if edges[i][j] && degreeRaw == 1 && degreeLegacy == 1 && !local[i] && served[i] && (copies[i] == 1 || previous[fmt.Sprint("legacy-", j)] == fmt.Sprint("group-", i)) {
					want[fmt.Sprint("legacy-", j)] = fmt.Sprint("group-", i)
				}
			}
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			slices.Reverse(rows)
		}
		require.Equal(ht, want, rawLogicalSupersessions(rows, active, previous))
	}, hegel.WithTestCases(2000))
}

// Removing either side of an established identity removes its map, even if the
// caller still marks the old logical group served. No stale edge is evidence.
func TestRawLogicalSupersessionsMissingNodes(t *testing.T) {
	legacy := rawparity.Candidate{ID: "legacy", Agent: "codex"}
	raw := rawparity.Candidate{ID: "physical", Group: "group", Agent: "codex", Aliases: []string{"legacy"}}
	for _, rows := range [][]rawparity.Candidate{nil, {legacy}, {raw}} {
		require.Empty(t, rawLogicalSupersessions(rows, map[string]bool{"group": true}, map[string]string{"legacy": "group"}))
	}
	require.Equal(t, map[string]string{"legacy": "group"}, rawLogicalSupersessions([]rawparity.Candidate{legacy, raw}, map[string]bool{"group": true}, nil))
}

func TestRawLogicalSupersessionsUsesProviderAndCanonicalIdentity(t *testing.T) {
	for _, tc := range []struct {
		legacy, agent, alias string
		local, match         bool
	}{
		{"codex:x", "codex", "codex:x", false, true},
		{"codex:x", "claude", "codex:x", false, false},
		{"codex:x", "codex", "CODEX:x", false, false},
		{"codex:x~host", "codex", "codex:x~host", false, false},
		{"codex:x", "codex", "codex:x", true, false},
	} {
		rows := []rawparity.Candidate{{ID: tc.legacy, Agent: tc.agent}, {ID: "physical", Group: "group", Agent: "codex", Aliases: []string{tc.alias}, SourceLocal: tc.local}}
		want := map[string]string{}
		if tc.match {
			want[tc.legacy] = "group"
		}
		require.Equal(t, want, rawLogicalSupersessions(rows, map[string]bool{"group": true}, nil))
		require.Empty(t, rawLogicalSupersessions(rows, nil, nil), "no audited served groups cannot supersede legacy")
	}
}

func TestRawLegacyPinMatchesUUIDAndNormalizedContent(t *testing.T) {
	source := db.Message{Ordinal: 2, Role: "user", Content: "retained", SourceUUID: "uuid"}
	for _, tc := range []struct {
		name     string
		messages []db.Message
		ordinal  int
		resolved bool
	}{
		{"same", []db.Message{source}, 2, true},
		{"moved", []db.Message{{Ordinal: 7, Role: "user", Content: "retained", SourceUUID: "uuid"}}, 7, true},
		{"reused UUID", []db.Message{{Ordinal: 2, Role: "user", Content: "changed", SourceUUID: "uuid"}}, 0, false},
		{"missing", nil, 0, false},
		{"different UUID", []db.Message{{Ordinal: 2, Role: "user", Content: "retained", SourceUUID: "other"}}, 0, false},
		{"ambiguous", []db.Message{source, {Ordinal: 7, Role: "user", Content: "retained", SourceUUID: "uuid"}}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var targets []rawCarryMessage
			for _, m := range tc.messages {
				key, err := rawMessageKey(m)
				require.NoError(t, err)
				targets = append(targets, rawCarryMessage{Message: m, Key: key, Revision: "target-revision"})
			}
			ref, err := matchRawLegacyPin(source, targets)
			require.NoError(t, err)
			require.Equal(t, tc.resolved, ref.Resolved)
			if tc.resolved {
				require.Equal(t, tc.ordinal, ref.Ordinal)
				require.Equal(t, "target-revision", ref.ContentRevision)
				sourceKey, err := rawMessageKey(source)
				require.NoError(t, err)
				if tc.ordinal != source.Ordinal {
					require.NotEqual(t, sourceKey, ref.MessageKey)
				}
			}
		})
	}
	source.SourceUUID = ""
	ref, err := matchRawLegacyPin(source, []rawCarryMessage{{Message: source, Key: "key"}})
	require.NoError(t, err)
	require.False(t, ref.Resolved)
}

// A normalized match is ordinal-independent, but the installed pin key belongs
// to the exact target message and includes its ordinal.
func TestRawLegacyPinTargetKeyProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ordinal := int(hegel.Draw(ht, hegel.Integers(0, math.MaxInt32)))
		source := db.Message{Ordinal: 0, Role: "user", Content: hegel.Draw(ht, hegel.Text()), SourceUUID: "uuid:" + hegel.Draw(ht, hegel.Text())}
		target := source
		target.Ordinal = ordinal
		key, err := rawMessageKey(target)
		require.NoError(ht, err)
		ref, err := matchRawLegacyPin(source, []rawCarryMessage{{Message: target, Key: key, Revision: "target"}, {Message: target, Key: key, Revision: "duplicate-cohort"}})
		require.NoError(ht, err)
		require.True(ht, ref.Resolved)
		require.Equal(ht, ordinal, ref.Ordinal)
		require.Equal(ht, key, ref.MessageKey)
	}, hegel.WithTestCases(1000))
}
