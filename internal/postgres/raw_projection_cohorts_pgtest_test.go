//go:build pgtest

package postgres

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawsync"
	"slices"
	"testing"
)

func TestRawProjectionThreeSourceCohortNamesAndPinNotes(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			f := newProjectionFixture(t)
			devices := []string{"device-a", "device-b", "device-c"}
			if reverse {
				slices.Reverse(devices)
			}
			manifests := map[string]rawsync.CanonicalManifest{}
			receipts := map[string]string{}
			byAlias := map[string]string{}
			for _, device := range devices {
				m, r := f.accept(t, device, "initial-"+device, "")
				out := projectionOutcome("shared pin")
				out.Outcome.Results[0].Result.Messages[1].Content = device
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
				manifests[device] = m
				receipts[device] = r.Receipt
				byAlias[f.alias(t, m)] = device
			}
			aliases := make([]string, 0, len(byAlias))
			for alias := range byAlias {
				aliases = append(aliases, alias)
			}
			slices.Sort(aliases)
			a, b, c := aliases[0], aliases[1], aliases[2]
			for i, alias := range aliases {
				require.NoError(t, f.sink.SetCuration(t.Context(), alias, "display_name", []string{"alpha", "beta", "gamma"}[i]))
				require.NoError(t, f.sink.SetPin(t.Context(), alias, 0, true, []string{"one", "two", "three"}[i]))
			}
			require.NoError(t, f.sink.SetCuration(t.Context(), c, "starred", true))
			for _, alias := range []string{a, b} {
				device := byAlias[alias]
				m, r := f.accept(t, device, "equal-"+device, receipts[device])
				receipts[device] = r.Receipt
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("shared pin")))
			}
			ar, err := f.sink.Resolve(t.Context(), a)
			require.NoError(t, err)
			br, err := f.sink.Resolve(t.Context(), b)
			require.NoError(t, err)
			assert.Equal(t, ar.SessionID, br.SessionID)
			store := &Store{pg: f.runtime}
			session, err := store.GetSession(t.Context(), ar.SessionID)
			require.NoError(t, err)
			require.NotNil(t, session.DisplayName)
			assert.Equal(t, "alpha", *session.DisplayName)
			pins, err := store.ListPinnedMessages(t.Context(), ar.SessionID, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			require.NotNil(t, pins[0].Note)
			assert.Equal(t, "one", *pins[0].Note)
			require.NoError(t, f.sink.SetCuration(t.Context(), b, "display_name", "cohort"))
			require.NoError(t, f.sink.SetPin(t.Context(), b, 0, false, ""))
			cr, err := f.sink.Resolve(t.Context(), c)
			require.NoError(t, err)
			pins, err = store.ListPinnedMessages(t.Context(), cr.SessionID, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, "three", *pins[0].Note)
			stars, err := store.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []string{cr.SessionID}, stars)
			device := byAlias[a]
			m, _ := f.accept(t, device, "split", receipts[device])
			out := projectionOutcome("shared pin")
			out.Outcome.Results[0].Result.Messages[1].Content = "split"
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
			for _, alias := range []string{a, b} {
				r, err := f.sink.Resolve(t.Context(), alias)
				require.NoError(t, err)
				session, err := store.GetSession(t.Context(), r.SessionID)
				require.NoError(t, err)
				require.NotNil(t, session.DisplayName)
				assert.Equal(t, "cohort", *session.DisplayName)
				pins, err := store.ListPinnedMessages(t.Context(), r.SessionID, "")
				require.NoError(t, err)
				assert.Empty(t, pins)
			}
		})
	}
}
