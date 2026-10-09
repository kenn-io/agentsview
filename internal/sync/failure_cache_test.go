package sync

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestProviderParseFailureIsCachedUntilSourceChanges(t *testing.T) {
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
	engine.failures.Record(first.failureCacheKey, first.failureIdentity)

	second := engine.processFile(t.Context(), file)
	require.NoError(t, second.err)
	assert.True(t, second.skip)
	assert.True(t, second.cachedFailure)
	assert.Equal(t, int32(1), provider.parseCalls.Load())

	updated := info.ModTime().Add(time.Second)
	require.NoError(t, os.Chtimes(path, updated, updated))
	provider.fingerprint.MTimeNS = updated.UnixNano()
	third := engine.processFile(t.Context(), file)
	require.Error(t, third.err)
	assert.False(t, third.cachedFailure)
	assert.Equal(t, int32(2), provider.parseCalls.Load())

	engine.failures.Record(third.failureCacheKey, third.failureIdentity)
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
		engine.failures.Record(first.failureCacheKey, first.failureIdentity)
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

	database, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("malformed source")

	require.Error(t, engine.SyncPathsContext(t.Context(), []string{path}))
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Equal(t, db.SourceFailure{MTimeNS: info.ModTime().UnixNano()},
		persisted[providerAgentSkipCacheKey(path, agent)])
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
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))
	assert.Equal(t, int32(1), provider.parseCalls.Load())
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Equal(t, db.SourceFailure{MTimeNS: info.ModTime().UnixNano()},
		persisted[providerAgentSkipCacheKey(path, agent)])
}

func TestSyncSingleSessionPersistsProviderFailure(t *testing.T) {
	const sessionID = "single-session-failure"

	database, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, parser.AgentClaude, func(string) parser.ParseOutcome {
			return parser.ParseOutcome{}
		},
	)
	provider.allowFindSource = true
	seedActiveBaselineSource(t, database, parser.AgentClaude, sessionID, path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("malformed source")

	require.Error(t, engine.SyncSingleSessionContext(t.Context(), sessionID))
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Equal(t, db.SourceFailure{MTimeNS: info.ModTime().UnixNano()},
		persisted[providerAgentSkipCacheKey(path, parser.AgentClaude)])
}

func TestCanceledSyncSingleSessionDoesNotPersistProviderFailure(t *testing.T) {
	const sessionID = "single-session-canceled-failure"

	database, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, parser.AgentClaude, func(string) parser.ParseOutcome {
			return parser.ParseOutcome{}
		},
	)
	provider.allowFindSource = true
	seedActiveBaselineSource(t, database, parser.AgentClaude, sessionID, path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("malformed source")

	ctx, cancel := context.WithCancel(t.Context())
	provider.parseCancel = cancel
	require.Error(t, engine.SyncSingleSessionContext(ctx, sessionID))
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Empty(t, persisted)
	assert.False(t, engine.failures.Check(
		providerAgentSkipCacheKey(path, parser.AgentClaude),
		db.SourceFailure{MTimeNS: info.ModTime().UnixNano()},
	), "a canceled pass must not record a failure")
}

