package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

var (
	processProviderPiebaldSchemaOnce  stdsync.Once
	processProviderPiebaldSchemaBytes []byte
	processProviderPiebaldSchemaErr   error
)

const processProviderPiebaldSchema = `
	CREATE TABLE projects (
		id INTEGER PRIMARY KEY,
		directory TEXT NOT NULL,
		name TEXT NOT NULL
	);
	CREATE TABLE chats (
		id INTEGER PRIMARY KEY,
		title TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		is_deleted BOOLEAN NOT NULL DEFAULT 0,
		message_count INTEGER NOT NULL DEFAULT 0,
		current_directory TEXT,
		worktree_path TEXT,
		branch_name TEXT,
		project_id INTEGER
	);
	CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		parent_chat_id INTEGER NOT NULL,
		parent_message_id INTEGER,
		role TEXT NOT NULL,
		model TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		input_tokens BIGINT,
		output_tokens BIGINT,
		reasoning_tokens BIGINT,
		cache_read_tokens BIGINT,
		cache_write_tokens BIGINT,
		status TEXT NOT NULL,
		finish_reason TEXT,
		error TEXT,
		enabled INTEGER NOT NULL DEFAULT 1
	);
	CREATE TABLE message_parts (
		id INTEGER PRIMARY KEY,
		parent_chat_message_id INTEGER NOT NULL,
		part_index INTEGER NOT NULL,
		part_type TEXT NOT NULL
	);
	CREATE TABLE message_part_text (
		message_part_id INTEGER PRIMARY KEY,
		is_thinking BOOLEAN NOT NULL DEFAULT FALSE
	);
	CREATE TABLE message_content_nodes (
		id INTEGER PRIMARY KEY,
		parent_text_part_id INTEGER NOT NULL,
		node_index INTEGER NOT NULL,
		node_type TEXT NOT NULL
	);
	CREATE TABLE message_node_text (
		node_id INTEGER PRIMARY KEY,
		content TEXT NOT NULL
	);
	CREATE TABLE message_part_tool_call (
		message_part_id INTEGER PRIMARY KEY,
		provider_tool_use_id TEXT NOT NULL,
		tool_name TEXT NOT NULL,
		tool_input TEXT NOT NULL,
		tool_result TEXT,
		tool_error TEXT,
		tool_state TEXT NOT NULL DEFAULT 'pending',
		sub_agent_chat_id INTEGER
	);
`

func TestProcessFileProviderForgeVirtualSource(t *testing.T) {

	root := t.TempDir()
	dbPath := writeProcessProviderForgeDB(t, root)
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentForge: {root},
		},
		Machine: "devbox",
	})

	files := engine.classifyProviderChangedPath(dbPath)
	require.Len(t, files, 1)
	assert.Equal(t, dbPath+"#conv-001", files[0].Path)
	assert.Equal(t, parser.AgentForge, files[0].Agent)
	assert.False(t, files[0].ForceParse)

	res := engine.processFile(context.Background(), files[0])

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	assert.True(t, res.forceReplace)
	assert.NotZero(t, res.mtime)
	assert.Equal(t, "forge:conv-001", res.results[0].Session.ID)
	assert.Equal(t, parser.AgentForge, res.results[0].Session.Agent)
	assert.Equal(t, "devbox", res.results[0].Session.Machine)
	assert.Len(t, res.results[0].Messages, 2)
}

func TestProviderChangedPathUsesSourceMachine(t *testing.T) {
	root := t.TempDir()
	dbPath := writeProcessProviderForgeDB(t, root)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentForge: {root},
		},
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentForge: {root: "archivebox"},
		},
		Machine: "localbox",
	})

	files := engine.classifyProviderChangedPath(dbPath)
	require.Len(t, files, 1)
	assert.Equal(t, "archivebox", files[0].Machine)

	res := engine.processFile(context.Background(), files[0])

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	assert.Equal(t, "archivebox", res.results[0].Session.Machine)
	assert.Equal(t, "forge:conv-001", res.results[0].Session.ID)

	engine.SyncPathsContext(context.Background(), []string{dbPath})
	sess, err := database.GetSessionFull(context.Background(), "forge:conv-001")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "archivebox", sess.Machine)
}

func TestProviderPeriodicSyncUsesSourceMachine(t *testing.T) {
	root := t.TempDir()
	writeProcessProviderForgeDB(t, root)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentForge: {root},
		},
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentForge: {root: "archivebox"},
		},
		Machine: "localbox",
	})

	stats := engine.SyncAll(context.Background(), nil)

	assert.False(t, stats.Aborted)
	sess, err := database.GetSessionFull(context.Background(), "forge:conv-001")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "archivebox", sess.Machine)
}

func TestProcessFileProviderSkipsStoredFreshSource(t *testing.T) {

	root := t.TempDir()
	dbPath := writeProcessProviderForgeDB(t, root)
	virtualPath := dbPath + "#conv-001"
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentForge: {root},
		},
		Machine: "devbox",
	})

	first := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  virtualPath,
		Agent: parser.AgentForge,
	})
	require.NoError(t, first.err)
	require.Len(t, first.results, 1)
	written, _, failed, _ := engine.writeBatch(
		[]pendingWrite{{
			sess:         first.results[0].Session,
			msgs:         first.results[0].Messages,
			usageEvents:  first.results[0].UsageEvents,
			forceReplace: first.forceReplace,
		}},
		syncWriteDefault,
		false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)
	require.Empty(t, engine.skipCache)

	second := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  virtualPath,
		Agent: parser.AgentForge,
	})

	require.NoError(t, second.err)
	assert.True(t, second.skip)
	assert.True(t, second.cacheSkip)
	assert.Equal(t, first.mtime, second.mtime)
	assert.Empty(t, second.results)
}

func TestProcessFileProviderPiebaldVirtualSource(t *testing.T) {

	root := t.TempDir()
	dbPath := filepath.Join(root, "app.db")
	piebaldDB := openProcessProviderPiebaldDB(t, dbPath)
	seedProcessProviderPiebaldChat(t, piebaldDB)
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPiebald: {root},
		},
		Machine: "devbox",
	})

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  dbPath + "#42",
		Agent: parser.AgentPiebald,
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	assert.True(t, res.forceReplace)
	assert.NotZero(t, res.mtime)
	assert.Equal(t, "piebald:42", res.results[0].Session.ID)
	assert.Equal(t, parser.AgentPiebald, res.results[0].Session.Agent)
	assert.Equal(t, "devbox", res.results[0].Session.Machine)
	assert.Len(t, res.results[0].Messages, 2)
}

