package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// TestDiscoverProviderSourcesDiscardsNonHermesIncompleteDiscovery proves
// this PR's "use the partial sources on DiscoveryIncompleteError" branch in
// discoverProviderSources stays scoped to Hermes and Augure Desktop. Every
// other provider -- including Aider and the raw-capture-backed providers,
// which already return DiscoveryIncompleteError from real discovery on
// main -- must keep the prior behavior: any discovery error, including an
// incomplete one, discards that pass's partial sources for the agent
// entirely instead of writing them.
//
// Reuses changedPathPlanProvider/changedPathPlanFactory (changed_path_plan_test.go),
// which already returns configurable sources alongside a discovery error,
// instead of a second discovery-fixture provider.
func TestDiscoverProviderSourcesDiscardsNonHermesIncompleteDiscovery(t *testing.T) {
	root := t.TempDir()
	path, fingerprint := writeProcessProviderSource(t, root, "fixture.jsonl")
	source := processFixtureSource(path)
	outcome := parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{
			Result: processFixtureResult(
				"cowork:fixture", parser.AgentCowork, "fixture-project",
				path, fingerprint,
			),
		}},
		ResultSetComplete: true,
	}
	factory := changedPathPlanFactory{
		agent: parser.AgentCowork,
		caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources: parser.CapabilitySupported,
		}},
	}
	factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			discovered: []parser.SourceRef{source},
			discoverErr: parser.DiscoveryIncompleteError{
				Provider: parser.AgentCowork, Reason: "test budget exceeded",
			},
			outcomes: map[string]parser.ParseOutcome{source.Key: outcome},
			fingerprints: map[string]parser.SourceFingerprint{
				source.Key: fingerprint,
			},
		}
	}

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs:         map[parser.AgentType][]string{parser.AgentCowork: {root}},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncAll(t.Context(), nil)

	assert.Positive(t, stats.Failed,
		"an incomplete discovery for a non-Hermes agent must still count as a failure")
	assert.Zero(t, stats.Synced,
		"the partial source must not be written for a non-Hermes agent")
	stored, err := database.GetSession(t.Context(), "cowork:fixture")
	require.NoError(t, err)
	assert.Nil(t, stored,
		"the partial source's session must be discarded, not written")
}