func TestMissingSourceFailureIsCachedUntilSourceAppears(t *testing.T) {
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
	assert.True(t, first.failureIdentity.Missing)
	engine.failures.Record(first.failureCacheKey, first.failureIdentity)

	second := engine.processFile(t.Context(), file)
	require.NoError(t, second.err)
	assert.True(t, second.skip)
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
	engine.failures.Record(first.failureCacheKey, first.failureIdentity)
	engine.flushFailureCache(t.Context())

	restarted := NewEngine(t.Context(), database, EngineConfig{
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
	require.NoError(t, second.err)
	assert.True(t, second.skip)
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
		t.Context(), "stale-failure", db.CurrentDataVersion()-1,
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
	engine.failures.Record(first.failureCacheKey, first.failureIdentity)

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

// A resync is how a parser upgrade reaches sources that never produced a
// session row, so it must retry sources that failed under the old parser.
func TestResyncRetriesCachedSourceFailure(t *testing.T) {
	engine, database, provider, _, path := newResyncFailureEngine(t)
	info, err := os.Stat(path)
	require.NoError(t, err)
	engine.failures.Record(
		providerAgentSkipCacheKey(path, provider.Def.Type),
		db.SourceFailure{MTimeNS: info.ModTime().UnixNano()},
	)
	engine.flushFailureCache(t.Context())
	parsed := provider.parseCalls.Load()

	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	assert.Equal(t, parsed+1, provider.parseCalls.Load())
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Empty(t, persisted)
}

// After a worker resync is discarded, the parent still holds the old failure
// cache. Resetting it before the incremental catch-up retries sources that
// failed under the previous parser, matching in-process resync abort.
func TestResetFailureCacheRetriesCachedSourceFailure(t *testing.T) {
	const agent parser.AgentType = "reset-failure"

	database, engine, provider, root, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome {
			return parser.ParseOutcome{ResultSetComplete: true}
		},
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	engine.failures.Record(
		providerAgentSkipCacheKey(path, agent),
		db.SourceFailure{MTimeNS: info.ModTime().UnixNano()},
	)
	engine.flushFailureCache(t.Context())

	engine.ResetFailureCache(t.Context())

	retried := engine.processFile(t.Context(), parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	})
	require.NoError(t, retried.err)
	assert.Equal(t, int32(1), provider.parseCalls.Load())
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Empty(t, persisted)

	restarted := NewEngine(t.Context(), database, EngineConfig{
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
	afterRestart := restarted.processFile(t.Context(), parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	})
	require.NoError(t, afterRestart.err)
	assert.False(t, afterRestart.cachedFailure)
	assert.Equal(t, int32(2), provider.parseCalls.Load())
}

func TestWatcherOverflowRetriesCachedSourceFailure(t *testing.T) {
	const agent parser.AgentType = "overflow-failure"

	database, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome {
			return parser.ParseOutcome{ResultSetComplete: true}
		},
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	engine.failures.Record(
		providerAgentSkipCacheKey(path, agent),
		db.SourceFailure{MTimeNS: info.ModTime().UnixNano()},
	)
	engine.flushFailureCache(t.Context())

	engine.clearWatcherOverflowCaches(t.Context())

	retried := engine.processFile(t.Context(), parser.DiscoveredFile{
		Path: path, Agent: agent,
		ProviderSource: provider.source, ProviderProcess: true,
	})
	require.NoError(t, retried.err)
	assert.Equal(t, int32(1), provider.parseCalls.Load())
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Empty(t, persisted)
}

func TestSuccessfulSingleSessionRefreshForgetsFailureDurably(t *testing.T) {
	const sessionID = "single-session-recovered"

	database, engine, provider, _, path := newChangedPathOutcomeEngine(
		t, parser.AgentClaude, func(string) parser.ParseOutcome {
			return parser.ParseOutcome{ResultSetComplete: true}
		},
	)
	provider.allowFindSource = true
	seedActiveBaselineSource(t, database, parser.AgentClaude, sessionID, path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{
		Key: path, MTimeNS: info.ModTime().UnixNano(),
	}
	provider.parseErr = errors.New("malformed source")
	require.Error(t, engine.SyncSingleSessionContext(t.Context(), sessionID))
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	require.Len(t, persisted, 1)

	provider.parseErr = nil
	require.NoError(t, engine.SyncSingleSessionContext(t.Context(), sessionID))
	persisted, err = database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Empty(t, persisted)
}

// An epoch mtime is a real value, so an absent marker must not match it.
func TestClaudeRowlessFreshnessRequiresMarkerForEpochMtime(t *testing.T) {
	_, engine, _, _, path := newChangedPathOutcomeEngine(
		t, parser.AgentClaude, func(string) parser.ParseOutcome {
			return parser.ParseOutcome{}
		},
	)
	assert.False(t, engine.claudeRowlessFreshnessMarked(path, path, "hash", 0))
}

func TestFailureCacheRealProviders(t *testing.T) {
	for _, tc := range []struct {
		agent                parser.AgentType
		path, first, changed string
	}{
		{parser.AgentGemini, "tmp/project/chats/session-broken.json", `{"messages": invalid}`, `{"messages": broken!}`},
		{parser.AgentCline, "data/sessions/session-1/session-1.json", `[]`, `42`},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tc.path)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(tc.first), 0o600))
			database := openTestDB(t)
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{tc.agent: {root}}, Machine: "local"}
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			require.Error(t, engine.SyncPathsContext(t.Context(), []string{path}))
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
			restarted := NewEngine(t.Context(), database, cfg)
			t.Cleanup(restarted.Close)
			require.NoError(t, restarted.SyncPathsContext(t.Context(), []string{path}))
			info, err := os.Stat(path)
			require.NoError(t, err)
			// A different malformed payload at the same mtime must be attempted again.
			require.NoError(t, os.WriteFile(path, []byte(tc.changed), 0o600))
			require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
			require.Error(t, restarted.SyncPathsContext(t.Context(), []string{path}))
			require.NoError(t, restarted.SyncPathsContext(t.Context(), []string{path}))
		})
	}
}

