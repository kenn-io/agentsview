package sync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func missingS3ObjectError() error {
	return minio.ErrorResponse{
		Code:    minio.NoSuchKey,
		Message: "not found",
	}
}

func TestProcessFileS3UsesObjectMetadataToSkipBeforeFetch(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	path := "s3://bucket/root/codex/rollout-2026-06-24T00-00-00-" +
		uuid + ".jsonl"
	size := int64(2048)
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	sess := db.Session{
		ID:        "laptop~codex:" + uuid,
		Project:   "agentsview",
		Machine:   "laptop",
		Agent:     "codex",
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(size),
		FileMtime: int64Ptr(mtime),
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	var fetched bool
	fetchS3Object = func(string) (io.ReadCloser, error) {
		fetched = true
		return io.NopCloser(strings.NewReader("")), nil
	}

	e := &Engine{db: database}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentCodex,
		Path:        path,
		Machine:     "laptop",
		SourceSize:  size,
		SourceMtime: mtime,
	})
	require.NoError(t, res.err)
	assert.True(t, res.skip, "unchanged S3 object should skip")
	assert.False(t, fetched, "unchanged S3 object should not be fetched")
}

func TestProcessFileS3SameMetadataDifferentURIRewritesSourcePath(t *testing.T) {
	database := openTestDB(t)
	oldPath := "s3://bucket/laptop/raw/claude/test-proj/path-change.jsonl"
	newPath := "s3://bucket/laptop/raw/claude-copy/test-proj/path-change.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()
	mtime := time.Date(2026, 6, 24, 12, 0, 30, 0, time.UTC).UnixNano()

	sess := db.Session{
		ID:        "laptop~path-change",
		Project:   "test-proj",
		Machine:   "laptop",
		Agent:     "claude",
		FilePath:  strPtr(oldPath),
		FileSize:  int64Ptr(int64(len(content))),
		FileMtime: int64Ptr(mtime),
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	var fetched bool
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, newPath, got)
		fetched = true
		return io.NopCloser(strings.NewReader(content)), nil
	}

	e := &Engine{db: database}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        newPath,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: mtime,
	})
	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.True(t, fetched)
	require.Len(t, res.results, 1)

	written, _, failed := e.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	updated, err := database.GetSessionFull(
		context.Background(), "laptop~path-change",
	)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, newPath, derefString(updated.FilePath))
}

func TestProcessFileS3FetchErrorDoesNotCacheSkip(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/fetch-fails.jsonl"
	mtime := time.Date(2026, 6, 24, 12, 1, 0, 0, time.UTC).UnixNano()

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, path, got)
		return nil, errors.New("temporary s3 failure")
	}

	e := &Engine{db: database}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  123,
		SourceMtime: mtime,
	})

	require.Error(t, res.err)
	assert.True(t, res.cacheSkip)
	assert.Equal(t, mtime, res.mtime)
	assert.True(t, res.noCacheSkip)
}

func TestDedupeClaudeS3IncludesSourceMachine(t *testing.T) {
	e := &Engine{db: openTestDB(t)}
	files := []parser.DiscoveredFile{
		{
			Agent:   parser.AgentClaude,
			Path:    "s3://bucket/laptop/raw/claude/proj/shared-id.jsonl",
			Machine: "laptop",
		},
		{
			Agent:   parser.AgentClaude,
			Path:    "s3://bucket/server/raw/claude/proj/shared-id.jsonl",
			Machine: "server",
		},
	}

	got := e.dedupeClaudeDiscoveredFiles(files)

	require.Len(t, got, 2)
	assert.ElementsMatch(t, []string{
		"s3://bucket/laptop/raw/claude/proj/shared-id.jsonl",
		"s3://bucket/server/raw/claude/proj/shared-id.jsonl",
	}, []string{got[0].Path, got[1].Path})
}

