package sync

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

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
		if got != path {
			return nil, missingS3ObjectError()
		}
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
		if got != path {
			return nil, missingS3ObjectError()
		}
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

func TestProcessS3CodexUsesSessionIndex(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	path := "s3://bucket/laptop/raw/codex/2026/06/24/" +
		"rollout-2026-06-24T00-00-00-" + uuid + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta("2024-01-01T00:00:00Z", uuid, "/repo", "codex").
		AddCodexMessage("2024-01-01T00:00:01Z", "user", "Hello").
		AddCodexMessage("2024-01-01T00:00:02Z", "assistant", "Hi.").
		String()
	index := `{"id":"` + uuid + `","thread_name":"S3 title","updated_at":"2026-06-24T00:00:00Z"}` + "\n"

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		switch got {
		case path:
			return io.NopCloser(strings.NewReader(content)), nil
		case indexPath:
			return io.NopCloser(strings.NewReader(index)), nil
		default:
			return nil, missingS3ObjectError()
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentCodex,
		Path:        path,
		Machine:     "laptop",
		SourceSize:  int64(len(content) + len(index)),
		SourceMtime: time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano(),
	})

	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	assert.Equal(t, "S3 title", res.results[0].Session.SessionName)
}

func TestProcessS3CodexChangedSessionIndexTitleBypassesStoredSkip(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	path := "s3://bucket/laptop/raw/codex/2026/06/24/" +
		"rollout-2026-06-24T00-00-00-" + uuid + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta("2024-01-01T00:00:00Z", uuid, "/repo", "codex").
		AddCodexMessage("2024-01-01T00:00:01Z", "user", "Hello").
		AddCodexMessage("2024-01-01T00:00:02Z", "assistant", "Hi.").
		String()
	index := `{"id":"` + uuid + `","thread_name":"New title","updated_at":"2026-06-24T00:00:00Z"}` + "\n"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	oldTitle := "Old title"
	require.NoError(t, database.UpsertSession(db.Session{
		ID:          "laptop~codex:" + uuid,
		Project:     "repo",
		Machine:     "laptop",
		Agent:       "codex",
		FilePath:    strPtr(path),
		FileSize:    int64Ptr(int64(len(content))),
		FileMtime:   int64Ptr(mtime),
		FileHash:    strPtr("s3:fingerprint:rollout"),
		SessionName: &oldTitle,
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"laptop~codex:"+uuid, db.CurrentDataVersion(),
	))

	oldFetch := fetchS3Object
	oldStat := statS3Object
	t.Cleanup(func() {
		fetchS3Object = oldFetch
		statS3Object = oldStat
	})
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, indexPath, got)
		return parser.S3Object{
			URI:          indexPath,
			Size:         int64(len(index)),
			LastModified: time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC),
			Fingerprint:  "s3:fingerprint:index",
		}, nil
	}
	var fetchedRollout bool
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		switch got {
		case path:
			fetchedRollout = true
			return io.NopCloser(strings.NewReader(content)), nil
		case indexPath:
			return io.NopCloser(strings.NewReader(index)), nil
		default:
			return nil, missingS3ObjectError()
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:             parser.AgentCodex,
		Path:              path,
		Machine:           "laptop",
		SourceSize:        int64(len(content)),
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:rollout",
	})

	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.True(t, fetchedRollout)
	require.Len(t, res.results, 1)
	assert.Equal(t, "New title", res.results[0].Session.SessionName)
}

func TestProcessS3CodexStoredSkipCachesSessionIndex(t *testing.T) {
	database := openTestDB(t)
	const firstUUID = "11111111-1111-4111-8111-111111111111"
	const secondUUID = "22222222-2222-4222-8222-222222222222"
	root := "s3://bucket/laptop/raw/codex"
	firstPath := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		firstUUID + ".jsonl"
	secondPath := root + "/2026/06/24/rollout-2026-06-24T00-01-00-" +
		secondUUID + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	index := `{"id":"` + firstUUID + `","thread_name":"First title","updated_at":"2026-06-24T00:00:00Z"}` + "\n" +
		`{"id":"` + secondUUID + `","thread_name":"Second title","updated_at":"2026-06-24T00:01:00Z"}` + "\n"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	for _, seed := range []struct {
		id, path, title, hash string
		size                  int64
	}{
		{
			id:    "laptop~codex:" + firstUUID,
			path:  firstPath,
			title: "First title",
			hash:  "s3:fingerprint:first",
			size:  101,
		},
		{
			id:    "laptop~codex:" + secondUUID,
			path:  secondPath,
			title: "Second title",
			hash:  "s3:fingerprint:second",
			size:  202,
		},
	} {
		require.NoError(t, database.UpsertSession(db.Session{
			ID:          seed.id,
			Project:     "repo",
			Machine:     "laptop",
			Agent:       "codex",
			FilePath:    strPtr(seed.path),
			FileSize:    int64Ptr(seed.size),
			FileMtime:   int64Ptr(mtime),
			FileHash:    strPtr(seed.hash),
			SessionName: strPtr(seed.title),
		}))
		require.NoError(t, database.SetSessionDataVersion(
			seed.id, db.CurrentDataVersion(),
		))
	}

	oldFetch := fetchS3Object
	oldStat := statS3Object
	t.Cleanup(func() {
		fetchS3Object = oldFetch
		statS3Object = oldStat
	})
	var statCalls, fetchCalls int
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, indexPath, got)
		statCalls++
		return parser.S3Object{
			URI:          indexPath,
			Size:         int64(len(index)),
			LastModified: time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC),
			Fingerprint:  "s3:fingerprint:index",
		}, nil
	}
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, indexPath, got)
		fetchCalls++
		return io.NopCloser(strings.NewReader(index)), nil
	}

	e := &Engine{db: database, machine: "central"}
	for _, file := range []parser.DiscoveredFile{
		{
			Agent:             parser.AgentCodex,
			Path:              firstPath,
			Machine:           "laptop",
			SourceSize:        101,
			SourceMtime:       mtime,
			SourceFingerprint: "s3:fingerprint:first",
		},
		{
			Agent:             parser.AgentCodex,
			Path:              secondPath,
			Machine:           "laptop",
			SourceSize:        202,
			SourceMtime:       mtime,
			SourceFingerprint: "s3:fingerprint:second",
		},
	} {
		res := e.processFile(context.Background(), file)
		require.NoError(t, res.err)
		require.True(t, res.skip)
	}

	assert.Equal(t, 1, statCalls)
	assert.Equal(t, 1, fetchCalls)
}

