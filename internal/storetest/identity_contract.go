package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

// IdentityStore is the source-scoped Task 6 identity-read slice.
type IdentityStore interface {
	ListProjectIdentityObservations(
		context.Context, []string,
	) ([]export.ProjectIdentityObservation, error)
	BuildProjectIdentityMap(
		context.Context, []string,
	) (map[string]export.ProjectMapEntry, error)
}

// IdentityBackend registers one embedded BunStore and its fixture setup.
type IdentityBackend struct {
	Name string
	Open func(*testing.T) (IdentityStore, IdentityFixture)
}

// RunIdentityContract verifies source-scoped identity behavior against one
// shared BunStore rather than a concrete backend shadow.
func RunIdentityContract(t *testing.T, backend IdentityBackend) {
	t.Helper()
	t.Run(backend.Name, func(t *testing.T) {
		store, fixture := backend.Open(t)
		ctx := t.Context()

		observations, err := store.ListProjectIdentityObservations(ctx, nil)
		require.NoError(t, err)
		require.Len(t, observations, 2)
		assert.LessOrEqual(t,
			observations[0].SourceArchiveID, observations[1].SourceArchiveID,
			"observations are ordered by source archive before project fields",
		)
		byProject := make(map[string]export.ProjectIdentityObservation, len(observations))
		for _, observation := range observations {
			byProject[observation.Project] = observation
		}
		alphaObservation := byProject[fixture.AlphaProject]
		betaObservation := byProject[fixture.BetaProject]
		assert.Equal(t, fixture.ArchiveAID, alphaObservation.SourceArchiveID)
		assert.Equal(t, "identity-host-a", alphaObservation.Machine)
		assert.Equal(t, "example.com/org/alpha", alphaObservation.Key)
		assert.Equal(t,
			time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
			alphaObservation.ObservedAt,
		)
		assert.Equal(t, fixture.ArchiveBID, betaObservation.SourceArchiveID)
		assert.Equal(t, "example.com/org/beta", betaObservation.Key)

		filtered, err := store.ListProjectIdentityObservations(ctx, []string{
			fixture.BetaProject, fixture.AlphaProject, fixture.BetaProject,
		})
		require.NoError(t, err)
		require.Len(t, filtered, 2)
		assert.LessOrEqual(t, filtered[0].SourceArchiveID, filtered[1].SourceArchiveID)
		assert.ElementsMatch(t,
			[]string{fixture.AlphaProject, fixture.BetaProject},
			[]string{filtered[0].Project, filtered[1].Project},
		)
		empty, err := store.ListProjectIdentityObservations(ctx, []string{})
		require.NoError(t, err)
		assert.NotNil(t, empty)
		assert.Empty(t, empty)

		projects, err := store.BuildProjectIdentityMap(ctx, []string{
			fixture.AlphaProject, fixture.BetaProject,
		})
		require.NoError(t, err)
		require.Len(t, projects, 2)
		alpha := projects[fixture.AlphaProject]
		beta := projects[fixture.BetaProject]
		assert.Equal(t, export.ProjectResolutionResolved, alpha.Resolution)
		require.NotNil(t, alpha.Identity)
		assert.Equal(t,
			"p1:sha256:abf74ae11cde456fe1c50f37d8cd5461bd5569bd7ee428afccbcf2729bcfeded",
			alpha.Identity.Key,
		)
		assert.NotEmpty(t, alpha.ProjectKey)
		assert.Equal(t, export.ProjectResolutionResolved, beta.Resolution)
		require.NotNil(t, beta.Identity)
		assert.Equal(t,
			"p1:sha256:bd94c930bc2681f36138b7cae21ac4ea3575d178231582cc70ae2e6f3552407e",
			beta.Identity.Key,
		)
		assert.NotEmpty(t, beta.ProjectKey)
		assert.NotEqual(t, alpha.ProjectKey, beta.ProjectKey)
	})
}