func TestDedupeCodexS3IncludesSourceMachine(t *testing.T) {
	const uuid = "11111111-1111-4111-8111-111111111111"
	files := []parser.DiscoveredFile{
		{
			Agent: parser.AgentCodex,
			Path: "s3://bucket/laptop/raw/codex/2026/06/24/" +
				"rollout-2026-06-24T00-00-00-" + uuid + ".jsonl",
			Machine: "laptop",
		},
		{
			Agent: parser.AgentCodex,
			Path: "s3://bucket/server/raw/codex/2026/06/24/" +
				"rollout-2026-06-24T00-00-00-" + uuid + ".jsonl",
			Machine: "server",
		},
	}

	got := dedupeDiscoveredFiles(files)

	require.Len(t, got, 2)
	assert.ElementsMatch(t, []string{files[0].Path, files[1].Path}, []string{
		got[0].Path,
		got[1].Path,
	})
}

func TestSyncS3MachineFromRootUsesRawAgentLayout(t *testing.T) {
	assert.Equal(
		t,
		"laptop",
		s3MachineFromRoot("s3://bucket/archive/raw/laptop/raw/claude"),
	)
}

func TestProcessS3SessionNamespacesIDsBySourceMachine(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/shared-id.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, path, got)
		return io.NopCloser(strings.NewReader(content)), nil
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano(),
	})
	require.NoError(t, res.err)
	require.Len(t, res.results, 1)

	written, _, failed := e.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	sess, err := database.GetSessionFull(context.Background(), "laptop~shared-id")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "laptop", sess.Machine)
	assert.Equal(t, path, derefString(sess.FilePath))
	raw, err := database.GetSessionFull(context.Background(), "shared-id")
	require.NoError(t, err)
	assert.Nil(t, raw)
}

func TestProcessS3CodexNamespacesIDsBySourceMachine(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/codex/2026/06/24/rollout-2026-06-24T00-00-00-abc.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta("2024-01-01T00:00:00Z", "abc", "/repo", "codex").
		AddCodexMessage("2024-01-01T00:00:01Z", "user", "Hello").
		AddCodexMessage("2024-01-01T00:00:02Z", "assistant", "Hi.").
		String()

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, path, got)
		return io.NopCloser(strings.NewReader(content)), nil
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentCodex,
		Path:        path,
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano(),
	})
	require.NoError(t, res.err)
	require.Len(t, res.results, 1)

	written, _, failed := e.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	sess, err := database.GetSessionFull(context.Background(), "laptop~codex:abc")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "laptop", sess.Machine)
	assert.Equal(t, path, derefString(sess.FilePath))
	raw, err := database.GetSessionFull(context.Background(), "codex:abc")
	require.NoError(t, err)
	assert.Nil(t, raw)
}

func TestProcessS3ClaudeFetchesPersistedToolResultSidecar(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/parent-session.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/tool-results/b123.txt"
	localResultPath := "/Users/alice/.claude/projects/test-proj/" +
		"parent-session/tool-results/b123.txt"
	fullOutput := "full output line 1\nfull output line 2\n"
	persistedContentJSON := mustSyncJSONString(t,
		"<persisted-output>\n"+
			"Output too large. Full output saved to: "+localResultPath+
			"\n\nPreview (first 2KB):\npreview only\n</persisted-output>")
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":32}}`,
	}, "\n")

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetched := make(map[string]int)
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		fetched[got]++
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			return io.NopCloser(strings.NewReader(fullOutput)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 6, 0, 0, time.UTC).UnixNano(),
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	require.Len(t, res.results[0].Messages, 3)
	require.Len(t, res.results[0].Messages[2].ToolResults, 1)
	assert.Equal(
		t,
		fullOutput,
		parser.DecodeContent(res.results[0].Messages[2].ToolResults[0].ContentRaw),
	)
	assert.Equal(t, 1, fetched[path])
	assert.Equal(t, 1, fetched[sidecarPath])
}

