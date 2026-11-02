package vector

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"hegel.dev/go/hegel"
)

func TestHostedRawRefusesEveryBuildMode(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		var encoded atomic.Int64
		m := NewManager(nil, twoDocSource(), soloEncoders(countingEncoder(&encoded)), fakeGeneration("fake-model"))
		m.DisableLocalDerivation()
		req := BuildRequest{FullRebuild: hegel.Draw(ht, hegel.Booleans()), Backstop: hegel.Draw(ht, hegel.Booleans()), IncludeAutomated: hegel.Draw(ht, hegel.Booleans())}
		if hegel.Draw(ht, hegel.Booleans()) {
			require.ErrorIs(ht, m.StartBuild(req), config.ErrHostedRawMode)
		} else {
			ran, err := m.TryBuild(t.Context(), req)
			require.ErrorIs(ht, err, config.ErrHostedRawMode)
			require.False(ht, ran)
		}
		require.Zero(ht, encoded.Load())
		require.Zero(ht, m.Status().BuildID)
		m.Shutdown()
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}