// TestProcessFileProviderPiebaldSkipsStoredFreshSource verifies
// that a provider-authoritative Piebald chat whose stored fingerprint already
// matches is not reparsed on a repeat processFile. Piebald keeps every chat in
// one app.db, but the provider fingerprint's mtime is the chat's own updated_at
// timestamp (see ListPiebaldSessionMeta), so an untouched chat has a stable
// per-session signal and skips on the DB-stored-fingerprint check. This mirrors
// the legacy syncPiebald/piebaldPendingSessionIDs skip and the Forge
// SkipsStoredFreshSource behavior; the in-memory skip cache stays empty.
func TestProcessFileProviderPiebaldSkipsStoredFreshSource(t *testing.T) {

	root := t.TempDir()
	dbPath := filepath.Join(root, "app.db")
	piebaldDB := openProcessProviderPiebaldDB(t, dbPath)
	seedProcessProviderPiebaldChat(t, piebaldDB)
	virtualPath := dbPath + "#42"
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPiebald: {root},
		},
		Machine: "devbox",
	})

	first := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  virtualPath,
		Agent: parser.AgentPiebald,
	})
	require.NoError(t, first.err)
	require.Len(t, first.results, 1)
	written, _, failed, _ := engine.writeBatch(
		[]pendingWrite{{
			sess:         first.results[0].Session,
			msgs:         first.results[0].Messages,
			usageEvents:  first.results[0].UsageEvents,
			forceReplace: first.forceReplace,
		}},
		syncWriteDefault,
		false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)
	require.Empty(t, engine.skipCache)

	second := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  virtualPath,
		Agent: parser.AgentPiebald,
	})

	require.NoError(t, second.err)
	assert.True(t, second.skip)
	assert.Equal(t, first.mtime, second.mtime)
	assert.Empty(t, second.results)
}

func TestProcessFileProviderWarpVirtualSource(t *testing.T) {

	root := t.TempDir()
	dbPath := filepath.Join(root, "warp.sqlite")
	warpDB := openProcessProviderWarpDB(t, dbPath)
	seedProcessProviderWarpConversation(t, warpDB)
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWarp: {root},
		},
		Machine: "devbox",
	})

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  dbPath + "#conv-001",
		Agent: parser.AgentWarp,
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	assert.True(t, res.forceReplace)
	assert.NotZero(t, res.mtime)
	assert.Equal(t, "warp:conv-001", res.results[0].Session.ID)
	assert.Equal(t, parser.AgentWarp, res.results[0].Session.Agent)
	assert.Equal(t, "devbox", res.results[0].Session.Machine)
	assert.NotEmpty(t, res.results[0].Messages)
}

func TestProcessFileProviderZCodeVirtualSource(t *testing.T) {
	root := t.TempDir()
	dbPath := writeProcessProviderZCodeDB(t, filepath.Join(root, ".zcode", "cli"))
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentZCode: {filepath.Join(root, ".zcode", "cli")},
		},
		Machine: "devbox",
	})

	files := engine.classifyProviderChangedPath(dbPath)
	require.Len(t, files, 1)
	assert.Equal(t, dbPath+"#session-001", files[0].Path)
	assert.Equal(t, parser.AgentZCode, files[0].Agent)
	assert.False(t, files[0].ForceParse)

	res := engine.processFile(context.Background(), files[0])

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	assert.True(t, res.forceReplace)
	assert.NotZero(t, res.mtime)
	assert.Equal(t, "zcode:session-001", res.results[0].Session.ID)
	assert.Equal(t, parser.AgentZCode, res.results[0].Session.Agent)
	assert.Equal(t, "devbox", res.results[0].Session.Machine)
	require.Len(t, res.results[0].Messages, 3)
	assert.Equal(t, parser.RoleAssistant, res.results[0].Messages[1].Role)
	assert.True(t, res.results[0].Messages[1].HasToolUse)
	require.Len(t, res.results[0].Messages[1].ToolCalls, 1)
	assert.Equal(t, "call-read", res.results[0].Messages[1].ToolCalls[0].ToolUseID)
	require.Len(t, res.results[0].UsageEvents, 1)
	assert.Equal(t, 1, res.results[0].UsageEvents[0].InputTokens)
	assert.Equal(t, 2, res.results[0].UsageEvents[0].OutputTokens)
}

func TestProcessFileUsesProviderDBBackedFamily(t *testing.T) {

	for _, agent := range []parser.AgentType{
		parser.AgentForge,
		parser.AgentPiebald,
		parser.AgentWarp,
		parser.AgentZCode,
	} {
		assert.True(t, processFileUsesProvider(agent), agent)
	}
	assert.False(t, processFileUsesProvider(parser.AgentClaude))
}

func TestProcessFileProviderAuthoritativeUsesInjectedProvider(t *testing.T) {

	root := t.TempDir()
	sourcePath, fingerprint := writeProcessProviderSource(t, root, "owned.jsonl")
	provider := newProcessFixtureProvider(
		parser.SourceRef{
			Provider:       parser.AgentCowork,
			Key:            "source-owned",
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath,
			ProjectHint:    "fixture-project",
		},
		fingerprint,
		parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{{
				Result: processFixtureResult(
					"cowork:owned",
					parser.AgentCowork,
					"fixture-project",
					sourcePath,
					fingerprint,
				),
				DataVersion: parser.DataVersionCurrent,
			}},
			ResultSetComplete: true,
			ForceReplace:      true,
		},
	)
	engine := newProcessFixtureEngine(t, root, provider)

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentCowork,
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	assert.False(t, res.skip)
	assert.True(t, res.forceReplace)
	assert.Equal(t, fingerprint.MTimeNS, res.mtime)
	assert.Equal(t, []string{"find-source", "fingerprint", "parse"}, provider.calls)
	require.Len(t, provider.findRequests, 1)
	assert.True(t, provider.findRequests[0].RequireFreshSource)
	assert.Equal(t, sourcePath, provider.findRequests[0].StoredFilePath)
	assert.Equal(t, parser.AgentCowork, res.results[0].Session.Agent)
	assert.Equal(t, "cowork:owned", res.results[0].Session.ID)
	assert.Equal(t, "devbox", res.results[0].Session.Machine)
	assert.Equal(t, "fixture-project", res.results[0].Session.Project)
}

