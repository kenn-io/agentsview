package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestProviderSyncSemanticsDeclarations verifies that every registered
// provider's declared Capabilities().Sync matches the engine's historical
// per-agent sync behavior. Providers not listed in wantSync are expected to
// carry the zero-value ProviderSyncSemantics.
func TestProviderSyncSemanticsDeclarations(t *testing.T) {
	wantSync := map[AgentType]ProviderSyncSemantics{
		AgentClaude: {
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
			SkipCacheFreshWithoutStoredRow:      true,
		},
		AgentCodex: {
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
			SkipCacheFreshWithoutStoredRow:      true,
		},
		AgentDevin: {
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
		AgentQoder: {
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
		AgentWindsurf: {
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
		AgentHermes: {
			FingerprintHashRequiredForFreshness: true,
		},
		AgentGemini: {
			FingerprintHashRequiredForFreshness: true,
		},
		AgentZed: {
			UnchangedResults: UnchangedResultMTime,
		},
		AgentKiro: {
			UnchangedResults: UnchangedResultMTime,
		},
		AgentTrae: {
			UnchangedResults: UnchangedResultMTimeAndHash,
		},
		AgentAider: {
			UnchangedResults: UnchangedResultMTimeAndHash,
		},
		AgentShelley: {
			UnchangedResults: UnchangedResultMTimeAndHash,
		},
		AgentOpenCode: {
			UnchangedResults: UnchangedResultMTimeAndHash,
		},
		AgentKilo: {
			UnchangedResults: UnchangedResultMTimeAndHash,
		},
		AgentMiMoCode: {
			UnchangedResults: UnchangedResultMTimeAndHash,
		},
		AgentIcodemate: {
			UnchangedResults: UnchangedResultMTimeAndHash,
		},
		AgentOmnigent: {
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
			UnchangedResults:                    UnchangedResultMTimeAndHash,
		},
	}

	for _, factory := range ProviderFactories() {
		agent := factory.Definition().Type
		t.Run(string(agent), func(t *testing.T) {
			assert.Equal(t, wantSync[agent], factory.Capabilities().Sync)
		})
	}
}
