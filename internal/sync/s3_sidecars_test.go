package sync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func missingS3ObjectError() error {
	return minio.ErrorResponse{
		Code:    minio.NoSuchKey,
		Message: "not found",
	}
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

func mustSyncJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}
