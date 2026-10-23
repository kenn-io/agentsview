package rawparity

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Exact identities must not collapse a page onto its thread or another agent.
func TestMatchProviderIdentities(t *testing.T) {
	for _, tc := range []struct {
		agent, raw, legacy, want string
		local                    bool
	}{
		{"codex", "codex:thread", "codex:thread", "equal", false},
		{"codex", "codex:thread_page", "codex:thread", "raw_only", false},
		{"codex", "codex:thread_page", "codex:thread_page", "equal", false},
		{"traex", "traex:thread_page", "codex:thread_page", "raw_only", false},
		{"augure-code", "augure-code:thread", "augure-code:thread", "equal", false},
		{"codex", "codex:thread", "host~codex:thread", "raw_only", false},
		{"piebald", "piebald:1", "piebald:1", "unmatchable", true},
		{"claude", "session", "session", "unmatchable", true},
	} {
		t.Run(tc.raw+"/"+tc.legacy, func(t *testing.T) {
			raw := Candidate{ID: "physical", PublicID: tc.raw, Agent: tc.agent, Group: "g", Aliases: []string{tc.raw}, SourceLocal: tc.local}
			legacy := Candidate{ID: tc.legacy, PublicID: tc.legacy, Agent: tc.agent, Machine: "archive"}
			got := Match([]Candidate{raw, legacy})
			require.NotEmpty(t, got)
			assert.Equal(t, tc.want, got[0].Class)
			if tc.want == "raw_only" {
				require.Len(t, got, 2)
				assert.Equal(t, "legacy_only", got[1].Class)
			}
		})
	}
	got := Match([]Candidate{{ID: "r", Agent: "codex", Group: "g", Aliases: []string{"same"}}, {ID: "same", Agent: "claude"}})
	require.Len(t, got, 2)
	assert.Equal(t, "raw_only", got[0].Class)
}

func TestMatchParserIDCaseSensitivity(t *testing.T) {
	raw := Candidate{ID: "physical", PublicID: "Session-A~branch", Agent: "claude", Group: "g", Aliases: []string{"Session-A"}}
	lower := Candidate{ID: "session-a", PublicID: "session-a", Agent: "claude", Machine: "archive"}
	exact := Candidate{ID: "Session-A", PublicID: "Session-A", Agent: "claude", Machine: "archive"}
	for _, tc := range []struct {
		name   string
		legacy Candidate
		want   []MatchResult
	}{
		{
			name: "case_distinct", legacy: lower,
			want: []MatchResult{
				{Class: "raw_only", Raw: []Candidate{raw}},
				{Class: "legacy_only", Legacy: []Candidate{lower}},
			},
		},
		{
			name: "exact_spelling", legacy: exact,
			want: []MatchResult{{Class: "equal", Raw: []Candidate{raw}, Legacy: []Candidate{exact}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Match([]Candidate{raw, tc.legacy}))
		})
	}
}

// The graph oracle walks generated edges instead of rebuilding the matcher's
// alias index or union structure. Each result pins both membership and class.
func TestMatchCandidateGraphProperty(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		rawFlags := hegel.Draw(ht, hegel.Lists(hegel.Booleans()))
		legacyFlags := hegel.Draw(ht, hegel.Lists(hegel.Booleans()))
		// Bound graph materialization, leaving the collection generators unrestricted.
		n, m := min(len(rawFlags), 12), min(len(legacyFlags), 12)
		agent := hegel.Draw(ht, hegel.Text())
		nodes := make([]Candidate, n+m)
		for i := range n {
			nodes[i] = Candidate{ID: fmt.Sprintf("R%d", i), Agent: agent, Group: fmt.Sprintf("G%d", i), SourceLocal: rawFlags[i]}
		}
		for j := range m {
			id := fmt.Sprintf("L%d", j)
			if legacyFlags[j] {
				id = "host~" + id
			}
			nodes[n+j] = Candidate{ID: id, Agent: agent}
			if hegel.Draw(ht, hegel.Booleans()) {
				nodes[n+j].Agent = "other:" + agent
			}
		}
		edges := make([][]int, len(nodes))
		for i := range n {
			for j := range m {
				if !hegel.Draw(ht, hegel.Booleans()) {
					continue
				}
				nodes[i].Aliases = append(nodes[i].Aliases, nodes[n+j].ID)
				if hegel.Draw(ht, hegel.Booleans()) {
					nodes[i].Aliases = append(nodes[i].Aliases, nodes[n+j].ID)
				}
				if !legacyFlags[j] && nodes[i].Agent == nodes[n+j].Agent {
					edges[i] = append(edges[i], n+j)
					edges[n+j] = append(edges[n+j], i)
				}
			}
		}
		var want []string
		visited := make([]bool, len(nodes))
		for start := range nodes {
			if visited[start] {
				continue
			}
			visited[start] = true
			queue := []int{start}
			component := MatchResult{}
			local := false
			for len(queue) > 0 {
				v := queue[0]
				queue = queue[1:]
				if v < n {
					component.Raw = append(component.Raw, nodes[v])
					local = local || rawFlags[v]
				} else {
					component.Legacy = append(component.Legacy, nodes[v])
				}
				for _, next := range edges[v] {
					if !visited[next] {
						visited[next] = true
						queue = append(queue, next)
					}
				}
			}
			component.Class = "equal"
			if len(component.Raw) == 0 {
				component.Class = "legacy_only"
			} else if len(component.Legacy) == 0 {
				component.Class = "raw_only"
			} else if local {
				component.Class = "unmatchable"
			} else if len(component.Raw) != 1 || len(component.Legacy) != 1 {
				component.Class = "ambiguous"
			}
			want = append(want, matchComponentKey(component))
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			slices.Reverse(nodes)
		}
		var got []string
		for _, component := range Match(nodes) {
			got = append(got, matchComponentKey(component))
		}
		assert.ElementsMatch(ht, want, got)
	}, hegel.WithTestCases(1000))
}

func matchComponentKey(c MatchResult) string {
	var ids []string
	for _, raw := range c.Raw {
		ids = append(ids, "raw:"+raw.ID)
	}
	for _, legacy := range c.Legacy {
		ids = append(ids, "legacy:"+legacy.ID)
	}
	slices.Sort(ids)
	return fmt.Sprintf("%s:%q", c.Class, ids)
}

func TestMatchRequiresRecordedAlias(t *testing.T) {
	for _, local := range []bool{false, true} {
		rows := []Candidate{
			{ID: "physical", PublicID: "codex:thread", Agent: "codex", Group: "g", SourceLocal: local},
			{ID: "codex:thread", Agent: "codex"},
		}
		got := Match(rows)
		require.Len(t, got, 2)
		assert.Equal(t, "raw_only", got[0].Class)
		assert.Equal(t, "legacy_only", got[1].Class)
	}
}