func TestProcessFileProviderAuthoritativeKeepsRetryStatePerResult(t *testing.T) {

	root := t.TempDir()
	sourcePath, fingerprint := writeProcessProviderSource(t, root, "retry.jsonl")
	provider := newProcessFixtureProvider(
		processFixtureSource(sourcePath),
		fingerprint,
		parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{
				{
					Result: processFixtureResult(
						"cowork:current",
						parser.AgentCowork,
						"fixture-project",
						sourcePath,
						fingerprint,
					),
					DataVersion: parser.DataVersionCurrent,
				},
				{
					Result: processFixtureResult(
						"cowork:retry",
						parser.AgentCowork,
						"fixture-project",
						sourcePath,
						fingerprint,
					),
					DataVersion: parser.DataVersionNeedsRetry,
				},
			},
			ResultSetComplete: true,
		},
	)
	engine := newProcessFixtureEngine(t, root, provider)

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentCowork,
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 2)
	assert.False(t, res.needsRetryForSession("cowork:current"))
	assert.True(t, res.needsRetryForSession("cowork:retry"))
	assert.False(t, res.suppressesPresenceSweepForRetry())
}

func TestProcessFileProviderAuthoritativeSuppressesUncleanSkipCache(t *testing.T) {

	root := t.TempDir()
	sourcePath, fingerprint := writeProcessProviderSource(t, root, "unclean.jsonl")
	provider := newProcessFixtureProvider(
		processFixtureSource(sourcePath),
		fingerprint,
		parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{{
				Result: processFixtureResult(
					"cowork:unclean",
					parser.AgentCowork,
					"fixture-project",
					sourcePath,
					fingerprint,
				),
				DataVersion: parser.DataVersionCurrent,
			}},
			ResultSetComplete: false,
		},
	)
	engine := newProcessFixtureEngine(t, root, provider)

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentCowork,
	})

	require.NoError(t, res.err)
	assert.True(t, res.cacheSkip)
	assert.True(t, res.noCacheSkip)
	assert.True(t, res.suppressPresenceSweep)
}

func TestProcessFileProviderAuthoritativeUsesSkipReasonCacheKey(t *testing.T) {

	root := t.TempDir()
	sourcePath, fingerprint := writeProcessProviderSource(t, root, "skip.jsonl")
	source := processFixtureSource(sourcePath)
	source.FingerprintKey = sourcePath + "#provider-key"
	provider := newProcessFixtureProvider(
		source,
		fingerprint,
		parser.ParseOutcome{
			SkipReason:        parser.SkipNonInteractive,
			ResultSetComplete: true,
		},
	)
	engine := newProcessFixtureEngine(t, root, provider)

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentCowork,
	})

	require.NoError(t, res.err)
	assert.True(t, res.skip)
	assert.True(t, res.cacheSkip)
	assert.False(t, res.noCacheSkip)
	assert.Equal(t, source.FingerprintKey, res.skipCacheKey(sourcePath))
}

func TestProcessFileProviderAuthoritativeForceParseAllowsStaleSourceLookup(t *testing.T) {

	root := t.TempDir()
	sourcePath, fingerprint := writeProcessProviderSource(t, root, "force.jsonl")
	provider := newProcessFixtureProvider(
		processFixtureSource(sourcePath),
		fingerprint,
		parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{{
				Result: processFixtureResult(
					"cowork:force",
					parser.AgentCowork,
					"fixture-project",
					sourcePath,
					fingerprint,
				),
				DataVersion: parser.DataVersionCurrent,
			}},
			ResultSetComplete: true,
		},
	)
	engine := newProcessFixtureEngine(t, root, provider)

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:       sourcePath,
		Agent:      parser.AgentCowork,
		ForceParse: true,
	})

	require.NoError(t, res.err)
	require.Len(t, provider.findRequests, 1)
	assert.False(t, provider.findRequests[0].RequireFreshSource)
	require.Len(t, provider.parseRequests, 1)
	assert.True(t, provider.parseRequests[0].ForceParse)
}

func TestProcessFileProviderAuthoritativeNotFoundFails(t *testing.T) {

	root := t.TempDir()
	sourcePath, fingerprint := writeProcessProviderSource(t, root, "missing.jsonl")
	provider := newProcessFixtureProvider(
		processFixtureSource(sourcePath),
		fingerprint,
		parser.ParseOutcome{ResultSetComplete: true},
	)
	provider.findFound = false
	engine := newProcessFixtureEngine(t, root, provider)

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentCowork,
	})

	require.Error(t, res.err)
	assert.ErrorContains(t, res.err, "provider source not found")
	assert.Equal(t, []string{"find-source"}, provider.calls)
}

func TestSyncSingleSessionProviderAuthoritativeBypassesProviderSkipCache(t *testing.T) {

	root := t.TempDir()
	sourcePath, fingerprint := writeProcessProviderSource(t, root, "single.jsonl")
	source := processFixtureSource(sourcePath)
	source.FingerprintKey = sourcePath + "#provider-key"
	provider := newProcessFixtureProvider(
		source,
		fingerprint,
		parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{{
				Result: processFixtureResult(
					"cowork:single",
					parser.AgentCowork,
					"fixture-project",
					sourcePath,
					fingerprint,
				),
				DataVersion: parser.DataVersionCurrent,
			}},
			ResultSetComplete: true,
		},
	)
	engine := newProcessFixtureEngine(t, root, provider)
	engine.cacheSkip(source.FingerprintKey, fingerprint.MTimeNS)

	require.NoError(t, engine.SyncSingleSession("cowork:single"))

	assert.Equal(
		t,
		[]string{
			"find-source",
			"find-source",
			"fingerprint",
			"parse",
		},
		provider.calls,
	)
	require.Len(t, provider.findRequests, 2)
	assert.Equal(t, "single", provider.findRequests[0].RawSessionID)
	assert.False(t, provider.findRequests[1].RequireFreshSource)
	require.Len(t, provider.parseRequests, 1)
	assert.True(t, provider.parseRequests[0].ForceParse)
	engine.skipMu.RLock()
	_, cached := engine.skipCache[source.FingerprintKey]
	engine.skipMu.RUnlock()
	assert.False(t, cached)
}