func TestSingleSessionTransientFailureForgetsCachedFailure(t *testing.T) {
	database, engine, provider, _, path := newChangedPathOutcomeEngine(t, parser.AgentClaude,
		func(string) parser.ParseOutcome { return parser.ParseOutcome{} })
	provider.allowFindSource = true
	seedActiveBaselineSource(t, database, parser.AgentClaude, "refresh-failure", path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{Key: path, MTimeNS: info.ModTime().UnixNano()}
	provider.parseErr = errors.New("malformed source")
	require.Error(t, engine.SyncSingleSessionContext(t.Context(), "refresh-failure"))
	provider.parseErr = errors.New("temporary read failure")
	require.Error(t, engine.SyncSingleSessionContext(t.Context(), "refresh-failure"))
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Empty(t, persisted)
}

func TestChangedPathTombstoneForgetsCachedFailure(t *testing.T) {
	database, engine, provider, _, path := newChangedPathOutcomeEngine(t, parser.AgentClaude,
		func(string) parser.ParseOutcome { return parser.ParseOutcome{} })
	seedActiveBaselineSource(t, database, parser.AgentClaude, "deleted-failure", path)
	require.NoError(t, database.BaselineActiveSessionSourcePaths(t.Context(), "local", []db.SessionSourcePath{{Agent: string(parser.AgentClaude), FilePath: path}}))
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{Key: path, MTimeNS: info.ModTime().UnixNano()}
	provider.parseErr = errors.New("malformed source")
	require.Error(t, engine.SyncPathsContext(t.Context(), []string{path}))
	require.NoError(t, os.Remove(path))
	provider.source = nil
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Empty(t, persisted)
	session, err := database.GetSessionFull(t.Context(), "deleted-failure")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.NotNil(t, session.SourceMissingAt)
}

func TestFailureCacheClearAcceptsPathAndProcessKey(t *testing.T) {
	for _, test := range []struct{ name, suffix string }{
		{"bare", ""},
		{"hashed", "?agent=clear-failure?source_hash=abc"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, engine, provider, _, path := newChangedPathOutcomeEngine(t, "clear-failure",
				func(string) parser.ParseOutcome { return parser.ParseOutcome{} })
			info, err := os.Stat(path)
			require.NoError(t, err)
			provider.fingerprint = parser.SourceFingerprint{Key: path, MTimeNS: info.ModTime().UnixNano()}
			provider.parseErr = errors.New("malformed source")
			first := engine.processFile(t.Context(), parser.DiscoveredFile{Path: path, Agent: "clear-failure", ProviderSource: provider.source, ProviderProcess: true})
			require.Error(t, first.err)
			require.True(t, first.cacheFailure)
			engine.failures.Record(first.failureCacheKey, first.failureIdentity)
			engine.clearSkipInMemory(path + test.suffix)
			second := engine.processFile(t.Context(), parser.DiscoveredFile{Path: path, Agent: "clear-failure", ProviderSource: provider.source, ProviderProcess: true})
			require.Error(t, second.err)
			assert.Equal(t, int32(2), provider.parseCalls.Load())
		})
	}
}

