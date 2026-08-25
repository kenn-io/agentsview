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
		// TraeX shares the Codex provider, so it must share its semantics.
		AgentTraeX: {
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
		AgentGoose: {
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
		AgentZed: {
			UnchangedResults: UnchangedResultMTime,
		},
		AgentKiro: {
			UnchangedResults:                    UnchangedResultMTimeAndHash,
			FingerprintHashRequiredForFreshness: true,
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
		// The OpenCode family shares one physical container per root, so
		// freshness consults the per-session child digest: it is the only
		// signal that sees a deleted message or part, which a MAX over
		// timestamps cannot. Containers without composite support emit an
		// empty hash, which the gate treats as no constraint.
		AgentOpenCode: {
			UnchangedResults:                    UnchangedResultMTimeAndHash,
			FingerprintHashRequiredForFreshness: true,
		},
		AgentKilo: {
			UnchangedResults:                    UnchangedResultMTimeAndHash,
			FingerprintHashRequiredForFreshness: true,
		},
		AgentMiMoCode: {
			UnchangedResults:                    UnchangedResultMTimeAndHash,
			FingerprintHashRequiredForFreshness: true,
		},
		AgentIcodemate: {
			UnchangedResults:                    UnchangedResultMTimeAndHash,
			FingerprintHashRequiredForFreshness: true,
		},
		AgentOmnigent: {
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
			UnchangedResults:                    UnchangedResultMTimeAndHash,
		},
		// Codebuff requires the per-component stat-hash digest (persisted in
		// the provider_freshness side-table) before a warm pass may consider a
		// source fresh; the side-table row is the only signal that sees
		// companion-file rewrites, offsetting size deltas, and sibling-only
		// directory mutations.
		AgentCodebuff: {
			FingerprintHashRequiredForFreshness: true,
		},
	}

	for _, factory := range ProviderFactories() {
		agent := factory.Definition().Type
		t.Run(string(agent), func(t *testing.T) {
			assert.Equal(t, wantSync[agent], factory.Capabilities().Sync)
		})
	}
}
