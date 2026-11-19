package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/growthtest"
)

func TestActiveAppendGrowth(t *testing.T) {
	growthtest.Run(t, growthtest.Check{
		Small: 1000,
		Large: 10000,
		Build: func(tb testing.TB, size int) (run, reset func()) {
			tb.Helper()
			o := options{Sessions: size, Turns: 4, Active: 8, ContentBytes: 512}
			var r report
			sources, database, engine, err := r.ingest(tb.Context(), o, tb.TempDir())
			require.NoError(tb, err)
			tb.Cleanup(func() {
				engine.Close()
				require.NoError(tb, database.Close())
			})
			paths := make([]string, o.Active)
			for i := range o.Active {
				paths[i] = sources[i].Path
			}
			return func() {
					require.NoError(tb, engine.SyncPathsContext(tb.Context(), paths))
				}, func() {
					for i := range o.Active {
						require.NoError(tb, sources[i].appendTurns(tb.Context(), 1, o.ContentBytes))
					}
				}
		},
		Measure:  growthtest.Fastest(9),
		MaxRatio: 2,
		Floor:    1e6,
	})
}