func TestFailureCacheKeepsPreParseIdentity(t *testing.T) {
	_, engine, provider, _, path := newChangedPathOutcomeEngine(t, "append-failure",
		func(string) parser.ParseOutcome { return parser.ParseOutcome{} })
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{Key: path, MTimeNS: info.ModTime().UnixNano()}
	provider.parseErr = errors.New("malformed source")
	started, release := make(chan struct{}, 1), make(chan struct{})
	provider.parseStarted, provider.parseRelease = started, release
	result := make(chan processResult, 1)
	file := parser.DiscoveredFile{Path: path, Agent: "append-failure", ProviderSource: provider.source, ProviderProcess: true}
	go func() { result <- engine.processFile(t.Context(), file) }()
	<-started
	updated := info.ModTime().Add(time.Second)
	require.NoError(t, os.Chtimes(path, updated, updated))
	close(release)
	first := <-result
	require.Error(t, first.err)
	engine.failures.Record(first.failureCacheKey, first.failureIdentity)
	provider.fingerprint.MTimeNS = updated.UnixNano()
	second := engine.processFile(t.Context(), file)
	require.Error(t, second.err)
	assert.Equal(t, int32(2), provider.parseCalls.Load())
}

func TestFailureClassifierIgnoresSourcePath(t *testing.T) {
	for _, test := range []struct {
		path, message string
		want          bool
	}{
		{"/code/api/orders/session.json", "malformed source", true},
		{"/code/decode/session.json", "failed", false},
		{"/code/parse/session.json", "permission denied", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			assert.Equal(t, test.want, sourceParseFailureCacheable(fmt.Errorf("%s: %s", test.path, test.message), test.path))
		})
	}
}

// Schema repair must invalidate a cached failure even without a chat timestamp change.
func TestFailureCachePiebaldSchemaRepair(t *testing.T) {
	for _, mode := range []string{"changed-path", "full-sync"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "app.db")
			sourceDB, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sourceDB.Close()) })
			_, err = sourceDB.ExecContext(t.Context(), `
    PRAGMA journal_mode=WAL;
    CREATE TABLE chats (id INTEGER PRIMARY KEY, title TEXT, created_at TEXT,
     updated_at TEXT, is_deleted INTEGER, message_count INTEGER,
     branch_name TEXT, project_id INTEGER);
    CREATE TABLE projects (id INTEGER PRIMARY KEY, directory TEXT, name TEXT);
    INSERT INTO chats VALUES (42, 'broken', '2026-01-01', '2026-01-01', 0, 1, '', NULL);
   `)
			require.NoError(t, err)
			database := openTestDB(t)
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentPiebald: {root}}, Machine: "local"}
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			run := func(e *Engine) bool {
				if mode == "full-sync" {
					stats := e.SyncAll(t.Context(), nil)
					return stats.ProcessingComplete()
				}
				return e.SyncPathsContext(t.Context(), []string{path}) == nil
			}
			require.False(t, run(engine))
			require.True(t, run(engine), "unchanged schema failure must stop failing the pass")
			replay := engine.SyncAllForceParseAfterCache(t.Context(), nil)
			require.True(t, replay.ProcessingComplete(), "remote replay must keep cached failures suppressed")
			restarted := NewEngine(t.Context(), database, cfg)
			t.Cleanup(restarted.Close)
			require.True(t, run(restarted), "failure must survive restart")
			// Repair only the schema; the chat timestamp remains unchanged. The next
			// attempted parse reaches the still-missing messages table and must fail.
			_, err = sourceDB.ExecContext(t.Context(), `ALTER TABLE chats ADD COLUMN worktree_path TEXT`)
			require.NoError(t, err)
			require.False(t, run(restarted), "a WAL-only schema repair must invalidate the cached failure")
		})
	}
}

