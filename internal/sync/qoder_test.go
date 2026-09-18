package sync

import (
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
	assert := assert.New(t)
	require := require.New(t)

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
		require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(os.WriteFile(path, []byte("{}\n"), 0o644))
	}

	files := requireClassifyPaths(t, engine, []string{mainPath})
	require.Len(files, 1, "main path did not classify")
	got := files[0]
	assert.Equal(mainPath, got.Path)
	assert.Equal("project", got.Project)
	assert.Equal(parser.AgentQoder, got.Agent)

	files = requireClassifyPaths(t, engine, []string{subPath})
	require.Len(files, 1, "subagent path did not classify")
	got = files[0]
	assert.Equal(subPath, got.Path)
	assert.Equal("project", got.Project)
	assert.Equal(parser.AgentQoder, got.Agent)

	files = requireClassifyPaths(t, engine, []string{sidecarPath})
	require.Len(files, 1, "sidecar path did not map to transcript")
	got = files[0]
	assert.Equal(mainPath, got.Path)
	assert.Equal("project", got.Project)
	assert.Equal(parser.AgentQoder, got.Agent)

	for _, path := range []string{statsPath, rootAgentPath, nestedPath} {
		files = requireClassifyPaths(t, engine, []string{path})
		assert.Emptyf(files, "%s classified as %+v", path, files)
	}
}

func TestEngineClassifyQoderProjectNamedSubagentsAsMainSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQoder: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(root, "subagents", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(os.WriteFile(path, []byte("{}\n"), 0o644))

	files := requireClassifyPaths(t, engine, []string{path})
	require.Len(files, 1, "path did not classify")
	got := files[0]
	assert.Equal(path, got.Path)
	assert.Equal("subagents", got.Project)
	assert.Equal(parser.AgentQoder, got.Agent)
}

func TestEngineSyncQoderSameMessageIDAppendForceReplaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	initial := `{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:20.000Z","message":{"role":"user","content":"hello"},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-06-04T09:47:21.000Z","message":{"id":"msg_split","role":"assistant","model":"auto","stop_reason":"tool_use","content":[{"type":"text","text":"Hello"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
`
	require.NoError(os.WriteFile(path, []byte(initial), 0o644))

	engine.SyncAll(t.Context(), nil)
	msgs, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(err)
	require.Len(msgs, 2)
	assert.Equal("Hello", msgs[1].Content)

	appended := `{"type":"assistant","uuid":"a2","parentUuid":"a1","timestamp":"2026-06-04T09:47:22.000Z","message":{"id":"msg_split","role":"assistant","model":"auto","stop_reason":"end_turn","content":[{"type":"text","text":"Hello world"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
`
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, writeErr := f.WriteString(appended)
	require.NoError(writeErr)
	require.NoError(f.Close())

	engine.SyncPaths([]string{path})
	msgs, err = database.GetAllMessages(t.Context(), sessionID)
	require.NoError(err)
	require.Len(msgs, 2)
	assert.Equal("Hello world", msgs[1].Content)
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
			assert := assert.New(t)
			require := require.New(t)

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
			require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"hello"},"sessionId":"11111111-1111-4111-8111-111111111111"}
`), 0o644))
			initialSidecar := []byte(`{"title":"Initial title","working_dir":"/tmp/qoder-a"}`)
			changedSidecar := []byte(`{"title":"Changed title","working_dir":"/tmp/qoder-a"}`)
			require.Len(changedSidecar, len(initialSidecar),
				"test must keep size stable so hash is the only freshness signal")
			require.NoError(os.WriteFile(sidecarPath, initialSidecar, 0o644))
			initialTime := time.Date(2026, time.June, 4, 10, 0, 0, 0, time.UTC)
			require.NoError(os.Chtimes(path, initialTime, initialTime))
			require.NoError(os.Chtimes(sidecarPath, initialTime, initialTime))

			file := parser.DiscoveredFile{
				Path:            path,
				Agent:           parser.AgentQoder,
				ProviderProcess: true,
			}
			first := engine.processFile(t.Context(), file)
			require.NoError(first.err)
			require.Len(first.results, 1)
			initialMtime := first.results[0].Session.File.Mtime
			initialHash := first.results[0].Session.File.Hash
			require.Equal(initialTime.UnixNano(), initialMtime)
			require.NotEmpty(initialHash)
			require.Contains(first.cacheKey, "?source_hash="+initialHash)
			writeProcessQoderResult(t, engine, first)

			require.NoError(os.WriteFile(sidecarPath, changedSidecar, 0o644))
			require.NoError(os.Chtimes(sidecarPath, initialTime, initialTime))

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

			second := engine.processFile(t.Context(), file)
			require.NoError(second.err)
			assert.False(second.skip)
			require.Len(second.results, 1)
			assert.Equal("Changed title", second.results[0].Session.SessionName)
			assert.Equal("/tmp/qoder-a", second.results[0].Session.Cwd)
			changedHash := second.results[0].Session.File.Hash
			require.NotEmpty(changedHash)
			require.NotEqual(initialHash, changedHash)
			require.Contains(second.cacheKey, "?source_hash="+changedHash)
		})
	}
}

func TestSourceMtimeQoderIncludesSidecarMtime(t *testing.T) {
	require := require.New(t)

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
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"hello"},"sessionId":"11111111-1111-4111-8111-111111111111"}
`), 0o644))
	require.NoError(os.WriteFile(
		sidecarPath,
		[]byte(`{"title":"Sidecar title","working_dir":"/tmp/qoder"}`),
		0o644,
	))

	transcriptTime := time.Date(2026, time.June, 4, 10, 0, 0, 0, time.UTC)
	sidecarTime := transcriptTime.Add(5 * time.Minute)
	require.NoError(os.Chtimes(path, transcriptTime, transcriptTime))
	require.NoError(os.Chtimes(sidecarPath, sidecarTime, sidecarTime))

	assert.Equal(t, sidecarTime.UnixNano(), engine.SourceMtime(t.Context(), "qoder:"+rawID))
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