func TestProcessS3ClaudeMissingSidecarKeepsPersistedPreview(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/parent-session.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/tool-results/missing.txt"
	localResultPath := "/Users/alice/.claude/projects/test-proj/" +
		"parent-session/tool-results/missing.txt"
	persistedContent := "<persisted-output>\n" +
		"Output too large. Full output saved to: " + localResultPath +
		"\n\nPreview (first 2KB):\npreview only\n</persisted-output>"
	persistedContentJSON := mustSyncJSONString(t, persistedContent)
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":32}}`,
	}, "\n")

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetched := make(map[string]int)
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		fetched[got]++
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			return nil, missingS3ObjectError()
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 12, 0, 0, time.UTC).UnixNano(),
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	require.Len(t, res.results[0].Messages, 3)
	require.Len(t, res.results[0].Messages[2].ToolResults, 1)
	assert.Equal(
		t,
		persistedContent,
		parser.DecodeContent(res.results[0].Messages[2].ToolResults[0].ContentRaw),
	)
	assert.Equal(t, 1, fetched[path])
	assert.Positive(t, fetched[sidecarPath])
}

func TestProcessS3ClaudeSidecarFetchErrorIsRetryable(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/parent-session.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/tool-results/out.txt"
	localResultPath := "/Users/alice/.claude/projects/test-proj/" +
		"parent-session/tool-results/out.txt"
	persistedContent := "<persisted-output>\n" +
		"Output too large. Full output saved to: " + localResultPath +
		"\n\nPreview (first 2KB):\npreview only\n</persisted-output>"
	persistedContentJSON := mustSyncJSONString(t, persistedContent)
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":32}}`,
	}, "\n")

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			return nil, errors.New("temporary sidecar failure")
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 12, 30, 0, time.UTC).UnixNano(),
	})

	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "temporary sidecar failure")
	assert.True(t, res.noCacheSkip)
}

