package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestEngineClassifyQoderPaths(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQoder: {root},
		},
		Machine: "local",
	})

	mainPath := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111.jsonl")
	subPath := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	sidecarPath := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111-session.json")
	statsPath := filepath.Join(root, "ai-stats", "usage.json")
	rootAgentPath := filepath.Join(root, "-Users-alice-project", "agent-123.jsonl")
	nestedPath := filepath.Join(root, "-Users-alice-project", "notes", "stray.jsonl")
	for _, path := range []string{mainPath, subPath, sidecarPath, statsPath, rootAgentPath, nestedPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	}

	files := requireClassifyPaths(t, engine, []string{mainPath})
	require.Len(t, files, 1, "main path did not classify")
	got := files[0]
	assert.Equal(t, mainPath, got.Path)
	assert.Equal(t, "project", got.Project)
	assert.Equal(t, parser.AgentQoder, got.Agent)

	files = requireClassifyPaths(t, engine, []string{subPath})
	require.Len(t, files, 1, "subagent path did not classify")
	got = files[0]
	assert.Equal(t, subPath, got.Path)
	assert.Equal(t, "project", got.Project)
	assert.Equal(t, parser.AgentQoder, got.Agent)

	files = requireClassifyPaths(t, engine, []string{sidecarPath})
	require.Len(t, files, 1, "sidecar path did not map to transcript")
	got = files[0]
	assert.Equal(t, mainPath, got.Path)
	assert.Equal(t, "project", got.Project)
	assert.Equal(t, parser.AgentQoder, got.Agent)

	for _, path := range []string{statsPath, rootAgentPath, nestedPath} {
		files = requireClassifyPaths(t, engine, []string{path})
		assert.Emptyf(t, files, "%s classified as %+v", path, files)
	}
}

func TestEngineClassifyQoderProjectNamedSubagentsAsMainSession(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQoder: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(root, "subagents", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))

	files := requireClassifyPaths(t, engine, []string{path})
	require.Len(t, files, 1, "path did not classify")
	got := files[0]
	assert.Equal(t, path, got.Path)
	assert.Equal(t, "subagents", got.Project)
	assert.Equal(t, parser.AgentQoder, got.Agent)
}

func TestEngineSyncQoderSameMessageIDAppendForceReplaces(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQoder: {root},
		},
		Machine: "local",
	})

	rawID := "11111111-1111-4111-8111-111111111111"
	sessionID := "qoder:" + rawID
	path := filepath.Join(root, "-Users-alice-project", rawID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	initial := `{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:20.000Z","message":{"role":"user","content":"hello"},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-06-04T09:47:21.000Z","message":{"id":"msg_split","role":"assistant","model":"auto","stop_reason":"tool_use","content":[{"type":"text","text":"Hello"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
`
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	engine.SyncAll(context.Background(), nil)
	msgs, err := database.GetAllMessages(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Hello", msgs[1].Content)

	appended := `{"type":"assistant","uuid":"a2","parentUuid":"a1","timestamp":"2026-06-04T09:47:22.000Z","message":{"id":"msg_split","role":"assistant","model":"auto","stop_reason":"end_turn","content":[{"type":"text","text":"Hello world"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
`
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, writeErr := f.WriteString(appended)
	require.NoError(t, writeErr)
	require.NoError(t, f.Close())

	engine.SyncPaths([]string{path})
	msgs, err = database.GetAllMessages(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Hello world", msgs[1].Content)
}

func TestProcessFileQoderSameSizeSameMtimeSidecarRewriteReparses(t *testing.T) {
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
			database := openTestDB(t)
			root := t.TempDir()
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentQoder: {root},
				},
				Machine: "local",
			})

			rawID := "11111111-1111-4111-8111-111111111111"
			path := filepath.Join(root, "-Users-alice-project", rawID+".jsonl")
			sidecarPath := strings.TrimSuffix(path, ".jsonl") + "-session.json"
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"hello"},"sessionId":"11111111-1111-4111-8111-111111111111"}
`), 0o644))
			initialSidecar := []byte(`{"title":"Initial title","working_dir":"/tmp/qoder-a"}`)
			changedSidecar := []byte(`{"title":"Changed title","working_dir":"/tmp/qoder-a"}`)
			require.Len(t, changedSidecar, len(initialSidecar),
				"test must keep size stable so hash is the only freshness signal")
			require.NoError(t, os.WriteFile(sidecarPath, initialSidecar, 0o644))
			initialTime := time.Date(2026, time.June, 4, 10, 0, 0, 0, time.UTC)
			require.NoError(t, os.Chtimes(path, initialTime, initialTime))
			require.NoError(t, os.Chtimes(sidecarPath, initialTime, initialTime))

			file := parser.DiscoveredFile{
				Path:            path,
				Agent:           parser.AgentQoder,
				ProviderProcess: true,
			}
			first := engine.processFile(context.Background(), file)
			require.NoError(t, first.err)
			require.Len(t, first.results, 1)
			initialMtime := first.results[0].Session.File.Mtime
			initialHash := first.results[0].Session.File.Hash
			require.Equal(t, initialTime.UnixNano(), initialMtime)
			require.NotEmpty(t, initialHash)
			require.Contains(t, first.cacheKey, "?source_hash="+initialHash)
			writeProcessQoderResult(t, engine, first)

			require.NoError(t, os.WriteFile(sidecarPath, changedSidecar, 0o644))
			require.NoError(t, os.Chtimes(sidecarPath, initialTime, initialTime))

			if tt.seedCache {
				engine.cacheSkip(first.cacheKey, initialMtime)
			}
			if tt.freshSync {
				engine = NewEngine(database, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentQoder: {root},
					},
					Machine: "local",
				})
			}

			second := engine.processFile(context.Background(), file)
			require.NoError(t, second.err)
			assert.False(t, second.skip)
			require.Len(t, second.results, 1)
			assert.Equal(t, "Changed title", second.results[0].Session.SessionName)
			assert.Equal(t, "/tmp/qoder-a", second.results[0].Session.Cwd)
			changedHash := second.results[0].Session.File.Hash
			require.NotEmpty(t, changedHash)
			require.NotEqual(t, initialHash, changedHash)
			require.Contains(t, second.cacheKey, "?source_hash="+changedHash)
		})
	}
}

func TestSourceMtimeQoderIncludesSidecarMtime(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQoder: {root},
		},
		Machine: "local",
	})

	rawID := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(root, "-Users-alice-project", rawID+".jsonl")
	sidecarPath := strings.TrimSuffix(path, ".jsonl") + "-session.json"
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"hello"},"sessionId":"11111111-1111-4111-8111-111111111111"}
`), 0o644))
	require.NoError(t, os.WriteFile(
		sidecarPath,
		[]byte(`{"title":"Sidecar title","working_dir":"/tmp/qoder"}`),
		0o644,
	))

	transcriptTime := time.Date(2026, time.June, 4, 10, 0, 0, 0, time.UTC)
	sidecarTime := transcriptTime.Add(5 * time.Minute)
	require.NoError(t, os.Chtimes(path, transcriptTime, transcriptTime))
	require.NoError(t, os.Chtimes(sidecarPath, sidecarTime, sidecarTime))

	assert.Equal(t, sidecarTime.UnixNano(), engine.SourceMtime("qoder:"+rawID))
}

func writeProcessQoderResult(
	t *testing.T,
	engine *Engine,
	result processResult,
) {
	t.Helper()
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