func TestProcessFileClaudeCachedSourceWithoutStoredSessionSkipsParse(t *testing.T) {
	root := t.TempDir()
	sourcePath, fingerprint := writeProcessProviderSource(t, root, "noninteractive.jsonl")
	fingerprint.Hash = "unchanged-content"
	source := processFixtureSource(sourcePath)
	source.Provider = parser.AgentClaude
	provider := newProcessFixtureProvider(
		source,
		fingerprint,
		parser.ParseOutcome{ResultSetComplete: true},
	)
	provider.Def.Type = parser.AgentClaude
	provider.Def.IDPrefix = "claude:"
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine:           "devbox",
		ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	engine.cacheSkip(providerProcessCacheKey(
		parser.DiscoveredFile{Path: sourcePath, Agent: parser.AgentClaude},
		source, fingerprint,
	), fingerprint.MTimeNS)

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentClaude,
	})

	require.NoError(t, res.err)
	assert.True(t, res.skip)
	assert.Equal(t, []string{"find-source", "fingerprint"}, provider.calls)
	assert.Empty(t, provider.parseRequests)
}

func TestProcessFileRowlessCachedSourceChangedHashReparses(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentCodex} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			sourcePath, fingerprint := writeProcessProviderSource(
				t, root, "became-interactive.jsonl",
			)
			fingerprint.Hash = "new-valid-content"
			source := processFixtureSource(sourcePath)
			source.Provider = agent
			provider := newProcessFixtureProvider(
				source,
				fingerprint,
				parser.ParseOutcome{
					Results: []parser.ParseResultOutcome{{
						Result: processFixtureResult(
							string(agent)+":valid", agent, "fixture-project",
							sourcePath, fingerprint,
						),
						DataVersion: parser.DataVersionCurrent,
					}},
					ResultSetComplete: true,
				},
			)
			provider.Def.Type = agent
			provider.Def.IDPrefix = string(agent) + ":"
			engine := NewEngine(openTestDB(t), EngineConfig{
				AgentDirs: map[parser.AgentType][]string{agent: {root}},
				Machine:   "devbox",
				ProviderFactories: []parser.ProviderFactory{
					processFixtureFactory{provider: provider},
				},
				ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
					agent: parser.ProviderMigrationProviderAuthoritative,
				},
			})
			oldFingerprint := fingerprint
			oldFingerprint.Hash = "old-ignored-content"
			oldKey := providerProcessCacheKey(
				parser.DiscoveredFile{Path: sourcePath, Agent: agent},
				source, oldFingerprint,
			)
			engine.cacheSkip(oldKey, fingerprint.MTimeNS)

			res := engine.processFile(context.Background(), parser.DiscoveredFile{
				Path: sourcePath, Agent: agent,
			})

			require.NoError(t, res.err)
			assert.False(t, res.skip)
			assert.Equal(t, []string{"find-source", "fingerprint", "parse"}, provider.calls)
			require.Len(t, provider.parseRequests, 1)
		})
	}
}

func TestCacheSkipRetainsOnlyLatestSourceHashKey(t *testing.T) {
	engine := &Engine{
		skipCache:        make(map[string]int64),
		skipFingerprints: make(map[string]string),
	}
	const (
		plain = "/archive/session.jsonl"
		base  = plain + "?source_hash="
	)
	engine.cacheSkip(plain, 1)
	engine.cacheSkip(base+"old", 1)
	engine.cacheSkip(base+"new", 1)

	assert.Equal(t, map[string]int64{base + "new": 1}, engine.SnapshotSkipCache())
}

func TestNewEngineNormalizesLegacySourceHashSkipDuplicates(t *testing.T) {
	database := openTestDB(t)
	const (
		plain     = "/archive/session.jsonl"
		hashBase  = plain + "?source_hash="
		unrelated = "/archive/unrelated.jsonl"
	)
	require.NoError(t, database.ReplaceSkippedFiles(map[string]int64{
		plain:            1,
		hashBase + "old": 2,
		hashBase + "new": 3,
		unrelated:        4,
	}))

	engine := NewEngine(database, EngineConfig{})
	t.Cleanup(engine.Close)

	assert.Equal(t, map[string]int64{unrelated: 4}, engine.SnapshotSkipCache(),
		"ambiguous legacy hashes must reparse once instead of choosing a stale key")
}

func TestProcessFileClaudeCachedStoredSessionChangedHashReparses(t *testing.T) {
	root := t.TempDir()
	sourcePath, fingerprint := writeProcessProviderSource(t, root, "stored.jsonl")
	fingerprint.Hash = "new-content"
	source := processFixtureSource(sourcePath)
	source.Provider = parser.AgentClaude
	provider := newProcessFixtureProvider(
		source,
		fingerprint,
		parser.ParseOutcome{ResultSetComplete: true},
	)
	provider.Def.Type = parser.AgentClaude
	provider.Def.IDPrefix = "claude:"
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine:           "devbox",
		ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	stored := processFixtureResult(
		"claude:stored",
		parser.AgentClaude,
		"fixture-project",
		sourcePath,
		fingerprint,
	)
	stored.Session.File.Hash = "old-content"
	written, _, failed, _ := engine.writeBatch(
		[]pendingWrite{{sess: stored.Session, msgs: stored.Messages}},
		syncWriteDefault,
		false,
	)
	require.Equal(t, 1, written)
	require.Zero(t, failed)
	engine.cacheSkip(source.FingerprintKey, fingerprint.MTimeNS)

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentClaude,
	})

	require.NoError(t, res.err)
	assert.False(t, res.skip)
	assert.Equal(t, []string{"find-source", "fingerprint", "parse"}, provider.calls)
	require.Len(t, provider.parseRequests, 1)
}

func TestProcessFileProviderDevinSkipsStoredFreshSource(t *testing.T) {
	root := t.TempDir()
	dbPath, transcriptPath := writeProcessProviderDevinFixture(
		t,
		root,
		"session-001",
		"Initial reply",
		1700000000000,
		1700000005000,
	)
	virtualPath := parser.VirtualSourcePath(dbPath, "session-001")
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentDevin: {root},
		},
		Machine: "devbox",
	})

	first := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  virtualPath,
		Agent: parser.AgentDevin,
	})
	require.NoError(t, first.err)
	require.Len(t, first.results, 1)
	require.Equal(t, virtualPath, engine.FindSourceFile("devin:session-001"))
	storedMtime := first.results[0].Session.File.Mtime
	require.NotZero(t, storedMtime)
	assert.GreaterOrEqual(t, storedMtime, transcriptProcessProviderMtime(t, transcriptPath))

	written, _, failed, _ := engine.writeBatch(
		[]pendingWrite{{
			sess:         first.results[0].Session,
			msgs:         first.results[0].Messages,
			usageEvents:  first.results[0].UsageEvents,
			forceReplace: first.forceReplace,
		}},
		syncWriteDefault,
		false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)

	_, dbStoredMtime, ok := database.GetSessionFileInfo("devin:session-001")
	require.True(t, ok)
	assert.Equal(t, storedMtime, dbStoredMtime)
	assert.Equal(t, storedMtime, engine.SourceMtime("devin:session-001"))

	second := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  virtualPath,
		Agent: parser.AgentDevin,
	})

	require.NoError(t, second.err)
	assert.True(t, second.skip)
	assert.True(t, second.cacheSkip)
	assert.Equal(t, storedMtime, second.mtime)
	assert.Empty(t, second.results)
}