func TestProcessS3ClaudeHydratedSidecarReplacesStoredPreview(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/parent-session.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/tool-results/out.txt"
	localResultPath := "/Users/alice/.claude/projects/test-proj/" +
		"parent-session/tool-results/out.txt"
	persistedContent := "<persisted-output>\n" +
		"Output too large. Full output saved to: " + localResultPath +
		"\n\nPreview (first 2KB):\npreview only\n</persisted-output>"
	persistedContentJSON := mustSyncJSONString(t, persistedContent)
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":32}}`,
	}, "\n")

	sidecarContent := "full output\n"
	sidecarAvailable := false
	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			if !sidecarAvailable {
				return nil, missingS3ObjectError()
			}
			return io.NopCloser(strings.NewReader(sidecarContent)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	first := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 14, 0, 0, time.UTC).UnixNano(),
	})
	require.NoError(t, first.err)
	written, _, failed := e.writeBatch([]pendingWrite{{
		sess: first.results[0].Session,
		msgs: first.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	sidecarAvailable = true
	second := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)) + int64(len(sidecarContent)),
		SourceMtime: time.Date(2026, 6, 24, 12, 15, 0, 0, time.UTC).UnixNano(),
	})
	require.NoError(t, second.err)
	require.True(t, second.forceReplace)
	written, _, failed = e.writeBatch([]pendingWrite{{
		sess:         second.results[0].Session,
		msgs:         second.results[0].Messages,
		forceReplace: second.forceReplace,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	msgs, err := database.GetAllMessages(
		context.Background(), "laptop~parent-session",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, sidecarContent, msgs[1].ToolCalls[0].ResultContent)
}

func TestSyncSingleSessionS3ClaudeSidecarOnlyChangeReplacesPreview(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/parent-session.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/tool-results/out.txt"
	localResultPath := "/Users/alice/.claude/projects/test-proj/" +
		"parent-session/tool-results/out.txt"
	persistedContent := "<persisted-output>\n" +
		"Output too large. Full output saved to: " + localResultPath +
		"\n\nPreview (first 2KB):\npreview only\n</persisted-output>"
	persistedContentJSON := mustSyncJSONString(t, persistedContent)
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":32}}`,
	}, "\n")
	transcriptMtime := time.Date(2026, 6, 24, 12, 16, 0, 0, time.UTC)
	sidecarMtime := transcriptMtime.Add(time.Minute)
	sidecarContent := "full output\n"
	sidecarAvailable := false

	oldFetch := fetchS3Object
	oldStat := statS3Object
	oldStatClaude := statClaudeS3Session
	t.Cleanup(func() {
		fetchS3Object = oldFetch
		statS3Object = oldStat
		statClaudeS3Session = oldStatClaude
	})
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			if !sidecarAvailable {
				return nil, missingS3ObjectError()
			}
			return io.NopCloser(strings.NewReader(sidecarContent)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, path, got)
		return parser.S3Object{
			URI:          path,
			Size:         int64(len(content)),
			LastModified: transcriptMtime,
		}, nil
	}
	statClaudeS3Session = func(got string) (parser.S3Object, error) {
		require.Equal(t, path, got)
		size := int64(len(content))
		mtime := transcriptMtime
		if sidecarAvailable {
			size += int64(len(sidecarContent))
			mtime = sidecarMtime
		}
		return parser.S3Object{
			URI:          path,
			Size:         size,
			LastModified: mtime,
		}, nil
	}

	e := &Engine{
		db:      database,
		machine: "central",
		agentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"s3://bucket/laptop/raw/claude"},
		},
	}
	first := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: transcriptMtime.UnixNano(),
	})
	require.NoError(t, first.err)
	written, _, failed := e.writeBatch([]pendingWrite{{
		sess: first.results[0].Session,
		msgs: first.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	sidecarAvailable = true
	require.NoError(t, e.SyncSingleSession("laptop~parent-session"))

	msgs, err := database.GetAllMessages(
		context.Background(), "laptop~parent-session",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, sidecarContent, msgs[1].ToolCalls[0].ResultContent)
}

func TestProcessS3ClaudeMissingSidecarReplacesStoredHydratedOutput(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/parent-session.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/tool-results/out.txt"
	localResultPath := "/Users/alice/.claude/projects/test-proj/" +
		"parent-session/tool-results/out.txt"
	persistedContent := "<persisted-output>\n" +
		"Output too large. Full output saved to: " + localResultPath +
		"\n\nPreview (first 2KB):\npreview only\n</persisted-output>"
	persistedContentJSON := mustSyncJSONString(t, persistedContent)
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":32}}`,
	}, "\n")
	sidecarContent := "full output\n"
	sidecarAvailable := true
	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			if !sidecarAvailable {
				return nil, missingS3ObjectError()
			}
			return io.NopCloser(strings.NewReader(sidecarContent)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	first := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)) + int64(len(sidecarContent)),
		SourceMtime: time.Date(2026, 6, 24, 12, 17, 0, 0, time.UTC).UnixNano(),
	})
	require.NoError(t, first.err)
	written, _, failed := e.writeBatch([]pendingWrite{{
		sess:         first.results[0].Session,
		msgs:         first.results[0].Messages,
		forceReplace: first.forceReplace,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	sidecarAvailable = false
	second := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 18, 0, 0, time.UTC).UnixNano(),
	})
	require.NoError(t, second.err)
	require.True(t, second.forceReplace)
	written, _, failed = e.writeBatch([]pendingWrite{{
		sess:         second.results[0].Session,
		msgs:         second.results[0].Messages,
		forceReplace: second.forceReplace,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	msgs, err := database.GetAllMessages(
		context.Background(), "laptop~parent-session",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, persistedContent, msgs[1].ToolCalls[0].ResultContent)
}

func TestProcessS3ClaudeFetchesSidecarFromCustomProjectsRoot(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/parent-session.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/tool-results/custom.txt"
	localResultPath := "/mnt/claude-projects/test-proj/" +
		"parent-session/tool-results/custom.txt"
	fullOutput := "custom root output\n"
	persistedContentJSON := mustSyncJSONString(t,
		"<persisted-output>\n"+
			"Output too large. Full output saved to: "+localResultPath+
			"\n\nPreview (first 2KB):\npreview only\n</persisted-output>")
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":19}}`,
	}, "\n")

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetched := make(map[string]int)
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		fetched[got]++
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			return io.NopCloser(strings.NewReader(fullOutput)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 11, 0, 0, time.UTC).UnixNano(),
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	require.Len(t, res.results[0].Messages, 3)
	require.Len(t, res.results[0].Messages[2].ToolResults, 1)
	assert.Equal(
		t,
		fullOutput,
		parser.DecodeContent(res.results[0].Messages[2].ToolResults[0].ContentRaw),
	)
	assert.Equal(t, 1, fetched[path])
	assert.Equal(t, 1, fetched[sidecarPath])
}

