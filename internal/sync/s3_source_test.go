package sync

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

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

func TestProcessFileS3CodexReparsesStaleProjectBeforeSkip(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	path := "s3://bucket/laptop/raw/codex/2026/06/24/" +
		"rollout-2026-06-24T00-00-00-" + uuid + ".jsonl"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			"2024-01-01T00:00:00Z",
			uuid,
			"/home/roborev/.roborev/ci-worktrees/agentsview/roborev-ci-28293-3831737461",
			"user",
		).
		AddCodexMessage("2024-01-01T00:00:01Z", "user", "review this").
		String()
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	sess := db.Session{
		ID:        "laptop~codex:" + uuid,
		Project:   "roborev_ci_28293_3831737461",
		Machine:   "laptop",
		Agent:     "codex",
		FilePath:  strPtr(path),
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
		if got != path {
			return nil, missingS3ObjectError()
		}
		fetched = true
		return io.NopCloser(strings.NewReader(content)), nil
	}

	e := &Engine{db: database}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:       parser.AgentCodex,
		Path:        path,
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: mtime,
	})

	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.True(t, fetched)
	require.Len(t, res.results, 1)
	assert.False(t, parser.NeedsProjectReparse(res.results[0].Session.Project))
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
