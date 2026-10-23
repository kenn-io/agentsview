package extract

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"hegel.dev/go/hegel"
)

func TestHostedRawExtractionNeverCallsModel(t *testing.T) {
	database := newTestArchive(t)
	seedSession(t, database, "fixture", turnMessages("fix the bug", "done"), nil)
	server, calls := modelServer(t, alwaysEntries(t, "fixture"))
	manager := newManager(t, database, server.URL, nil)
	database.DisableLocalDerivation()
	hegel.Test(t, func(ht *hegel.T) {
		opts := PassOptions{Full: hegel.Draw(ht, hegel.Booleans()), Limit: hegel.Draw(ht, hegel.Integers(math.MinInt, math.MaxInt))}
		if hegel.Draw(ht, hegel.Booleans()) {
			opts.SessionID = "fixture"
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			_, err := manager.RunPass(t.Context(), opts)
			require.ErrorIs(ht, err, config.ErrHostedRawMode)
		} else {
			_, _, err := manager.TryPass(t.Context(), opts)
			require.ErrorIs(ht, err, config.ErrHostedRawMode)
		}
		require.Zero(ht, calls.count())
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestHostedRawTryPassZeroOptionsRegression(t *testing.T) {
	database := newTestArchive(t)
	server, calls := modelServer(t, alwaysEntries(t, "fixture"))
	manager := newManager(t, database, server.URL, nil)
	database.DisableLocalDerivation()
	_, _, err := manager.TryPass(t.Context(), PassOptions{})
	require.ErrorIs(t, err, config.ErrHostedRawMode)
	require.Zero(t, calls.count())
}