func TestProcessS3ClaudeFetchesCustomRootSidecarWithSubagentsInS3Prefix(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "s3://bucket/archive/subagents/laptop/raw/claude/" +
		"test-proj/parent-session.jsonl"
	sidecarPath := "s3://bucket/archive/subagents/laptop/raw/claude/" +
		"test-proj/parent-session/tool-results/custom.txt"
	localResultPath := "/mnt/claude-projects/test-proj/" +
		"parent-session/tool-results/custom.txt"
	fullOutput := "custom root output\n"
	persistedContentJSON := mustSyncJSONString(t,
		"<persisted-output>\n"+
			"Output too large. Full output saved to: "+localResultPath+
			"\n\nPreview (first 2KB):\npreview only\n</persisted-output>")
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":19}}`,
	}, "\n")

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetched := make(map[string]int)
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		fetched[got]++
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			return io.NopCloser(strings.NewReader(fullOutput)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 13, 0, 0, time.UTC).UnixNano(),
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	require.Len(t, res.results[0].Messages, 3)
	require.Len(t, res.results[0].Messages[2].ToolResults, 1)
	assert.Equal(
		t,
		fullOutput,
		parser.DecodeContent(res.results[0].Messages[2].ToolResults[0].ContentRaw),
	)
	assert.Equal(t, 1, fetched[path])
	assert.Equal(t, 1, fetched[sidecarPath])
}

func TestProcessS3ClaudeFetchesSubagentLocalPersistedToolResultSidecar(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/subagents/agent-sub1.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/subagents/agent-sub1/tool-results/sub.txt"
	localResultPath := "/Users/alice/.claude/projects/test-proj/" +
		"parent-session/subagents/agent-sub1/tool-results/sub.txt"
	fullOutput := "subagent output\n"
	persistedContentJSON := mustSyncJSONString(t,
		"<persisted-output>\n"+
			"Output too large. Full output saved to: "+localResultPath+
			"\n\nPreview (first 2KB):\npreview only\n</persisted-output>")
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":16}}`,
	}, "\n")

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetched := make(map[string]int)
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		fetched[got]++
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			return io.NopCloser(strings.NewReader(fullOutput)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 7, 0, 0, time.UTC).UnixNano(),
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	require.Len(t, res.results[0].Messages, 3)
	require.Len(t, res.results[0].Messages[2].ToolResults, 1)
	assert.Equal(
		t,
		fullOutput,
		parser.DecodeContent(res.results[0].Messages[2].ToolResults[0].ContentRaw),
	)
	assert.Equal(t, 1, fetched[path])
	assert.Equal(t, 1, fetched[sidecarPath])
}

func TestProcessS3ClaudeFetchesParentSidecarWithUnrelatedSubagentsAncestor(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/subagents/agent-sub1.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/tool-results/parent.txt"
	localResultPath := "/Users/subagents/.claude/projects/test-proj/" +
		"parent-session/tool-results/parent.txt"
	fullOutput := "parent output\n"
	persistedContentJSON := mustSyncJSONString(t,
		"<persisted-output>\n"+
			"Output too large. Full output saved to: "+localResultPath+
			"\n\nPreview (first 2KB):\npreview only\n</persisted-output>")
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":14}}`,
	}, "\n")

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetched := make(map[string]int)
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		fetched[got]++
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			return io.NopCloser(strings.NewReader(fullOutput)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 9, 0, 0, time.UTC).UnixNano(),
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	require.Len(t, res.results[0].Messages, 3)
	require.Len(t, res.results[0].Messages[2].ToolResults, 1)
	assert.Equal(
		t,
		fullOutput,
		parser.DecodeContent(res.results[0].Messages[2].ToolResults[0].ContentRaw),
	)
	assert.Equal(t, 1, fetched[path])
	assert.Equal(t, 1, fetched[sidecarPath])
}

