package activity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLaunchersUsesKnownRunsAndCountsInteractiveAncestors(t *testing.T) {
	runs := []SessionMeta{
		{SessionID: "root"},
		{SessionID: "middle", IsSubagent: true, IsLaunched: true, ParentSessionID: "root"},
		{SessionID: "child", IsSubagent: true, IsLaunched: true, ParentSessionID: "middle"},
	}
	got, err := ResolveLaunchers(t.Context(), runs, map[string]bool{"root": true, "child": true}, func(context.Context, []string) ([]Launcher, error) {
		require.FailNow(t, "known ancestors must not be loaded again")
		return nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "root", got.Waiting["child"])
	assert.Equal(t, "root", got.Waiting["middle"])
	assert.Equal(t, []string{"middle", "root"}, got.Examined)
}

func TestResolveLaunchersSkipsNonInteractive(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []SessionMeta
	}{
		{name: "automated", runs: []SessionMeta{{SessionID: "parent", IsAutomated: true}, {SessionID: "run", IsSubagent: true, IsLaunched: true, ParentSessionID: "parent"}}},
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