func TestProcessFileProviderDevinReparsesTranscriptOnlyChange(t *testing.T) {
	root := t.TempDir()
	dbPath, transcriptPath := writeProcessProviderDevinFixture(
		t,
		root,
		"session-002",
		"Initial reply",
		1700000000000,
		1700000005000,
	)
	virtualPath := parser.VirtualSourcePath(dbPath, "session-002")
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentDevin: {root},
		},
		Machine: "devbox",
	})

	first := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  virtualPath,
		Agent: parser.AgentDevin,
	})
	require.NoError(t, first.err)
	require.Len(t, first.results, 1)
	written, _, failed, _ := engine.writeBatch(
		[]pendingWrite{{
			sess:         first.results[0].Session,
			msgs:         first.results[0].Messages,
			usageEvents:  first.results[0].UsageEvents,
			forceReplace: first.forceReplace,
		}},
		syncWriteDefault,
		false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)
	before := engine.SourceMtime("devin:session-002")

	future := time.Now().Add(2 * time.Second)
	writeProcessProviderDevinTranscript(t, transcriptPath, "Updated reply")
	require.NoError(t, os.Chtimes(transcriptPath, future, future))

	after := engine.SourceMtime("devin:session-002")
	assert.Greater(t, after, before)

	second := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  virtualPath,
		Agent: parser.AgentDevin,
	})
	require.NoError(t, second.err)
	assert.False(t, second.skip)
	require.Len(t, second.results, 1)
	assert.Greater(t, second.results[0].Session.File.Mtime, first.results[0].Session.File.Mtime)
	assert.Equal(t, "Updated reply", second.results[0].Messages[1].Content)
}

func TestProcessFileProviderDevinSameSizeSameMtimeTranscriptRewriteReparses(t *testing.T) {
	tests := []struct {
		name      string
		seedCache bool
		freshSync bool
	}{
		{name: "skip cache", seedCache: true},
		{name: "db freshness", freshSync: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dbPath, transcriptPath := writeProcessProviderDevinFixture(
				t,
				root,
				"session-same-mtime",
				"Initial reply",
				1700000000000,
				1700000005000,
			)
			virtualPath := parser.VirtualSourcePath(dbPath, "session-same-mtime")
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentDevin: {root},
				},
				Machine: "devbox",
			})

			first := engine.processFile(context.Background(), parser.DiscoveredFile{
				Path:  virtualPath,
				Agent: parser.AgentDevin,
			})
			require.NoError(t, first.err)
			require.Len(t, first.results, 1)
			require.Len(t, first.results[0].Messages, 2)
			assert.Equal(t, "Initial reply", first.results[0].Messages[1].Content)
			initialMtime := first.results[0].Session.File.Mtime
			initialHash := first.results[0].Session.File.Hash
			require.NotZero(t, initialMtime)
			require.NotEmpty(t, initialHash)

			written, _, failed, _ := engine.writeBatch(
				[]pendingWrite{{
					sess:         first.results[0].Session,
					msgs:         first.results[0].Messages,
					usageEvents:  first.results[0].UsageEvents,
					forceReplace: first.forceReplace,
				}},
				syncWriteDefault,
				false,
			)
			require.Equal(t, 0, failed)
			require.Equal(t, 1, written)

			infoBefore, err := os.Stat(transcriptPath)
			require.NoError(t, err)
			writeProcessProviderDevinTranscript(t, transcriptPath, "Changed reply")
			initialTime := time.Unix(0, initialMtime)
			require.NoError(t, os.Chtimes(transcriptPath, initialTime, initialTime))
			infoAfter, err := os.Stat(transcriptPath)
			require.NoError(t, err)
			require.Equal(t, infoBefore.Size(), infoAfter.Size(),
				"test must keep size stable so hash is the only freshness signal")

			if tt.seedCache {
				engine.cacheSkip(virtualPath, initialMtime)
			}
			if tt.freshSync {
				engine = NewEngine(database, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentDevin: {root},
					},
					Machine: "devbox",
				})
			}

			second := engine.processFile(context.Background(), parser.DiscoveredFile{
				Path:  virtualPath,
				Agent: parser.AgentDevin,
			})
			require.NoError(t, second.err)
			assert.False(t, second.skip)
			require.Len(t, second.results, 1)
			require.Len(t, second.results[0].Messages, 2)
			assert.Equal(t, "Changed reply", second.results[0].Messages[1].Content)
			assert.NotEqual(t, initialHash, second.results[0].Session.File.Hash)
		})
	}
}