func TestProcessS3ClaudeFetchesNestedSubagentLocalPersistedToolResultSidecar(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/subagents/workflows/wf-123/agent-deep.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/subagents/workflows/wf-123/agent-deep/tool-results/out.txt"
	localResultPath := "/Users/alice/.claude/projects/test-proj/" +
		"parent-session/subagents/workflows/wf-123/agent-deep/tool-results/out.txt"
	fullOutput := "nested subagent output\n"
	persistedContentJSON := mustSyncJSONString(t,
		"<persisted-output>\n"+
			"Output too large. Full output saved to: "+localResultPath+
			"\n\nPreview (first 2KB):\npreview only\n</persisted-output>")
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":24}}`,
	}, "\n")

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetched := make(map[string]int)
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		fetched[got]++
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			return io.NopCloser(strings.NewReader(fullOutput)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 8, 0, 0, time.UTC).UnixNano(),
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	require.Len(t, res.results[0].Messages, 3)
	require.Len(t, res.results[0].Messages[2].ToolResults, 1)
	assert.Equal(
		t,
		fullOutput,
		parser.DecodeContent(res.results[0].Messages[2].ToolResults[0].ContentRaw),
	)
	assert.Equal(t, 1, fetched[path])
	assert.Equal(t, 1, fetched[sidecarPath])
}

func TestProcessS3ClaudeFetchesSidecarWithUnrelatedToolResultsAncestor(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/subagents/agent-sub1.jsonl"
	sidecarPath := "s3://bucket/laptop/raw/claude/test-proj/" +
		"parent-session/subagents/agent-sub1/tool-results/out.txt"
	localResultPath := "/mnt/tool-results/.claude/projects/test-proj/" +
		"parent-session/subagents/agent-sub1/tool-results/out.txt"
	fullOutput := "subagent output\n"
	persistedContentJSON := mustSyncJSONString(t,
		"<persisted-output>\n"+
			"Output too large. Full output saved to: "+localResultPath+
			"\n\nPreview (first 2KB):\npreview only\n</persisted-output>")
	resultPathJSON := mustSyncJSONString(t, localResultPath)
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":16}}`,
	}, "\n")

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetched := make(map[string]int)
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		fetched[got]++
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case sidecarPath:
			return io.NopCloser(strings.NewReader(fullOutput)), nil
		default:
			return nil, errors.New("unexpected s3 fetch: " + got)
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 10, 0, 0, time.UTC).UnixNano(),
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	require.Len(t, res.results[0].Messages, 3)
	require.Len(t, res.results[0].Messages[2].ToolResults, 1)
	assert.Equal(
		t,
		fullOutput,
		parser.DecodeContent(res.results[0].Messages[2].ToolResults[0].ContentRaw),
	)
	assert.Equal(t, 1, fetched[path])
	assert.Equal(t, 1, fetched[sidecarPath])
}

func TestS3ClaudeToolResultRelParsesWindowsPaths(t *testing.T) {
	rel, ok := s3ClaudeToolResultRel(
		`C:\Users\alice\.claude\projects\proj\session\tool-results\win.txt`,
	)

	require.True(t, ok)
	assert.Equal(t, "win.txt", rel)
}

func TestProcessS3ClaudeSubagentPreservesParentLayout(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/parent-sess/subagents/agent-sub1.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Do subtask").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Done.").
		String()

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, path, got)
		return io.NopCloser(strings.NewReader(content)), nil
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 6, 24, 12, 5, 0, 0, time.UTC).UnixNano(),
	})
	require.NoError(t, res.err)
	require.Len(t, res.results, 1)

	written, _, failed := e.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	sess, err := database.GetSessionFull(context.Background(), "laptop~agent-sub1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.ParentSessionID)
	assert.Equal(t, "laptop~parent-sess", *sess.ParentSessionID)
	assert.Equal(t, "subagent", sess.RelationshipType)
	assert.Equal(t, path, derefString(sess.FilePath))
}

