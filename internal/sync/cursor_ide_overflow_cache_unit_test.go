package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

// TestRetainedCursorIDEContainerSkipEntriesKeepsOnlyWholeContainers is a
// focused unit test for the selectivity clearWatcherOverflowCaches relies on:
// of a mixed skip cache (a Cursor IDE whole-container entry, a Cursor IDE
// composer-member entry, and an unrelated agent's entry), only the
// whole-container entry -- and its parallel fingerprint -- survives. This is
// case (c) from the overflow-keep-cursor-container-cache fix, exercised
// directly against the selection predicate rather than through a full sync,
// because not every non-container provider populates the plain skip cache on
// an ordinary first sync (for example Claude's incremental-append path),
// which would make an end-to-end assertion about "cleared" brittle.
func TestRetainedCursorIDEContainerSkipEntriesKeepsOnlyWholeContainers(t *testing.T) {
	containerPath := "/root/cursor/state.vscdb"
	memberPath := containerPath + "#composer-1"
	claudePath := "/root/claude/session.jsonl"

	containerKey := providerAgentSkipCacheKey(containerPath, parser.AgentCursorIDE) +
		"?source_hash=containerhash&data_version=1"
	memberKey := providerAgentSkipCacheKey(memberPath, parser.AgentCursorIDE) +
		"?source_hash=memberhash"
	claudeKey := providerAgentSkipCacheKey(claudePath, parser.AgentClaude)

	cache := map[string]int64{
		containerKey: 100,
		memberKey:    200,
		claudeKey:    300,
	}
	fingerprints := map[string]string{
		containerKey: "fp-container",
		memberKey:    "fp-member",
		claudeKey:    "fp-claude",
	}

	retainedCache, retainedFingerprints := retainedCursorIDEContainerSkipEntries(
		cache, fingerprints,
	)

	assert.Equal(t, map[string]int64{containerKey: 100}, retainedCache,
		"only the whole-container entry must survive")
	assert.Equal(t, map[string]string{containerKey: "fp-container"}, retainedFingerprints,
		"only the whole-container fingerprint must survive")
	// The inputs must not be mutated in place; clearWatcherOverflowCaches
	// assigns the returned maps directly onto a fresh e.skipCache /
	// e.skipFingerprints rather than reusing the pre-overflow ones.
	assert.Len(t, cache, 3)
	assert.Len(t, fingerprints, 3)
}

// TestCursorIDEContainerSkipCacheKey exercises the key-shape predicate on its
// own, independent of a real skip cache, so the agent/path parsing it does is
// covered even if a future change to retainedCursorIDEContainerSkipEntries
// stops exercising every shape.
func TestCursorIDEContainerSkipCacheKey(t *testing.T) {
	containerPath := "/root/cursor/state.vscdb"
	memberPath := containerPath + "#composer-1"

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{
			name: "whole container with hash and data version",
			key: providerAgentSkipCacheKey(containerPath, parser.AgentCursorIDE) +
				"?source_hash=abc&data_version=1",
			want: true,
		},
		{
			name: "composer member",
			key: providerAgentSkipCacheKey(memberPath, parser.AgentCursorIDE) +
				"?source_hash=abc",
			want: false,
		},
		{
			name: "no agent qualifier at all",
			key:  containerPath,
			want: false,
		},
		{
			name: "same base path but a different agent",
			key: providerAgentSkipCacheKey(containerPath, parser.AgentOmnigent) +
				"?source_hash=abc&data_version=1",
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cursorIDEContainerSkipCacheKey(tc.key))
		})
	}
}

// TestCursorIDEContainerCandidateSource exercises the predicate
// rehydrateReconciliationPage uses to decide whether a reconciliation
// candidate is exempt from the lost-events ForceParse.
func TestCursorIDEContainerCandidateSource(t *testing.T) {
	containerSource := parser.SourceRef{
		Provider: parser.AgentCursorIDE, DisplayPath: "/root/cursor/state.vscdb",
	}
	memberSource := parser.SourceRef{
		Provider:    parser.AgentCursorIDE,
		DisplayPath: "/root/cursor/state.vscdb#composer-1",
	}
	claudeSource := parser.SourceRef{
		Provider: parser.AgentClaude, DisplayPath: "/root/claude/session.jsonl",
	}

	assert.True(t, cursorIDEContainerCandidateSource(parser.AgentCursorIDE, containerSource))
	assert.False(t, cursorIDEContainerCandidateSource(parser.AgentCursorIDE, memberSource),
		"a composer member must still be forced")
	assert.False(t, cursorIDEContainerCandidateSource(parser.AgentClaude, claudeSource),
		"a non-Cursor-IDE source must still be forced")
}

// TestReconcileWatchRootsAfterLostEventsStillForcesNonCursorIDESource covers
// case (c) end to end: a non-Cursor-IDE source is still fully force-parsed by
// a lost-events reconcile, so the retained-cache behavior above does not leak
// into any other provider. It reuses the directStreamingProvider fixture
// (see engine_test.go), whose Parse records the ForceParse flag it actually
// received, which is the one thing an outside observer cannot otherwise see:
// unlike Cursor IDE containers, most non-container providers (for example
// Claude's incremental-append path) never populate the plain skip cache on
// an ordinary sync, so a skip-cache-only assertion would not be reliable
// here.
func TestReconcileWatchRootsAfterLostEventsStillForcesNonCursorIDESource(t *testing.T) {
	const agent parser.AgentType = "lost-events-force-probe"
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	source := parser.SourceRef{
		Provider: agent, Key: path, DisplayPath: path, FingerprintKey: path,
	}
	started := time.Unix(1704067200, 0)
	provider := &directStreamingProvider{
		Def: parser.AgentDef{Type: agent, FileBased: true},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources:    parser.CapabilitySupported,
			StreamingDiscovery: parser.CapabilitySupported,
			WatchSources:       parser.CapabilitySupported,
			FindSource:         parser.CapabilitySupported,
		}},
		source:      &source,
		fingerprint: parser.SourceFingerprint{Key: path, MTimeNS: 1},
		parseOutcome: parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{{
				Result: parser.ParseResult{Session: parser.ParsedSession{
					ID: "probe-session", Agent: agent, Project: "project",
					Machine: "local", StartedAt: started, EndedAt: started,
					File: parser.FileInfo{Path: path},
				}},
				DataVersion: parser.DataVersionCurrent,
			}},
			ResultSetComplete: true,
		},
	}
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			directStreamingFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))

	assert.GreaterOrEqual(t, int(provider.parseCalls.Load()), 1,
		"lost-events recovery must still reach a non-Cursor-IDE source")
	assert.True(t, provider.parseForce.Load(),
		"a non-Cursor-IDE source must still be force-parsed on lost events")
	active, err := database.GetSession(t.Context(), "probe-session")
	require.NoError(t, err)
	assert.NotNil(t, active)
}
