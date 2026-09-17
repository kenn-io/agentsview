package sync

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestProviderParseFailureUsesStickySkipCache(t *testing.T) {
	const agent parser.AgentType = "sticky-failure"

	_, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("malformed source")
	file := parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	}

	first := engine.processFile(t.Context(), file)
	require.Error(t, first.err)
	assert.True(t, first.cacheFailure)
	engine.cacheFailure(first.failureCacheKey, first.failureMtime)

	second := engine.processFile(t.Context(), file)
	require.Error(t, second.err)
	assert.True(t, second.cachedFailure)
	assert.Equal(t, int32(1), provider.parseCalls.Load())

	updated := info.ModTime().Add(time.Second)
	require.NoError(t, os.Chtimes(path, updated, updated))
	provider.fingerprint.MTimeNS = updated.UnixNano()
	third := engine.processFile(t.Context(), file)
	require.Error(t, third.err)
	assert.False(t, third.cachedFailure)
	assert.Equal(t, int32(2), provider.parseCalls.Load())

	engine.cacheFailure(third.failureCacheKey, third.failureMtime)
	file.ForceParse = true
	forced := engine.processFile(t.Context(), file)
	require.Error(t, forced.err)
	assert.False(t, forced.cachedFailure)
	assert.Equal(t, int32(3), provider.parseCalls.Load())
}

func TestAiderParseFailureDoesNotUseMtimeFailureCache(t *testing.T) {
	const agent parser.AgentType = parser.AgentAider

	_, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("malformed source")
	file := parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	}

	first := engine.processFile(t.Context(), file)
	require.Error(t, first.err)
	assert.False(t, first.cacheFailure)
	if first.cacheFailure {
		engine.cacheFailure(first.failureCacheKey, first.failureMtime)
	}

	mtime := info.ModTime()
	provider.parseErr = nil
	require.NoError(t, os.WriteFile(path, []byte("rewritten"), 0o600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	second := engine.processFile(t.Context(), file)
	require.NoError(t, second.err)
	assert.False(t, second.cachedFailure)
	assert.Equal(t, int32(2), provider.parseCalls.Load())
}

func TestChangedPathSyncPersistsProviderFailure(t *testing.T) {
	const agent parser.AgentType = "changed-path-failure"

	_, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("changed-path parse failure")

	require.Error(t, engine.SyncPathsContext(t.Context(), []string{path}))
	stored, ok := engine.SnapshotSkipCache()[providerAgentSkipCacheKey(path, agent)]
	require.True(t, ok)
	_, failure := decodeSkipFailureMtime(stored)
	assert.True(t, failure)
}

func TestFailedReconciliationPersistsProviderFailure(t *testing.T) {
	const agent parser.AgentType = "reconcile-failure"

	database, engine, provider, root, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("malformed source")

	require.Error(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))
	persisted, err := database.LoadSkippedFiles()
	require.NoError(t, err)
	value, ok := persisted[providerAgentSkipCacheKey(path, agent)]
	require.True(t, ok)
	_, failure := decodeSkipFailureMtime(value)
	assert.True(t, failure)
}

func TestMissingSourceFailureUsesSentinelUntilSourceAppears(t *testing.T) {
	const agent parser.AgentType = "missing-failure"

	_, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome {
			return parser.ParseOutcome{ResultSetComplete: true}
		},
	)
	require.NoError(t, os.Remove(path))
	provider.fingerprintErr = os.ErrNotExist
	file := parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	}

	first := engine.processFile(t.Context(), file)
	require.Error(t, first.err)
	assert.Equal(t, int64(0), first.failureMtime)
	engine.cacheFailure(first.failureCacheKey, first.failureMtime)

	second := engine.processFile(t.Context(), file)
	require.Error(t, second.err)
	assert.True(t, second.cachedFailure)
	assert.Equal(t, int32(0), provider.parseCalls.Load())

	file.ForceParse = true
	forced := engine.processFile(t.Context(), file)
	require.Error(t, forced.err)
	assert.False(t, forced.cachedFailure)
	file.ForceParse = false

	require.NoError(t, os.WriteFile(path, []byte("source"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprintErr = nil
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	third := engine.processFile(t.Context(), file)
	require.NoError(t, third.err)
	assert.False(t, third.cachedFailure)
	assert.Equal(t, int32(1), provider.parseCalls.Load())
}

func TestPersistedSourceFailureSuppressesParseAfterRestart(t *testing.T) {
	const agent parser.AgentType = "persisted-failure"

	database, engine, provider, root, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("persistent malformed source")
	file := parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	}

	first := engine.processFile(t.Context(), file)
	require.Error(t, first.err)
	engine.cacheFailure(first.failureCacheKey, first.failureMtime)
	require.Equal(t, 1, engine.persistSkipCache())

	restarted := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			directStreamingFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(restarted.Close)

	second := restarted.processFile(t.Context(), file)
	require.Error(t, second.err)
	assert.True(t, second.cachedFailure)
	assert.Equal(t, int32(1), provider.parseCalls.Load())
}

func TestTransientProviderFailureDoesNotUseFailureCache(t *testing.T) {
	const agent parser.AgentType = "transient-failure"

	_, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("temporary scan failure")
	file := parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	}

	first := engine.processFile(t.Context(), file)
	require.Error(t, first.err)
	assert.False(t, first.cacheFailure)
	second := engine.processFile(t.Context(), file)
	require.Error(t, second.err)
	assert.False(t, second.cachedFailure)
	assert.Equal(t, int32(2), provider.parseCalls.Load())
}

func TestCompositeProviderFailureBypassesFailureCache(t *testing.T) {
	const agent parser.AgentType = "composite-failure"

	_, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	provider.Caps.Source.CompositeFingerprint = parser.CapabilitySupported
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("malformed source")
	file := parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	}

	first := engine.processFile(t.Context(), file)
	require.Error(t, first.err)
	assert.False(t, first.cacheFailure)
	second := engine.processFile(t.Context(), file)
	require.Error(t, second.err)
	assert.False(t, second.cachedFailure)
	assert.Equal(t, int32(2), provider.parseCalls.Load())
}

func TestFailureCacheBypassesStaleDataVersion(t *testing.T) {
	const agent parser.AgentType = "stale-failure"

	database, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	seedActiveBaselineSource(t, database, agent, "stale-failure", path)
	require.NoError(t, database.SetSessionDataVersion(
		"stale-failure", db.CurrentDataVersion()-1,
	))
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("malformed source")
	file := parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	}

	first := engine.processFile(t.Context(), file)
	require.Error(t, first.err)
	require.True(t, first.cacheFailure)
	engine.cacheFailure(first.failureCacheKey, first.failureMtime)

	second := engine.processFile(t.Context(), file)
	require.Error(t, second.err)
	assert.False(t, second.cachedFailure)
	assert.Equal(t, int32(2), provider.parseCalls.Load())
}

func TestSourceParseFailureCacheableClassifiesTransientErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "malformed", err: errors.New("malformed source"), want: true},
		{name: "permission", err: errors.New("permission denied"), want: false},
		{name: "scan", err: errors.New("temporary scan failure"), want: false},
		{name: "locked", err: errors.New("database is locked"), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, sourceParseFailureCacheable(test.err))
		})
	}
}