func TestSyncSingleSessionS3PreservesStoredMachine(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/manual-id.jsonl"
	first := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()
	second := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello again").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi again.").
		String()
	mtime := time.Date(2026, 6, 24, 13, 0, 0, 0, time.UTC).UnixNano()

	var currentContent atomic.Value
	currentContent.Store(first)
	oldFetch := fetchS3Object
	oldStat := statS3Object
	oldStatClaude := statClaudeS3Session
	t.Cleanup(func() {
		fetchS3Object = oldFetch
		statS3Object = oldStat
		statClaudeS3Session = oldStatClaude
	})
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, path, got)
		return io.NopCloser(strings.NewReader(currentContent.Load().(string))), nil
	}
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, path, got)
		return parser.S3Object{
			URI:          path,
			Size:         int64(len(currentContent.Load().(string))),
			LastModified: time.Unix(0, mtime),
		}, nil
	}
	statClaudeS3Session = statS3Object

	e := &Engine{
		db:      database,
		machine: "central",
		agentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"s3://bucket/laptop/raw/claude"},
		},
	}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  int64(len(first)),
		SourceMtime: mtime,
	})
	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	written, _, failed := e.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)
	initial, err := database.GetSessionFull(
		context.Background(), "laptop~manual-id",
	)
	require.NoError(t, err)
	require.NotNil(t, initial)

	currentContent.Store(second)
	require.NoError(t, e.SyncSingleSession("laptop~manual-id"))

	sess, err := database.GetSessionFull(context.Background(), "laptop~manual-id")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "laptop", sess.Machine)
	assert.Equal(t, initial.Project, sess.Project)
	assert.Equal(t, path, derefString(sess.FilePath))
	raw, err := database.GetSessionFull(context.Background(), "manual-id")
	require.NoError(t, err)
	assert.Nil(t, raw)
}

func TestSyncSingleSessionS3WithoutMachineNamespaceUpdatesRawID(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "s3://bucket/claude/test-proj/manual-id.jsonl"
	first := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()
	second := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello again").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi again.").
		String()
	mtime := time.Date(2026, 6, 24, 13, 1, 0, 0, time.UTC).UnixNano()

	var currentContent atomic.Value
	currentContent.Store(first)
	oldFetch := fetchS3Object
	oldStat := statS3Object
	oldStatClaude := statClaudeS3Session
	t.Cleanup(func() {
		fetchS3Object = oldFetch
		statS3Object = oldStat
		statClaudeS3Session = oldStatClaude
	})
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, path, got)
		return io.NopCloser(strings.NewReader(currentContent.Load().(string))), nil
	}
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, path, got)
		return parser.S3Object{
			URI:          path,
			Size:         int64(len(currentContent.Load().(string))),
			LastModified: time.Unix(0, mtime),
		}, nil
	}
	statClaudeS3Session = statS3Object

	e := &Engine{
		db:      database,
		machine: "central",
		agentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"s3://bucket/claude"},
		},
	}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		SourceSize:  int64(len(first)),
		SourceMtime: mtime,
	})
	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	written, _, failed := e.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)
	initial, err := database.GetSessionFull(context.Background(), "manual-id")
	require.NoError(t, err)
	require.NotNil(t, initial)
	assert.Equal(t, "central", initial.Machine)

	currentContent.Store(second)
	require.NoError(t, e.SyncSingleSession("manual-id"))

	sess, err := database.GetSessionFull(context.Background(), "manual-id")
	require.NoError(t, err)
	require.NotNil(t, sess)
	msgs, err := database.GetAllMessages(context.Background(), "manual-id")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Hello again", msgs[0].Content)
	assert.Equal(t, "central", sess.Machine)
	assert.Equal(t, path, derefString(sess.FilePath))
	namespaced, err := database.GetSessionFull(
		context.Background(), "central~manual-id",
	)
	require.NoError(t, err)
	assert.Nil(t, namespaced)
}

