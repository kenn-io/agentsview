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
	got, err := ResolveLaunchers(t.Context(), []SessionMeta{{SessionID: "child", IsSubagent: true, ParentSessionID: "b"}, {SessionID: "other", IsSubagent: true, ParentSessionID: "missing"}}, nil, func(_ context.Context, ids []string) ([]Launcher, error) {
		calls = append(calls, ids)
		switch ids[0] {
		case "b":
			return []Launcher{{ID: "b", IsSubagent: true, ParentSessionID: "a"}}, nil
		case "a":
			return []Launcher{{ID: "a", IsSubagent: true, ParentSessionID: "b"}}, nil
		}
		return nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"b", "missing"}, {"a"}}, calls)
	assert.Equal(t, []string{"a", "b", "missing"}, got.Examined)
}

func TestResolveLaunchersUsesKnownRunsAndCountsInteractiveAncestors(t *testing.T) {
	runs := []SessionMeta{
		{SessionID: "root"},
		{SessionID: "middle", IsSubagent: true, ParentSessionID: "root"},
		{SessionID: "child", IsSubagent: true, ParentSessionID: "middle"},
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
		{name: "automated", runs: []SessionMeta{{SessionID: "parent", IsAutomated: true}, {SessionID: "run", IsSubagent: true, ParentSessionID: "parent"}}},
		{name: "fork", runs: []SessionMeta{{SessionID: "parent"}, {SessionID: "run", ParentSessionID: "parent"}}},
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
