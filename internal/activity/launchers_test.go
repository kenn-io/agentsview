package activity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLaunchers(t *testing.T) {
	// A cycle exercises the defensive guard against malformed ancestry.
	var calls [][]string
	got, err := ResolveLaunchers(t.Context(), []SessionMeta{{SessionID: "child", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"b"}}, {SessionID: "other", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"missing"}}}, nil, func(_ context.Context, ids []string) ([]Launcher, error) {
		calls = append(calls, ids)
		switch ids[0] {
		case "b":
			return []Launcher{{ID: "b", IsSubagent: true, ParentSessionIDs: []string{"a"}}}, nil
		case "a":
			return []Launcher{{ID: "a", IsSubagent: true, ParentSessionIDs: []string{"b"}}}, nil
		}
		return nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"b", "missing"}, {"a"}}, calls)
	assert.Equal(t, []string{"a", "b", "missing"}, got.Examined)
}

func TestResolveLaunchersFollowsEveryParentOnce(t *testing.T) {
	runs := []SessionMeta{{SessionID: "child", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"middle", "second"}}}
	got, err := ResolveLaunchers(t.Context(), runs, nil, func(_ context.Context, ids []string) ([]Launcher, error) {
		var rows []Launcher
		for _, id := range ids {
			switch id {
			case "middle":
				rows = append(rows, Launcher{ID: id, IsSubagent: true, ParentSessionIDs: []string{"first", "child", "second"}})
			case "first", "second":
				rows = append(rows, Launcher{ID: id, MatchesFilter: true})
			}
		}
		return rows, nil
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"first", "second"}, got.Waiting["child"])
}

func TestResolveLaunchersUsesKnownRunsAndCountsInteractiveAncestors(t *testing.T) {
	runs := []SessionMeta{
		{SessionID: "root"},
		{SessionID: "middle", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"root"}},
		{SessionID: "child", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"middle"}},
	}
	got, err := ResolveLaunchers(t.Context(), runs, map[string]bool{"root": true, "child": true}, func(context.Context, []string) ([]Launcher, error) {
		require.FailNow(t, "known ancestors must not be loaded again")
		return nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"root"}, got.Waiting["child"])
	assert.Equal(t, []string{"root"}, got.Waiting["middle"])
	assert.Equal(t, []string{"middle", "root"}, got.Examined)
}

func TestResolveLaunchersSkipsNonInteractive(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []SessionMeta
	}{
		{name: "automated", runs: []SessionMeta{{SessionID: "parent", IsAutomated: true}, {SessionID: "run", IsSubagent: true, ParentSessionIDs: []string{"parent"}}}},
		{name: "fork", runs: []SessionMeta{{SessionID: "run", ParentSessionIDs: []string{"parent"}}}},
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

func TestResolveLaunchersTraversesNativeAncestorsWithoutCountingThem(t *testing.T) {
	runs := []SessionMeta{
		{SessionID: "root"},
		{SessionID: "native", IsSubagent: true, ParentSessionIDs: []string{"root"}},
		{SessionID: "launched", IsSubagent: true, IsLaunched: true, ParentSessionIDs: []string{"native"}},
	}
	got, err := ResolveLaunchers(t.Context(), runs, map[string]bool{"root": true}, func(context.Context, []string) ([]Launcher, error) {
		require.FailNow(t, "known ancestors must not be loaded again")
		return nil, nil
	})
	require.NoError(t, err)
	assert.Empty(t, got.Waiting["native"])
	assert.Equal(t, []string{"root"}, got.Waiting["launched"])
}