func TestProcessS3CodexClearedSessionIndexTitleBypassesStoredSkip(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	path := "s3://bucket/laptop/raw/codex/2026/06/24/" +
		"rollout-2026-06-24T00-00-00-" + uuid + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta("2024-01-01T00:00:00Z", uuid, "/repo", "codex").
		AddCodexMessage("2024-01-01T00:00:01Z", "user", "Hello").
		AddCodexMessage("2024-01-01T00:00:02Z", "assistant", "Hi.").
		String()
	index := `{"id":"` + uuid + `","thread_name":"","updated_at":"2026-06-24T00:00:00Z"}` + "\n"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	require.NoError(t, database.UpsertSession(db.Session{
		ID:          "laptop~codex:" + uuid,
		Project:     "repo",
		Machine:     "laptop",
		Agent:       "codex",
		FilePath:    strPtr(path),
		FileSize:    int64Ptr(int64(len(content))),
		FileMtime:   int64Ptr(mtime),
		FileHash:    strPtr("s3:fingerprint:rollout"),
		SessionName: strPtr("Old title"),
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"laptop~codex:"+uuid, db.CurrentDataVersion(),
	))

	oldFetch := fetchS3Object
	oldStat := statS3Object
	t.Cleanup(func() {
		fetchS3Object = oldFetch
		statS3Object = oldStat
	})
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, indexPath, got)
		return parser.S3Object{
			URI:          indexPath,
			Size:         int64(len(index)),
			LastModified: time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC),
			Fingerprint:  "s3:fingerprint:index",
		}, nil
	}
	var fetchedRollout bool
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		switch got {
		case path:
			fetchedRollout = true
			return io.NopCloser(strings.NewReader(content)), nil
		case indexPath:
			return io.NopCloser(strings.NewReader(index)), nil
		default:
			return nil, missingS3ObjectError()
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:             parser.AgentCodex,
		Path:              path,
		Machine:           "laptop",
		SourceSize:        int64(len(content)),
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:rollout",
	})

	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.True(t, fetchedRollout)
	require.Len(t, res.results, 1)
	assert.Empty(t, res.results[0].Session.SessionName)
}

func TestProcessS3CodexMissingSessionIndexBypassesStoredSkip(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	path := "s3://bucket/laptop/raw/codex/2026/06/24/" +
		"rollout-2026-06-24T00-00-00-" + uuid + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta("2024-01-01T00:00:00Z", uuid, "/repo", "codex").
		AddCodexMessage("2024-01-01T00:00:01Z", "user", "Hello").
		AddCodexMessage("2024-01-01T00:00:02Z", "assistant", "Hi.").
		String()
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	require.NoError(t, database.UpsertSession(db.Session{
		ID:          "laptop~codex:" + uuid,
		Project:     "repo",
		Machine:     "laptop",
		Agent:       "codex",
		FilePath:    strPtr(path),
		FileSize:    int64Ptr(int64(len(content))),
		FileMtime:   int64Ptr(mtime),
		FileHash:    strPtr("s3:fingerprint:rollout"),
		SessionName: strPtr("Old title"),
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"laptop~codex:"+uuid, db.CurrentDataVersion(),
	))

	oldFetch := fetchS3Object
	oldStat := statS3Object
	t.Cleanup(func() {
		fetchS3Object = oldFetch
		statS3Object = oldStat
	})
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, indexPath, got)
		return parser.S3Object{}, missingS3ObjectError()
	}
	var fetchedRollout bool
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		switch got {
		case path:
			fetchedRollout = true
			return io.NopCloser(strings.NewReader(content)), nil
		case indexPath:
			return nil, missingS3ObjectError()
		default:
			return nil, missingS3ObjectError()
		}
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:             parser.AgentCodex,
		Path:              path,
		Machine:           "laptop",
		SourceSize:        int64(len(content)),
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:rollout",
	})

	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.True(t, fetchedRollout)
	require.Len(t, res.results, 1)
	assert.Empty(t, res.results[0].Session.SessionName)
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