func TestProcessFileProviderDevinRepeatedHashRewriteIgnoresStaleHashedCache(t *testing.T) {
	root := t.TempDir()
	dbPath, transcriptPath := writeProcessProviderDevinFixture(
		t,
		root,
		"session-repeated-hash",
		"Initial reply",
		1700000000000,
		1700000005000,
	)
	virtualPath := parser.VirtualSourcePath(dbPath, "session-repeated-hash")
	file := parser.DiscoveredFile{
		Path:  virtualPath,
		Agent: parser.AgentDevin,
	}
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentDevin: {root},
		},
		Machine: "devbox",
	})

	first := engine.processFile(context.Background(), file)
	require.NoError(t, first.err)
	require.Len(t, first.results, 1)
	require.Len(t, first.results[0].Messages, 2)
	assert.Equal(t, "Initial reply", first.results[0].Messages[1].Content)
	initialMtime := first.results[0].Session.File.Mtime
	initialHash := first.results[0].Session.File.Hash
	require.NotZero(t, initialMtime)
	require.NotEmpty(t, initialHash)
	require.Contains(t, first.cacheKey, "?source_hash="+initialHash)
	writeProcessProviderDevinResult(t, engine, first)

	engine.cacheSkip(first.cacheKey, initialMtime)

	writeProcessProviderDevinTranscript(t, transcriptPath, "Changed reply")
	initialTime := time.Unix(0, initialMtime)
	require.NoError(t, os.Chtimes(transcriptPath, initialTime, initialTime))
	second := engine.processFile(context.Background(), file)
	require.NoError(t, second.err)
	assert.False(t, second.skip)
	require.Len(t, second.results, 1)
	require.Len(t, second.results[0].Messages, 2)
	assert.Equal(t, "Changed reply", second.results[0].Messages[1].Content)
	changedHash := second.results[0].Session.File.Hash
	require.NotEmpty(t, changedHash)
	require.NotEqual(t, initialHash, changedHash)
	require.Contains(t, second.cacheKey, "?source_hash="+changedHash)
	writeProcessProviderDevinResult(t, engine, second)
	engine.clearSkip(second.cacheKey)

	writeProcessProviderDevinTranscript(t, transcriptPath, "Initial reply")
	require.NoError(t, os.Chtimes(transcriptPath, initialTime, initialTime))

	third := engine.processFile(context.Background(), file)
	require.NoError(t, third.err)
	assert.False(t, third.skip)
	require.Len(t, third.results, 1)
	require.Len(t, third.results[0].Messages, 2)
	assert.Equal(t, "Initial reply", third.results[0].Messages[1].Content)
	assert.Equal(t, initialHash, third.results[0].Session.File.Hash)
}

func TestSyncAllProviderDevinMissingDBPreservesArchive(t *testing.T) {
	root := t.TempDir()
	dbPath, _ := writeProcessProviderDevinFixture(
		t,
		root,
		"session-003",
		"Archived reply",
		1700000000000,
		1700000005000,
	)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentDevin: {root},
		},
		Machine: "devbox",
	})

	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)
	assertProviderProcessMessageContent(
		t,
		database,
		"devin:session-003",
		"Ship it",
		"Archived reply",
	)

	require.NoError(t, os.Remove(dbPath))
	stats = engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 0, stats.Synced)
	assertProviderProcessMessageContent(
		t,
		database,
		"devin:session-003",
		"Ship it",
		"Archived reply",
	)
	assert.Empty(t, engine.FindSourceFile("devin:session-003"))
	assert.Zero(t, engine.SourceMtime("devin:session-003"))
}

func writeProcessProviderForgeDB(t *testing.T, root string) string {
	t.Helper()
	dbPath := filepath.Join(root, ".forge.db")
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	_, err = database.Exec(`
		CREATE TABLE conversations (
			conversation_id TEXT PRIMARY KEY NOT NULL,
			title TEXT,
			workspace_id BIGINT NOT NULL,
			context TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP,
			metrics TEXT
		);
	`)
	require.NoError(t, err)
	_, err = database.Exec(
		`INSERT INTO conversations
			(conversation_id, title, workspace_id, context, created_at, updated_at, metrics)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"conv-001",
		"Provider Process",
		int64(1),
		`{"conversation_id":"conv-001","messages":[`+
			`{"message":{"text":{"role":"User","content":"Run provider process.","raw_content":{"Text":"Run provider process."},"timestamp":"2026-05-02T09:58:16Z"}}},`+
			`{"message":{"text":{"role":"Assistant","content":"Processed through provider.","timestamp":"2026-05-02T09:58:17Z"}}}`+
			`]}`,
		"2026-05-02 09:58:16.000000000",
		"2026-05-02 09:58:17.000000000",
		"",
	)
	require.NoError(t, err)
	return dbPath
}

func writeProcessProviderDevinResult(
	t *testing.T,
	engine *Engine,
	result processResult,
) {
	t.Helper()
	require.Len(t, result.results, 1)
	written, _, failed, _ := engine.writeBatch(
		[]pendingWrite{{
			sess:         result.results[0].Session,
			msgs:         result.results[0].Messages,
			usageEvents:  result.results[0].UsageEvents,
			forceReplace: result.forceReplace,
		}},
		syncWriteDefault,
		false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)
}

func writeProcessProviderDevinFixture(
	t *testing.T,
	root string,
	sessionID string,
	assistantReply string,
	createdAtMS int64,
	lastActivityAtMS int64,
) (string, string) {
	t.Helper()
	cliDir := filepath.Join(root, "cli")
	transcriptsDir := filepath.Join(cliDir, "transcripts")
	require.NoError(t, os.MkdirAll(transcriptsDir, 0o755))
	dbPath := filepath.Join(cliDir, "sessions.db")
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = database.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			title TEXT,
			working_directory TEXT,
			model TEXT,
			created_at INTEGER,
			last_activity_at INTEGER,
			hidden INTEGER NOT NULL DEFAULT 0
		);
	`)
	require.NoError(t, err)
	_, err = database.Exec(
		`INSERT INTO sessions
			(id, title, working_directory, model, created_at, last_activity_at, hidden)
		 VALUES (?, ?, ?, ?, ?, ?, 0)`,
		sessionID,
		"Devin fixture",
		"/Users/devbox/src/agentsview",
		"devin-1",
		createdAtMS,
		lastActivityAtMS,
	)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	transcriptPath := filepath.Join(transcriptsDir, sessionID+".json")
	writeProcessProviderDevinTranscript(t, transcriptPath, assistantReply)
	return dbPath, transcriptPath
}

