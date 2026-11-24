package activity

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLaunchers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runs      []SessionMeta
		ancestors map[string]Launcher
		calls     [][]string
		examined  []string
		waiting   []string
	}{
		{
			// A cycle exercises the defensive guard against malformed ancestry.
			name:      "cycle",
			runs:      []SessionMeta{{SessionID: "child", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"b"}}, {SessionID: "other", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"missing"}}},
			ancestors: map[string]Launcher{"b": {ID: "b", IsSubagent: true, ParentSessionIDs: []string{"a"}}, "a": {ID: "a", IsSubagent: true, ParentSessionIDs: []string{"b"}}},
			calls:     [][]string{{"b", "missing"}, {"a"}}, examined: []string{"a", "b", "missing"},
		},
		{
			name:      "shared parent",
			runs:      []SessionMeta{{SessionID: "child", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"middle", "second"}}},
			ancestors: map[string]Launcher{"middle": {ID: "middle", IsSubagent: true, ParentSessionIDs: []string{"first", "second"}}, "first": {ID: "first", MatchesFilter: true}, "second": {ID: "second", MatchesFilter: true}},
			waiting:   []string{"first", "second"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			got, err := ResolveLaunchers(t.Context(), tc.runs, nil, func(_ context.Context, ids []string) ([]Launcher, error) {
				calls = append(calls, ids)
				var rows []Launcher
				for _, id := range ids {
					if row, ok := tc.ancestors[id]; ok {
						rows = append(rows, row)
					}
				}
				return rows, nil
			})
			require.NoError(t, err)
			if tc.calls != nil {
				assert.Equal(t, tc.calls, calls)
				assert.Equal(t, tc.examined, got.Examined)
			}
			if tc.waiting != nil {
				assert.ElementsMatch(t, tc.waiting, got.Waiting["child"])
			}
		})
	}
}

func TestResolveLaunchersUsesKnownRunsAndCountsInteractiveAncestors(t *testing.T) {
	for _, launched := range []bool{true, false} {
		t.Run(fmt.Sprintf("middle launched=%t", launched), func(t *testing.T) {
			runs := []SessionMeta{
				{SessionID: "root"},
				{SessionID: "middle", IsSubagent: true, IsLaunched: launched, ParentSessionIDs: []string{"root"}},
				{SessionID: "child", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"middle"}},
			}
			got, err := ResolveLaunchers(t.Context(), runs, map[string]bool{"root": true, "child": true}, func(context.Context, []string) ([]Launcher, error) {
				require.FailNow(t, "known ancestors must not be loaded again")
				return nil, nil
			})
			require.NoError(t, err)
			assert.Equal(t, []string{"root"}, got.Waiting["child"])
			if launched {
				assert.Equal(t, []string{"root"}, got.Waiting["middle"])
			} else {
				assert.Empty(t, got.Waiting["middle"])
			}
			assert.Equal(t, []string{"middle", "root"}, got.Examined)
		})
	}
}

func TestResolveLaunchersSkipsNonInteractive(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []SessionMeta
	}{
		{name: "automated", runs: []SessionMeta{{SessionID: "parent", IsAutomated: true}, {SessionID: "run", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"parent"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := make(map[string]bool)
			for _, run := range tc.runs {
				selected[run.SessionID] = true
			}
			got, err := ResolveLaunchers(t.Context(), tc.runs, selected, func(context.Context, []string) ([]Launcher, error) {
				require.FailNow(t, "unexpected ancestor lookup")
				return nil, nil
			})
			require.NoError(t, err)
			assert.Empty(t, got.Waiting["run"])
		})
	}
}