func TestAbortedContributorKeepsSourceFailures(t *testing.T) {
	parent, database, _ := newResyncSplitEngine(t)
	_, _, provider, contributorRoot, path := newChangedPathOutcomeEngine(t, "contributor-failure",
		func(string) parser.ParseOutcome { return parser.ParseOutcome{} })
	info, err := os.Stat(path)
	require.NoError(t, err)
	provider.fingerprint = parser.SourceFingerprint{Key: path, MTimeNS: info.ModTime().UnixNano()}
	provider.allowDiscover = true
	provider.parseErr = errors.New("malformed source")
	cfg := EngineConfig{
		AgentDirs: map[parser.AgentType][]string{provider.Def.Type: {contributorRoot}}, Machine: "remote",
		ProviderFactories:      []parser.ProviderFactory{directStreamingFactory{provider: provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{provider.Def.Type: parser.ProviderMigrationProviderAuthoritative},
	}
	stats, err := parent.ResyncAllWithOptions(t.Context(), nil, RebuildOptions{
		Contributors: []RebuildContributor{{Name: "remote", Config: cfg}},
	})
	require.NoError(t, err)
	require.True(t, stats.Aborted)
	persisted, err := database.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	require.Contains(t, persisted, providerAgentSkipCacheKey(path, provider.Def.Type))
	restarted := NewEngine(t.Context(), database, cfg)
	t.Cleanup(restarted.Close)
	parsed := provider.parseCalls.Load()
	next := restarted.SyncAll(t.Context(), nil)
	require.True(t, next.ProcessingComplete())
	assert.Equal(t, parsed, provider.parseCalls.Load())
}

func TestFailureClassifierIgnoresCompanionPath(t *testing.T) {
	var value struct{}
	err := json.Unmarshal([]byte(`{"broken":?}`), &value)
	require.Error(t, err)
	assert.True(t, sourceParseFailureCacheable(
		fmt.Errorf("decode metadata /data/incomplete/session.json: %w", err),
		"/data/incomplete/session.jsonl",
	))
}

func TestFailureCacheMissingGrokSummary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "session-1", "summary.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))
	engine := NewEngine(t.Context(), openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	files := requireClassifyProviderChangedPath(t, engine, path)
	require.Len(t, files, 1)
	// A queued source can disappear after discovery and before its worker runs.
	require.NoError(t, os.Remove(path))
	run := func() SyncStats {
		return engine.collectAndBatch(t.Context(), engine.startWorkers(t.Context(), files),
			len(files), len(files), nil, syncWriteDefault)
	}
	first := run()
	require.Equal(t, 1, first.Failed)
	cached := run()
	require.True(t, cached.ProcessingComplete())
	require.Equal(t, 1, cached.Skipped)
	require.NoError(t, os.WriteFile(path, []byte(`{"broken":?}`), 0o600))
	recreated := run()
	require.Equal(t, 1, recreated.Failed, "creating the summary must invalidate the missing-file entry")
	cached = run()
	require.True(t, cached.ProcessingComplete())
}

func TestCachedOpenCodeFailureDoesNotVerifyContainer(t *testing.T) {
	schema, err := os.ReadFile("../parser/testdata/opencode_v2/beta.sql")
	require.NoError(t, err)
	root := t.TempDir()
	source, err := sql.Open("sqlite3", filepath.Join(root, "opencode.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	_, err = source.ExecContext(t.Context(), string(schema))
	require.NoError(t, err)
	archive := openTestDB(t)
	cfg := EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {root}},
		Machine:   "local", DisableFilesystemProjectDiscovery: true,
	}
	first := NewEngine(t.Context(), archive, cfg)
	initial := first.SyncAll(t.Context(), nil)
	first.Close()
	require.True(t, initial.ProcessingComplete())
	const sessionID = "ses_f78f36bd6ffeTXo4UWv9dIETyI"
	_, err = source.ExecContext(t.Context(),
		`UPDATE session_message SET data = '!', time_updated = 1900000000000 WHERE session_id = ? AND type = 'user'`, sessionID)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), archive, cfg)
	t.Cleanup(engine.Close)
	failed := engine.SyncAll(t.Context(), nil)
	require.False(t, failed.ProcessingComplete())
	cached := engine.SyncAll(t.Context(), nil)
	require.True(t, cached.ProcessingComplete())
	// Repair child data without changing the session or message timestamps.
	_, err = source.ExecContext(t.Context(),
		`UPDATE session_message SET data = '{"text":"repaired prompt"}' WHERE session_id = ? AND type = 'user'`, sessionID)
	require.NoError(t, err)
	repaired := engine.SyncAll(t.Context(), nil)
	require.True(t, repaired.ProcessingComplete())
	messages, err := archive.GetAllMessages(t.Context(), "opencode:"+sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	assert.Equal(t, "repaired prompt", messages[0].Content,
		"a cached failure must not authorize watermark-only discovery after repair")
}
