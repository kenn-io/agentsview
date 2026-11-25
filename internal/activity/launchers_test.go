package activity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLaunchersUsesKnownRunsAndCountsInteractiveAncestors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runs     []SessionMeta
		selected map[string]bool
		waiting  map[string]string
		examined []string
	}{
		{name: "interactive", runs: []SessionMeta{{SessionID: "root"}, {SessionID: "middle", IsSubagent: true, IsLaunched: true, ParentSessionID: "root"}, {SessionID: "child", IsSubagent: true, IsLaunched: true, ParentSessionID: "middle"}}, selected: map[string]bool{"root": true, "child": true}, waiting: map[string]string{"child": "root", "middle": "root"}, examined: []string{"middle", "root"}},
		{name: "automated", runs: []SessionMeta{{SessionID: "parent", IsAutomated: true}, {SessionID: "run", IsSubagent: true, IsLaunched: true, ParentSessionID: "parent"}}, selected: map[string]bool{"parent": true, "run": true}, waiting: map[string]string{}, examined: []string{"parent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveLaunchers(t.Context(), tc.runs, tc.selected, func(context.Context, []string) ([]Launcher, error) {
				require.FailNow(t, "known ancestors must not be loaded again")
				return nil, nil
			})
			require.NoError(t, err)
			assert.Equal(t, tc.waiting, got.Waiting)
			assert.Equal(t, tc.examined, got.Examined)
		})
	}
}