func writeProcessProviderDevinTranscript(
	t *testing.T,
	transcriptPath string,
	assistantReply string,
) {
	t.Helper()
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{
		"created_at":"2024-01-01T00:00:00Z",
		"updated_at":"2024-01-01T00:00:05Z",
		"agent":{"model_name":"devin-1"},
		"steps":[
			{
				"step_id":"step-user",
				"source":"user",
				"timestamp":"2024-01-01T00:00:01Z",
				"message":"Ship it"
			},
			{
				"step_id":"step-agent",
				"source":"agent",
				"timestamp":"2024-01-01T00:00:05Z",
				"message":"`+assistantReply+`"
			}
		]
	}`), 0o644))
}

func transcriptProcessProviderMtime(t *testing.T, transcriptPath string) int64 {
	t.Helper()
	info, err := os.Stat(transcriptPath)
	require.NoError(t, err)
	return info.ModTime().UnixNano()
}

func assertProviderProcessMessageContent(
	t *testing.T,
	database *db.DB,
	sessionID string,
	want ...string,
) {
	t.Helper()
	msgs, err := database.GetAllMessages(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, len(want))
	for i, content := range want {
		assert.Equal(t, content, msgs[i].Content)
	}
}

func newProcessFixtureEngine(
	t *testing.T,
	root string,
	provider *processFixtureProvider,
) *Engine {
	t.Helper()
	return NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCowork: {root},
		},
		Machine:           "devbox",
		ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
		},
	})
}

func writeProcessProviderSource(
	t *testing.T,
	root string,
	name string,
) (string, parser.SourceFingerprint) {
	t.Helper()
	path := filepath.Join(root, name)
	require.NoError(t, os.WriteFile(path, []byte(`{"session":"fixture"}`+"\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)
	return path, parser.SourceFingerprint{
		Key:     path,
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}
}

func processFixtureSource(path string) parser.SourceRef {
	return parser.SourceRef{
		Provider:       parser.AgentCowork,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    "fixture-project",
	}
}

func processFixtureResult(
	id string,
	agent parser.AgentType,
	project string,
	path string,
	fingerprint parser.SourceFingerprint,
) parser.ParseResult {
	started := time.Unix(1_800_000_000, 0).UTC()
	ended := started.Add(time.Second)
	return parser.ParseResult{
		Session: parser.ParsedSession{
			ID:               id,
			Project:          project,
			Machine:          "devbox",
			Agent:            agent,
			StartedAt:        started,
			EndedAt:          ended,
			FirstMessage:     "fixture prompt",
			MessageCount:     1,
			UserMessageCount: 1,
			File: parser.FileInfo{
				Path:  path,
				Size:  fingerprint.Size,
				Mtime: fingerprint.MTimeNS,
			},
		},
		Messages: []parser.ParsedMessage{{
			Ordinal:   0,
			Role:      parser.RoleUser,
			Content:   "fixture prompt",
			Timestamp: started,
		}},
	}
}

func newProcessFixtureProvider(
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	outcome parser.ParseOutcome,
) *processFixtureProvider {
	return &processFixtureProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{
				Type:        parser.AgentCowork,
				DisplayName: "Cowork",
				IDPrefix:    "cowork:",
				FileBased:   true,
			},
			Caps: parser.Capabilities{
				Source: parser.SourceCapabilities{
					FindSource:           parser.CapabilitySupported,
					CompositeFingerprint: parser.CapabilitySupported,
				},
			},
		},
		source:      source,
		findFound:   true,
		fingerprint: fingerprint,
		outcome:     outcome,
	}
}

type processFixtureFactory struct {
	provider *processFixtureProvider
}

func (f processFixtureFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f processFixtureFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f processFixtureFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

type processFixtureProvider struct {
	parser.ProviderBase

	source        parser.SourceRef
	findFound     bool
	fingerprint   parser.SourceFingerprint
	outcome       parser.ParseOutcome
	calls         []string
	findRequests  []parser.FindSourceRequest
	parseRequests []parser.ParseRequest
}

func (p *processFixtureProvider) FindSource(
	_ context.Context,
	req parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	p.calls = append(p.calls, "find-source")
	p.findRequests = append(p.findRequests, req)
	if !p.findFound {
		return parser.SourceRef{}, false, nil
	}
	return p.source, true, nil
}

func (p *processFixtureProvider) Fingerprint(
	context.Context,
	parser.SourceRef,
) (parser.SourceFingerprint, error) {
	p.calls = append(p.calls, "fingerprint")
	return p.fingerprint, nil
}

func (p *processFixtureProvider) Parse(
	_ context.Context,
	req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	p.calls = append(p.calls, "parse")
	p.parseRequests = append(p.parseRequests, req)
	return p.outcome, nil
}

func openProcessProviderPiebaldDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	copyProcessProviderPiebaldSchemaTemplate(t, path)
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	return database
}

func copyProcessProviderPiebaldSchemaTemplate(t *testing.T, path string) {
	t.Helper()
	processProviderPiebaldSchemaOnce.Do(func() {
		processProviderPiebaldSchemaBytes, processProviderPiebaldSchemaErr =
			buildProcessProviderPiebaldSchemaTemplate()
	})
	require.NoError(t, processProviderPiebaldSchemaErr)
	require.NoError(t, os.WriteFile(path, processProviderPiebaldSchemaBytes, 0o644))
}

func buildProcessProviderPiebaldSchemaTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "agentsview-process-piebald-schema-*")
	if err != nil {
		return nil, fmt.Errorf("create piebald provider schema dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "template.db")
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open piebald provider schema template: %w", err)
	}
	if _, err = database.Exec(processProviderPiebaldSchema); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("create piebald provider schema template: %w", err)
	}
	if err = database.Close(); err != nil {
		return nil, fmt.Errorf("close piebald provider schema template: %w", err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read piebald provider schema template: %w", err)
	}
	return bytes, nil
}

func seedProcessProviderPiebaldChat(t *testing.T, database *sql.DB) {
	t.Helper()
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO projects (id, directory, name) VALUES (1, '/repo/app', 'app')`,
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO chats
			(id, title, created_at, updated_at, is_deleted, message_count,
			 current_directory, branch_name, project_id)
		 VALUES (42, 'Provider Process', '2026-05-01T10:00:00Z',
			 '2026-05-01T10:05:00Z', 0, 2, '/repo/app', 'main', 1)`,
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO messages
			(id, parent_chat_id, role, model, created_at, updated_at, status)
		 VALUES (100, 42, 'user', '', '2026-05-01T10:00:01Z',
			 '2026-05-01T10:00:01Z', 'completed')`,
	)
	seedProcessProviderPiebaldTextPart(
		t, database, 200, 100, 0, "Use the provider parser.",
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO messages
			(id, parent_chat_id, role, model, created_at, updated_at,
			 input_tokens, output_tokens, cache_read_tokens, status, finish_reason)
		 VALUES (101, 42, 'assistant', 'claude-test',
			 '2026-05-01T10:00:02Z', '2026-05-01T10:00:03Z',
			 10, 20, 5, 'completed', 'end_turn')`,
	)
	seedProcessProviderPiebaldTextPart(
		t, database, 201, 101, 0, "Provider parse complete.",
	)
}

func seedProcessProviderPiebaldTextPart(
	t *testing.T,
	database *sql.DB,
	partID, msgID int64,
	idx int,
	text string,
) {
	t.Helper()
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO message_parts
			(id, parent_chat_message_id, part_index, part_type)
		 VALUES (?, ?, ?, 'text')`,
		partID, msgID, idx,
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO message_part_text
			(message_part_id, is_thinking)
		 VALUES (?, 0)`,
		partID,
	)
	nodeID := partID + 100000
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO message_content_nodes
			(id, parent_text_part_id, node_index, node_type)
		 VALUES (?, ?, 0, 'text')`,
		nodeID, partID,
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO message_node_text
			(node_id, content)
		 VALUES (?, ?)`,
		nodeID, text,
	)
}

func openProcessProviderWarpDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	_, err = database.Exec(`
		CREATE TABLE agent_conversations (
			id INTEGER PRIMARY KEY NOT NULL,
			conversation_id TEXT NOT NULL,
			conversation_data TEXT NOT NULL,
			last_modified_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE UNIQUE INDEX ux_agent_conversations_conversation_id
			ON agent_conversations (conversation_id);

		CREATE TABLE ai_queries (
			id INTEGER PRIMARY KEY NOT NULL,
			exchange_id TEXT NOT NULL,
			conversation_id TEXT NOT NULL,
			start_ts DATETIME NOT NULL,
			input TEXT NOT NULL,
			working_directory TEXT,
			output_status TEXT NOT NULL,
			model_id TEXT NOT NULL DEFAULT '',
			planning_model_id TEXT NOT NULL DEFAULT '',
			coding_model_id TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX ux_ai_queries_exchange_id
			ON ai_queries(exchange_id);
	`)
	require.NoError(t, err)
	return database
}

func seedProcessProviderWarpConversation(t *testing.T, database *sql.DB) {
	t.Helper()
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO agent_conversations
			(conversation_id, conversation_data, last_modified_at)
		 VALUES (?, ?, ?)`,
		"conv-001",
		`{
			"conversation_usage_metadata":{
				"token_usage":[
					{"model_id":"Claude Opus 4","warp_tokens":1000,"byok_tokens":0}
				]
			}
		}`,
		"2026-04-07 10:00:00",
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO ai_queries
			(exchange_id, conversation_id, start_ts, input, working_directory,
			 output_status, model_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"ex-001",
		"conv-001",
		"2026-04-07 09:50:00.000000",
		`[{"Query":{"text":"Use the provider parser.","context":[]}}]`,
		"/repo/app",
		`"Completed"`,
		"auto-genius",
	)
}

func writeProcessProviderZCodeDB(t *testing.T, cliRoot string) string {
	t.Helper()
	dbDir := filepath.Join(cliRoot, "db")
	require.NoError(t, os.MkdirAll(dbDir, 0o755))
	dbPath := filepath.Join(dbDir, "db.sqlite")
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	mustExecProcessProviderSQL(t, database, `
		CREATE TABLE session (
			id TEXT PRIMARY KEY NOT NULL,
			project_id TEXT,
			workspace_id TEXT,
			directory TEXT,
			title TEXT,
			time_created TEXT,
			time_updated TEXT
		);
		CREATE TABLE model_usage (
			session_id TEXT NOT NULL,
			turn_id TEXT,
			provider_id TEXT,
			model_id TEXT,
			status TEXT,
			input_tokens INTEGER,
			output_tokens INTEGER,
			reasoning_tokens INTEGER,
			cache_creation_input_tokens INTEGER,
			cache_read_input_tokens INTEGER,
			computed_total_tokens INTEGER,
			started_at TEXT,
			completed_at TEXT,
			duration_ms INTEGER,
			tool_call_count INTEGER
		);
		CREATE TABLE message (
			id TEXT PRIMARY KEY NOT NULL,
			session_id TEXT NOT NULL,
			time_created TEXT,
			data TEXT
		);
		CREATE TABLE part (
			id TEXT PRIMARY KEY NOT NULL,
			message_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			data TEXT
		);
	`)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO session (
			id, project_id, workspace_id, directory, title,
			time_created, time_updated
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"session-001", "project-7", "workspace-19", "/Users/alice/code/acme-app",
		"Acme session", "2026-07-06T13:00:01Z", "2026-07-06T13:05:00Z",
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO model_usage (
			session_id, turn_id, provider_id, model_id, status,
			input_tokens, output_tokens, reasoning_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			computed_total_tokens, started_at, completed_at,
			duration_ms, tool_call_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"session-001", "1", "builtin:bigmodel-coding-plan", "claude-sonnet-4-6", "done",
		int64(1), int64(2), int64(0), int64(0), int64(0), int64(3),
		"2026-07-06T13:00:02Z", "2026-07-06T13:00:03Z", int64(1000), int64(1),
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO message (
			id, session_id, time_created, data
		) VALUES (?, ?, ?, ?)`,
		"msg-1", "session-001", "2026-07-06T13:00:01Z", `{"role":"user"}`,
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO part (
			id, message_id, session_id, data
		) VALUES (?, ?, ?, ?)`,
		"part-1", "msg-1", "session-001", `{"type":"text","text":"Inspect the auth flow."}`,
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO message (
			id, session_id, time_created, data
		) VALUES (?, ?, ?, ?)`,
		"msg-2", "session-001", "2026-07-06T13:00:02Z",
		`{"role":"assistant","modelID":"claude-sonnet-4-6"}`,
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO part (
			id, message_id, session_id, data
		) VALUES (?, ?, ?, ?)`,
		"part-2", "msg-2", "session-001",
		`{"type":"tool_use","id":"call-read","name":"Read","input":{"file_path":"auth.go"}}`,
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO message (
			id, session_id, time_created, data
		) VALUES (?, ?, ?, ?)`,
		"msg-3", "session-001", "2026-07-06T13:00:03Z", `{"role":"user"}`,
	)
	mustExecProcessProviderSQL(t, database,
		`INSERT INTO part (
			id, message_id, session_id, data
		) VALUES (?, ?, ?, ?)`,
		"part-3", "msg-3", "session-001",
		`{"type":"tool_result","tool_use_id":"call-read","content":"package auth"}`,
	)
	return dbPath
}

func mustExecProcessProviderSQL(
	t *testing.T,
	database *sql.DB,
	query string,
	args ...any,
) {
	t.Helper()
	_, err := database.Exec(query, args...)
	require.NoError(t, err)
}
