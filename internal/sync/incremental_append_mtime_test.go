package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestIncrementalAppendStoresMtimeForTheParsedOffset covers a transcript that
// grows between the engine's pre-parse stat and the parser's read. The stored
// file_size then covers the later state while file_mtime still describes the
// earlier one, so the next sync over the unchanged file sees a matching size
// with a newer mtime and re-parses the whole transcript.
func TestIncrementalAppendStoresMtimeForTheParsedOffset(t *testing.T) {
	ctx := t.Context()
	database := openTestDB(t)
	root := t.TempDir()
	projectDir := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "raced-append.jsonl")

	builder := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "start", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "ok", "b", "a")
	initial := builder.String()
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	engine := NewEngine(ctx, database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(ctx, nil).Synced)

	// The append the next sync reacts to. The engine stats the file before
	// parsing, so info1 is the size and mtime it carries into the parse.
	builder.AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "next", "c", "b")
	firstAppend := builder.String()
	appendFile(t, path, firstAppend[len(initial):])
	preParseMtime := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	require.NoError(t, os.Chtimes(path, preParseMtime, preParseMtime))
	info1, err := os.Stat(path)
	require.NoError(t, err)

	// Grow the transcript again inside the stat-to-read window. The engine's
	// path rewriter runs after tryProviderIncrementalAppend's stat and before
	// the provider reads, which is exactly that window.
	builder.AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "done", "d", "c")
	secondAppend := builder.String()
	readMtime := preParseMtime.Add(time.Second)
	grown := false
	engine.pathRewriter = func(p string) string {
		if !grown {
			grown = true
			appendFile(t, path, secondAppend[len(firstAppend):])
			require.NoError(t, os.Chtimes(path, readMtime, readMtime))
		}
		return p
	}

	provider, ok := parser.NewProvider(
		parser.AgentClaude,
		parser.ProviderConfig{Roots: []string{root}, Machine: "local"},
	)
	require.True(t, ok)
	pathRef := parser.SourceRef{
		Provider: parser.AgentClaude, Key: path,
		DisplayPath: path, FingerprintKey: path,
	}
	result, applied := engine.tryProviderIncrementalAppend(
		ctx, provider, pathRef,
		parser.DiscoveredFile{Agent: parser.AgentClaude, Path: path},
		parser.SourceFingerprint{
			Key: path, Size: info1.Size(),
			MTimeNS: info1.ModTime().UnixNano(),
		},
		nil, "", nil, nil,
	)
	require.True(t, applied, "incremental append must apply")
	require.True(t, grown,
		"the rewriter must run between the stat and the provider read")
	require.NotNil(t, result.incremental)

	postRead, err := os.Stat(path)
	require.NoError(t, err)
	require.Greater(t, postRead.Size(), info1.Size(),
		"the file must have grown after the pre-parse stat")

	assert.Equal(t, postRead.Size(), result.incremental.fileSize,
		"the stored offset covers the bytes the parser read")
	assert.Equal(t, postRead.ModTime().UnixNano(),
		result.incremental.fileMtime,
		"the stored mtime must describe the same moment as the offset")

	require.NoError(t, engine.writeIncremental(ctx, result.incremental))
	assert.True(t,
		engine.shouldSkipFile(ctx, result.incremental.sessionID, postRead),
		"an unchanged file must skip on the next sync, not full-parse",
	)
}

// TestIncrementalAppendUnchangedSourceStillSkips guards the ordinary append
// path: when nothing writes during the parse, the stored size and mtime still
// describe the same moment and the next sync skips the unchanged transcript.
func TestIncrementalAppendUnchangedSourceStillSkips(t *testing.T) {
	ctx := t.Context()
	database := openTestDB(t)
	root := t.TempDir()
	projectDir := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "steady-append.jsonl")

	builder := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "start", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "ok", "b", "a")
	initial := builder.String()
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	engine := NewEngine(ctx, database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(ctx, nil).Synced)

	builder.AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "next", "c", "b")
	appendFile(t, path, builder.String()[len(initial):])
	appendedAt := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	require.NoError(t, os.Chtimes(path, appendedAt, appendedAt))

	stats := engine.SyncAll(ctx, nil)
	require.Equal(t, 1, stats.Synced)
	info, err := os.Stat(path)
	require.NoError(t, err)

	inc, found := database.GetSessionForIncremental(
		ctx, path, string(parser.AgentClaude),
	)
	require.True(t, found)
	storedSize, storedMtime, ok := database.GetSessionFileInfo(ctx, inc.ID)
	require.True(t, ok)
	assert.Equal(t, info.Size(), storedSize)
	assert.Equal(t, info.ModTime().UnixNano(), storedMtime)
	assert.True(t, engine.shouldSkipFile(ctx, inc.ID, info),
		"a transcript that did not change must still skip",
	)

	unchanged := engine.SyncAll(ctx, nil)
	assert.Equal(t, 1, unchanged.Skipped,
		"the second sync must skip the unchanged transcript")
	assert.Zero(t, unchanged.Synced)
}