func TestSourceMtimeS3RawSessionUsesObjectMetadata(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/claude/test-proj/manual-id.jsonl"
	mtime := time.Date(2026, 6, 24, 13, 2, 0, 0, time.UTC)
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "manual-id",
		Project:   "test-proj",
		Machine:   "central",
		Agent:     "claude",
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(128),
		FileMtime: int64Ptr(mtime.Add(-time.Minute).UnixNano()),
	}))

	oldStat := statS3Object
	oldStatClaude := statClaudeS3Session
	t.Cleanup(func() {
		statS3Object = oldStat
		statClaudeS3Session = oldStatClaude
	})
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, path, got)
		return parser.S3Object{
			URI:          path,
			Size:         256,
			LastModified: mtime,
		}, nil
	}
	statClaudeS3Session = statS3Object

	e := &Engine{
		db:      database,
		machine: "central",
		agentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"s3://bucket/claude"},
		},
	}

	assert.Equal(t, mtime.UnixNano(), e.SourceMtime("manual-id"))
}

func TestSourceMtimeS3HostPrefixedClaudeUsesSidecarMetadata(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/manual-id.jsonl"
	transcriptMtime := time.Date(2026, 6, 24, 13, 5, 0, 0, time.UTC)
	sidecarMtime := transcriptMtime.Add(time.Minute)
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "laptop~manual-id",
		Project:   "test-proj",
		Machine:   "laptop",
		Agent:     "claude",
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(128),
		FileMtime: int64Ptr(transcriptMtime.UnixNano()),
	}))

	oldStat := statS3Object
	oldStatClaude := statClaudeS3Session
	t.Cleanup(func() {
		statS3Object = oldStat
		statClaudeS3Session = oldStatClaude
	})
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, path, got)
		return parser.S3Object{
			URI:          path,
			Size:         128,
			LastModified: transcriptMtime,
		}, nil
	}
	statClaudeS3Session = func(got string) (parser.S3Object, error) {
		require.Equal(t, path, got)
		return parser.S3Object{
			URI:          path,
			Size:         256,
			LastModified: sidecarMtime,
		}, nil
	}

	e := &Engine{
		db:      database,
		machine: "central",
		agentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"s3://bucket/laptop/raw/claude"},
		},
	}

	assert.Equal(
		t,
		sidecarMtime.UnixNano(),
		e.SourceMtime("laptop~manual-id"),
	)
}

func TestPickPreferredClaudeDiscoveredFileUsesS3Metadata(t *testing.T) {
	database := openTestDB(t)
	oldFile := parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        "s3://bucket/a/test-proj/session.jsonl",
		Project:     "test-proj",
		SourceSize:  100,
		SourceMtime: time.Date(2026, 6, 24, 13, 3, 0, 0, time.UTC).UnixNano(),
	}
	newFile := parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        "s3://bucket/z/test-proj/session.jsonl",
		Project:     "test-proj",
		SourceSize:  200,
		SourceMtime: time.Date(2026, 6, 24, 13, 4, 0, 0, time.UTC).UnixNano(),
	}
	e := &Engine{db: database, machine: "central"}

	got := e.pickPreferredClaudeDiscoveredFile(
		"session", []parser.DiscoveredFile{oldFile, newFile},
	)

	assert.Equal(t, newFile.Path, got.Path)
}

func TestClaudeSourceMatchesStoredUsesS3Metadata(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/claude/test-proj/session.jsonl"
	mtime := time.Date(2026, 6, 24, 13, 5, 0, 0, time.UTC).UnixNano()
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "session",
		Project:   "test-proj",
		Machine:   "central",
		Agent:     "claude",
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(512),
		FileMtime: int64Ptr(mtime),
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"session", db.CurrentDataVersion(),
	))

	e := &Engine{db: database, machine: "central"}

	assert.True(t, e.claudeSourceMatchesStored("session", parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		SourceSize:  512,
		SourceMtime: mtime,
	}))
}

func mustSyncJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}
