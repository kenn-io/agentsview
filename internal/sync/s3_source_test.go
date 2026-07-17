package sync

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	oldStat := statS3Object
	t.Cleanup(func() {
		fetchS3Object = oldFetch
		statS3Object = oldStat
	})
	var fetched bool
	fetchS3Object = func(string) (io.ReadCloser, error) {
		fetched = true
		return io.NopCloser(strings.NewReader("")), nil
	}
	statS3Object = func(string) (parser.S3Object, error) {
		return parser.S3Object{}, missingS3ObjectError()
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

func TestProcessFileS3SameMetadataDifferentFingerprintFetches(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/fingerprint.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	sess := db.Session{
		ID:        "laptop~fingerprint",
		Project:   "test-proj",
		Machine:   "laptop",
		Agent:     "claude",
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(int64(len(content))),
		FileMtime: int64Ptr(mtime),
		FileHash:  strPtr("s3:fingerprint:old"),
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	var fetched bool
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, path, got)
		fetched = true
		return io.NopCloser(strings.NewReader(content)), nil
	}

	e := &Engine{db: database}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:             parser.AgentClaude,
		Path:              path,
		Project:           "test-proj",
		Machine:           "laptop",
		SourceSize:        int64(len(content)),
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:new",
	})

	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.True(t, fetched)
	require.Len(t, res.results, 1)
	assert.Equal(t, "s3:fingerprint:new", res.results[0].Session.File.Hash)
}

func TestProcessFileS3ChangedFingerprintReplacesStoredMessages(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/fingerprint-rewrite.jsonl"
	first := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Reply").
		String()
	second := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "HELLO").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "REPLY").
		String()
	require.Len(t, second, len(first), "test fixture must keep size stable")
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	var content atomic.Value
	content.Store(first)
	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, path, got)
		return io.NopCloser(strings.NewReader(content.Load().(string))), nil
	}

	e := &Engine{db: database}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:             parser.AgentClaude,
		Path:              path,
		Project:           "test-proj",
		Machine:           "laptop",
		SourceSize:        int64(len(first)),
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:first",
	})
	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	written, _, failed, _ := e.writeBatch([]pendingWrite{{
		sess:         res.results[0].Session,
		msgs:         res.results[0].Messages,
		forceReplace: res.forceReplace,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	content.Store(second)
	res = e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:             parser.AgentClaude,
		Path:              path,
		Project:           "test-proj",
		Machine:           "laptop",
		SourceSize:        int64(len(second)),
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:second",
	})
	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.Len(t, res.results, 1)
	written, _, failed, _ = e.writeBatch([]pendingWrite{{
		sess:         res.results[0].Session,
		msgs:         res.results[0].Messages,
		forceReplace: res.forceReplace,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	msgs, err := database.GetAllMessages(
		context.Background(), "laptop~fingerprint-rewrite",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "HELLO", msgs[0].Content)
	assert.Equal(t, "REPLY", msgs[1].Content)
}

func TestProcessFileS3ChangedFingerprintBypassesMtimeSkipCache(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/cached.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	var fetched bool
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, path, got)
		fetched = true
		return io.NopCloser(strings.NewReader(content)), nil
	}

	e := &Engine{
		db:        database,
		skipCache: map[string]int64{path: mtime},
	}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:             parser.AgentClaude,
		Path:              path,
		Project:           "test-proj",
		Machine:           "laptop",
		SourceSize:        int64(len(content)),
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:new",
	})

	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.True(t, fetched)
	require.Len(t, res.results, 1)
}

func TestFilterFilesByMtimeKeepsS3ChangedFingerprint(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/fingerprint.jsonl"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "laptop~fingerprint",
		Project:   "test-proj",
		Machine:   "laptop",
		Agent:     "claude",
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(512),
		FileMtime: int64Ptr(mtime),
		FileHash:  strPtr("s3:fingerprint:old"),
	}))

	e := &Engine{db: database}
	got := e.filterFilesByMtime(context.Background(), []parser.DiscoveredFile{{
		Agent:             parser.AgentClaude,
		Path:              path,
		Project:           "test-proj",
		Machine:           "laptop",
		SourceSize:        512,
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:new",
	}}, time.Unix(0, mtime).Add(time.Nanosecond))

	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].Path)
}

func TestFilterFilesByMtimeKeepsS3ChangedSize(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/size.jsonl"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "laptop~size",
		Project:   "test-proj",
		Machine:   "laptop",
		Agent:     "claude",
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(512),
		FileMtime: int64Ptr(mtime),
	}))

	e := &Engine{db: database}
	got := e.filterFilesByMtime(context.Background(), []parser.DiscoveredFile{{
		Agent:       parser.AgentClaude,
		Path:        path,
		Project:     "test-proj",
		Machine:     "laptop",
		SourceSize:  1024,
		SourceMtime: mtime,
	}}, time.Unix(0, mtime).Add(time.Nanosecond))

	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].Path)
}

func TestFilterFilesByMtimeKeepsOnlyS3CodexChangedIndexTitle(t *testing.T) {
	database := openTestDB(t)
	renamedUUID := "11111111-1111-4111-8111-111111111111"
	unchangedUUID := "22222222-2222-4222-8222-222222222222"
	root := "s3://bucket/laptop/raw/codex"
	renamedPath := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		renamedUUID + ".jsonl"
	unchangedPath := root + "/2026/06/24/rollout-2026-06-24T00-01-00-" +
		unchangedUUID + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()
	indexMtime := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	index := `{"id":"` + renamedUUID + `","thread_name":"Renamed title","updated_at":"2026-06-24T12:30:00Z"}` + "\n" +
		`{"id":"` + unchangedUUID + `","thread_name":"Unchanged title","updated_at":"2026-06-24T12:30:00Z"}` + "\n"

	for _, seed := range []struct {
		id, path, title, hash string
		size                  int64
	}{
		{
			id:    "laptop~codex:" + renamedUUID,
			path:  renamedPath,
			title: "Original title",
			hash:  "s3:fingerprint:renamed-rollout",
			size:  101,
		},
		{
			id:    "laptop~codex:" + unchangedUUID,
			path:  unchangedPath,
			title: "Unchanged title",
			hash:  "s3:fingerprint:unchanged-rollout",
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

	oldStat := statS3Object
	oldFetch := fetchS3Object
	t.Cleanup(func() {
		statS3Object = oldStat
		fetchS3Object = oldFetch
	})
	var statCalls, fetchCalls int
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, indexPath, got)
		statCalls++
		return parser.S3Object{
			URI:          indexPath,
			Size:         int64(len(index)),
			LastModified: indexMtime,
			Fingerprint:  "s3:fingerprint:index",
		}, nil
	}
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, indexPath, got)
		fetchCalls++
		return io.NopCloser(strings.NewReader(index)), nil
	}

	e := &Engine{db: database}
	got := e.filterFilesByMtime(context.Background(), []parser.DiscoveredFile{
		{
			Agent:             parser.AgentCodex,
			Path:              renamedPath,
			Machine:           "laptop",
			SourceSize:        101,
			SourceMtime:       mtime,
			SourceFingerprint: "s3:fingerprint:renamed-rollout",
		},
		{
			Agent:             parser.AgentCodex,
			Path:              unchangedPath,
			Machine:           "laptop",
			SourceSize:        202,
			SourceMtime:       mtime,
			SourceFingerprint: "s3:fingerprint:unchanged-rollout",
		},
	}, indexMtime.Add(-time.Minute))

	require.Len(t, got, 1)
	assert.Equal(t, renamedPath, got[0].Path)
	assert.Equal(t, 1, statCalls)
	assert.Equal(t, 1, fetchCalls)
}

func TestFilterFilesByMtimeDoesNotFetchOldS3CodexIndex(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	root := "s3://bucket/laptop/raw/codex"
	path := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		uuid + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()
	indexMtime := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	index := `{"id":"` + uuid + `","thread_name":"Renamed title","updated_at":"2026-06-24T12:30:00Z"}` + "\n"

	require.NoError(t, database.UpsertSession(db.Session{
		ID:          "laptop~codex:" + uuid,
		Project:     "repo",
		Machine:     "laptop",
		Agent:       "codex",
		FilePath:    strPtr(path),
		FileSize:    int64Ptr(101),
		FileMtime:   int64Ptr(mtime),
		FileHash:    strPtr("s3:fingerprint:rollout"),
		SessionName: strPtr("Old title"),
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"laptop~codex:"+uuid, db.CurrentDataVersion(),
	))

	oldStat := statS3Object
	oldFetch := fetchS3Object
	t.Cleanup(func() {
		statS3Object = oldStat
		fetchS3Object = oldFetch
	})
	var statCalls, fetchCalls int
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, indexPath, got)
		statCalls++
		return parser.S3Object{
			URI:          indexPath,
			Size:         int64(len(index)),
			LastModified: indexMtime,
			Fingerprint:  "s3:fingerprint:index",
		}, nil
	}
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, indexPath, got)
		fetchCalls++
		return io.NopCloser(strings.NewReader(index)), nil
	}

	e := &Engine{db: database}
	got := e.filterFilesByMtime(context.Background(), []parser.DiscoveredFile{{
		Agent:             parser.AgentCodex,
		Path:              path,
		Machine:           "laptop",
		SourceSize:        101,
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:rollout",
	}}, indexMtime.Add(time.Minute))

	assert.Empty(t, got)
	assert.Equal(t, 1, statCalls)
	assert.Equal(t, 0, fetchCalls)
}

func TestFilterFilesByMtimeKeepsS3CodexIndexFetchError(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	root := "s3://bucket/laptop/raw/codex"
	path := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		uuid + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()
	indexMtime := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)

	require.NoError(t, database.UpsertSession(db.Session{
		ID:          "laptop~codex:" + uuid,
		Project:     "repo",
		Machine:     "laptop",
		Agent:       "codex",
		FilePath:    strPtr(path),
		FileSize:    int64Ptr(101),
		FileMtime:   int64Ptr(mtime),
		FileHash:    strPtr("s3:fingerprint:rollout"),
		SessionName: strPtr("Old title"),
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"laptop~codex:"+uuid, db.CurrentDataVersion(),
	))

	oldStat := statS3Object
	oldFetch := fetchS3Object
	t.Cleanup(func() {
		statS3Object = oldStat
		fetchS3Object = oldFetch
	})
	var fetchCalls int
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, indexPath, got)
		return parser.S3Object{
			URI:          indexPath,
			Size:         123,
			LastModified: indexMtime,
			Fingerprint:  "s3:fingerprint:index",
		}, nil
	}
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, indexPath, got)
		fetchCalls++
		return nil, errors.New("temporary index read failure")
	}

	e := &Engine{db: database}
	got := e.filterFilesByMtime(context.Background(), []parser.DiscoveredFile{{
		Agent:             parser.AgentCodex,
		Path:              path,
		Machine:           "laptop",
		SourceSize:        101,
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:rollout",
	}}, indexMtime.Add(-time.Minute))

	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].Path)
	assert.Equal(t, 1, fetchCalls)
}

func TestFilterFilesByMtimeKeepsS3CodexClearedIndexTitle(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	root := "s3://bucket/laptop/raw/codex"
	path := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		uuid + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()
	indexMtime := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)

	require.NoError(t, database.UpsertSession(db.Session{
		ID:          "laptop~codex:" + uuid,
		Project:     "repo",
		Machine:     "laptop",
		Agent:       "codex",
		FilePath:    strPtr(path),
		FileSize:    int64Ptr(101),
		FileMtime:   int64Ptr(mtime),
		FileHash:    strPtr("s3:fingerprint:rollout"),
		SessionName: strPtr("Old title"),
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"laptop~codex:"+uuid, db.CurrentDataVersion(),
	))

	for _, tc := range []struct {
		name, index string
	}{
		{
			name:  "blank title",
			index: `{"id":"` + uuid + `","thread_name":"","updated_at":"2026-06-24T12:30:00Z"}` + "\n",
		},
		{
			name:  "missing title row",
			index: `{"id":"22222222-2222-4222-8222-222222222222","thread_name":"Other","updated_at":"2026-06-24T12:30:00Z"}` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldStat := statS3Object
			oldFetch := fetchS3Object
			t.Cleanup(func() {
				statS3Object = oldStat
				fetchS3Object = oldFetch
			})
			statS3Object = func(got string) (parser.S3Object, error) {
				require.Equal(t, indexPath, got)
				return parser.S3Object{
					URI:          indexPath,
					Size:         int64(len(tc.index)),
					LastModified: indexMtime,
					Fingerprint:  "s3:fingerprint:index",
				}, nil
			}
			fetchS3Object = func(got string) (io.ReadCloser, error) {
				require.Equal(t, indexPath, got)
				return io.NopCloser(strings.NewReader(tc.index)), nil
			}

			e := &Engine{db: database}
			got := e.filterFilesByMtime(context.Background(), []parser.DiscoveredFile{{
				Agent:             parser.AgentCodex,
				Path:              path,
				Machine:           "laptop",
				SourceSize:        101,
				SourceMtime:       mtime,
				SourceFingerprint: "s3:fingerprint:rollout",
			}}, indexMtime.Add(-time.Minute))

			require.Len(t, got, 1)
			assert.Equal(t, path, got[0].Path)
		})
	}
}

func TestFilterFilesByMtimeKeepsS3CodexMissingIndexWhenTitleStored(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	root := "s3://bucket/laptop/raw/codex"
	path := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		uuid + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()

	require.NoError(t, database.UpsertSession(db.Session{
		ID:          "laptop~codex:" + uuid,
		Project:     "repo",
		Machine:     "laptop",
		Agent:       "codex",
		FilePath:    strPtr(path),
		FileSize:    int64Ptr(101),
		FileMtime:   int64Ptr(mtime),
		FileHash:    strPtr("s3:fingerprint:rollout"),
		SessionName: strPtr("Old title"),
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"laptop~codex:"+uuid, db.CurrentDataVersion(),
	))

	oldStat := statS3Object
	oldFetch := fetchS3Object
	t.Cleanup(func() {
		statS3Object = oldStat
		fetchS3Object = oldFetch
	})
	var fetchCalls int
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, indexPath, got)
		return parser.S3Object{}, missingS3ObjectError()
	}
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		fetchCalls++
		return nil, missingS3ObjectError()
	}

	e := &Engine{db: database}
	got := e.filterFilesByMtime(context.Background(), []parser.DiscoveredFile{{
		Agent:             parser.AgentCodex,
		Path:              path,
		Machine:           "laptop",
		SourceSize:        101,
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:rollout",
	}}, time.Unix(0, mtime).Add(time.Nanosecond))

	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].Path)
	assert.Equal(t, 0, fetchCalls)
}

func TestPickPreferredCodexDiscoveredFileUsesS3MachinePrefix(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	root := "s3://bucket/laptop/raw/codex"
	datedPath := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		uuid + ".jsonl"
	archivedPath := root + "/archived_sessions/rollout-2026-06-24T00-00-00-" +
		uuid + ".jsonl"

	require.NoError(t, database.UpsertSession(db.Session{
		ID:       "laptop~codex:" + uuid,
		Project:  "repo",
		Machine:  "laptop",
		Agent:    "codex",
		FilePath: strPtr(archivedPath),
	}))

	chosen := pickPreferredCodexDiscoveredFile(database, []parser.DiscoveredFile{
		{
			Agent:   parser.AgentCodex,
			Path:    datedPath,
			Machine: "laptop",
		},
		{
			Agent:   parser.AgentCodex,
			Path:    archivedPath,
			Machine: "laptop",
		},
	})

	assert.Equal(t, archivedPath, chosen.Path)
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

func TestProcessFileS3CodexIndexFetchErrorDoesNotSkip(t *testing.T) {
	database := openTestDB(t)
	const uuid = "11111111-1111-4111-8111-111111111111"
	path := "s3://bucket/laptop/raw/codex/2026/06/24/" +
		"rollout-2026-06-24T00-00-00-" + uuid + ".jsonl"
	indexPath := "s3://bucket/laptop/raw/session_index.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta("2024-01-01T00:00:00Z", uuid, "/repo", "codex").
		AddCodexMessage("2024-01-01T00:00:01Z", "user", "Hello").
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

	oldStat := statS3Object
	oldFetch := fetchS3Object
	t.Cleanup(func() {
		statS3Object = oldStat
		fetchS3Object = oldFetch
	})
	statS3Object = func(got string) (parser.S3Object, error) {
		require.Equal(t, indexPath, got)
		return parser.S3Object{
			URI:          indexPath,
			Size:         123,
			LastModified: time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC),
			Fingerprint:  "s3:fingerprint:index",
		}, nil
	}
	var fetchedRollout bool
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		if got == path {
			fetchedRollout = true
			return io.NopCloser(strings.NewReader(content)), nil
		}
		require.Equal(t, indexPath, got)
		return nil, errors.New("temporary index read failure")
	}

	e := &Engine{db: database}
	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent:             parser.AgentCodex,
		Path:              path,
		Machine:           "laptop",
		SourceSize:        int64(len(content)),
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:rollout",
	})

	require.Error(t, res.err)
	assert.False(t, res.skip)
	assert.True(t, res.noCacheSkip)
	assert.False(t, fetchedRollout)
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

	written, _, failed, _ := e.writeBatch([]pendingWrite{{
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

func TestStructuredS3MachineOverrideChangesStoredMachineAndIDPrefix(t *testing.T) {
	database := openTestDB(t)
	root := "s3://bucket/pathbox/raw/claude"
	sessionPath := root + "/test-proj/override-id.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()
	mtime := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC).UnixNano()

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, sessionPath, got)
		return io.NopCloser(strings.NewReader(content)), nil
	}

	engine := NewEngine(database, EngineConfig{
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentClaude: {root: "explicitbox"},
		},
		Machine: "viewer",
	})
	file := parser.DiscoveredFile{
		Agent:       parser.AgentClaude,
		Path:        sessionPath,
		Project:     "test-proj",
		Machine:     engine.s3MachineForSource(parser.AgentClaude, root, "pathbox"),
		SourceSize:  int64(len(content)),
		SourceMtime: mtime,
	}

	res := engine.processFile(context.Background(), file)

	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.Len(t, res.results, 1)
	written, _, failed, _ := engine.writeBatch([]pendingWrite{{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Zero(t, failed)
	sess, err := database.GetSessionFull(
		context.Background(), "explicitbox~override-id",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "explicitbox", sess.Machine)
	assert.Equal(t, sessionPath, derefString(sess.FilePath))
	pathDerived, err := database.GetSessionFull(
		context.Background(), "pathbox~override-id",
	)
	require.NoError(t, err)
	assert.Nil(t, pathDerived)
}

func TestS3MachineLabelTransitionsReattributeStoredSession(t *testing.T) {
	database := openTestDB(t)
	root := "s3://bucket/pathbox/raw/claude"
	sessionPath := root + "/test-proj/transition-id.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()
	mtime := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC).UnixNano()

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, sessionPath, got)
		return io.NopCloser(strings.NewReader(content)), nil
	}

	engine := NewEngine(database, EngineConfig{Machine: "viewer"})
	writeAs := func(machine string) string {
		t.Helper()
		res := engine.processFile(context.Background(), parser.DiscoveredFile{
			Agent:       parser.AgentClaude,
			Path:        sessionPath,
			Project:     "test-proj",
			Machine:     machine,
			SourceSize:  int64(len(content)),
			SourceMtime: mtime,
		})
		require.NoError(t, res.err)
		require.False(t, res.skip)
		require.Len(t, res.results, 1)
		sessionID := machine + "~transition-id"
		written, _, failed, _ := engine.writeBatch([]pendingWrite{{
			sess: res.results[0].Session,
			msgs: res.results[0].Messages,
			usageEvents: []parser.ParsedUsageEvent{{
				SessionID:    sessionID,
				Source:       "session",
				Model:        "fixture-model",
				InputTokens:  10,
				OutputTokens: 2,
				OccurredAt:   "2024-01-01T00:00:05Z",
				DedupKey:     "transition-usage",
			}},
		}}, syncWriteDefault, false)
		require.Equal(t, 1, written)
		require.Zero(t, failed)
		return sessionID
	}

	pathDerivedID := writeAs("pathbox")
	pathDerivedMessages, err := database.GetAllMessages(
		context.Background(), pathDerivedID,
	)
	require.NoError(t, err)
	require.Len(t, pathDerivedMessages, 2)
	pinNote := "keep this message"
	_, err = database.PinMessage(
		pathDerivedID, pathDerivedMessages[0].ID, &pinNote,
	)
	require.NoError(t, err)
	_, err = database.InsertRecallEntry(db.RecallEntry{
		ID: "transition-recall", Type: "fact", Scope: "project",
		Title: "Transition recall", Body: "Preserve this entry",
		SourceSessionID: pathDerivedID,
		Evidence: []db.RecallEvidence{{
			SessionID:           pathDerivedID,
			MessageStartOrdinal: 0,
			MessageEndOrdinal:   1,
		}},
	})
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(db.Session{
		ID:              "referencing-child",
		Project:         "test-proj",
		Machine:         "viewer",
		Agent:           "claude",
		ParentSessionID: strPtr(pathDerivedID),
	}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID:      "referencing-parent",
		Project: "test-proj",
		Machine: "viewer",
		Agent:   "claude",
	}))
	require.NoError(t, database.InsertMessages([]db.Message{{
		SessionID: "referencing-parent",
		Ordinal:   0,
		Role:      "assistant",
		Content:   "spawn",
		ToolCalls: []db.ToolCall{{
			ToolName:          "Agent",
			Category:          "agent",
			ToolUseID:         "transition-tool",
			SubagentSessionID: pathDerivedID,
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID:         "transition-tool",
				SubagentSessionID: pathDerivedID,
				Source:            "progress",
				Status:            "completed",
				EventIndex:        0,
			}},
		}},
	}}))

	for _, transition := range []struct {
		name  string
		from  string
		label string
	}{
		{name: "path-derived to explicit", from: pathDerivedID, label: "explicitbox"},
		{name: "explicit to changed label", from: "explicitbox~transition-id", label: "renamedbox"},
	} {
		t.Run(transition.name, func(t *testing.T) {
			currentID := writeAs(transition.label)

			old, err := database.GetSessionFull(context.Background(), transition.from)
			require.NoError(t, err)
			assert.Nil(t, old)
			ids, err := database.ListSessionIDsByFilePath(sessionPath, "claude")
			require.NoError(t, err)
			assert.Equal(t, []string{currentID}, ids)

			msgs, err := database.GetAllMessages(context.Background(), currentID)
			require.NoError(t, err)
			assert.Len(t, msgs, 2)
			events, err := database.GetUsageEvents(context.Background(), currentID)
			require.NoError(t, err)
			assert.Len(t, events, 1)
			pins, err := database.ListPinnedMessages(
				context.Background(), currentID, "",
			)
			require.NoError(t, err)
			require.Len(t, pins, 1)
			require.NotNil(t, pins[0].Note)
			assert.Equal(t, pinNote, *pins[0].Note)
			recall, err := database.GetRecallEntry(
				context.Background(), "transition-recall",
			)
			require.NoError(t, err)
			require.NotNil(t, recall)
			assert.Equal(t, currentID, recall.SourceSessionID)
			require.Len(t, recall.Evidence, 1)
			assert.Equal(t, currentID, recall.Evidence[0].SessionID)

			child, err := database.GetSessionFull(
				context.Background(), "referencing-child",
			)
			require.NoError(t, err)
			require.NotNil(t, child)
			require.NotNil(t, child.ParentSessionID)
			assert.Equal(t, currentID, *child.ParentSessionID)

			parentMessages, err := database.GetAllMessages(
				context.Background(), "referencing-parent",
			)
			require.NoError(t, err)
			require.Len(t, parentMessages, 1)
			require.Len(t, parentMessages[0].ToolCalls, 1)
			call := parentMessages[0].ToolCalls[0]
			assert.Equal(t, currentID, call.SubagentSessionID)
			require.Len(t, call.ResultEvents, 1)
			assert.Equal(t, currentID, call.ResultEvents[0].SubagentSessionID)
		})
	}
}

func TestS3MachineLabelTransitionKeepsOldIdentityWhenReplacementIsRejected(
	t *testing.T,
) {
	database := openTestDB(t)
	path := "s3://bucket/pathbox/raw/claude/test-proj/rejected-id.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()
	mtime := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC).UnixNano()

	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	}
	engine := NewEngine(database, EngineConfig{Machine: "viewer"})
	parse := func(machine string) parser.ParseResult {
		t.Helper()
		res := engine.processFile(context.Background(), parser.DiscoveredFile{
			Agent:       parser.AgentClaude,
			Path:        path,
			Project:     "test-proj",
			Machine:     machine,
			SourceSize:  int64(len(content)),
			SourceMtime: mtime,
		})
		require.NoError(t, res.err)
		require.Len(t, res.results, 1)
		return res.results[0]
	}

	initial := parse("pathbox")
	written, _, failed, _ := engine.writeBatch([]pendingWrite{{
		sess: initial.Session,
		msgs: initial.Messages,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Zero(t, failed)

	const replacementID = "explicitbox~rejected-id"
	require.NoError(t, database.UpsertSession(db.Session{
		ID:      replacementID,
		Project: "placeholder",
		Machine: "explicitbox",
		Agent:   "claude",
	}))
	require.NoError(t, database.DeleteSession(replacementID))

	replacement := parse("explicitbox")
	written, _, failed, _ = engine.writeBatch([]pendingWrite{{
		sess: replacement.Session,
		msgs: replacement.Messages,
	}}, syncWriteDefault, false)
	assert.Zero(t, written)
	assert.Zero(t, failed)

	old, err := database.GetSessionFull(
		context.Background(), "pathbox~rejected-id",
	)
	require.NoError(t, err)
	assert.NotNil(t, old)
	assert.True(t, database.IsSessionExcluded(replacementID))
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
	written, _, failed, _ := e.writeBatch([]pendingWrite{{
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
	written, _, failed, _ := e.writeBatch([]pendingWrite{{
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

// TestParseMaterializedS3SourcePreservesExclusionsWithoutResults pins that the
// S3 provider parse path keeps ExcludedSessionIDs even when no live session is
// produced. A Claude /usage probe parses to zero kept sessions but one excluded
// ID; the caller needs that ID to drop a previously-archived row on resync, so
// an empty Results slice must not short-circuit the exclusion through.
func TestParseMaterializedS3SourcePreservesExclusionsWithoutResults(t *testing.T) {
	database := openTestDB(t)
	e := &Engine{db: database, machine: "laptop"}

	dir := t.TempDir()
	usageCmd := "<command-name>/usage</command-name>\n" +
		"            <command-message>usage</command-message>\n" +
		"            <command-args></command-args>"
	content := testjsonl.ClaudeUserJSON(usageCmd, "2026-06-24T00:00:00Z")
	sessionID := "11111111-2222-3333-4444-555555555555"
	tempPath := filepath.Join(dir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(tempPath, []byte(content), 0o600))

	file := parser.DiscoveredFile{
		Agent:   parser.AgentClaude,
		Path:    "s3://bucket/laptop/raw/claude/proj/" + sessionID + ".jsonl",
		Project: "proj",
		Machine: "laptop",
	}
	res, err := e.parseMaterializedS3Source(
		context.Background(), file, dir, tempPath,
	)
	require.NoError(t, err)
	assert.Empty(t, res.results, "a /usage probe yields no live sessions")
	require.Len(t, res.excludedSessionIDs, 1,
		"the excluded probe ID must survive an empty Results slice")
}

// TestFilterFilesByMtimeAppliesCutoffToProviderDiscoveredClaudeS3 pins that a
// provider-discovered Claude s3:// object (one carrying a ProviderSource, as
// emitted by discoverProviderSources) is still subject to the incremental-sync
// mtime cutoff. The Claude provider cannot Fingerprint an s3:// URI; without the
// s3:// short-circuit in discoveredFileEffectiveMtime that error makes
// filterFilesByMtime keep every old object, defeating the cutoff.
func TestFilterFilesByMtimeAppliesCutoffToProviderDiscoveredClaudeS3(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/claude/test-proj/old.jsonl"
	mtime := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).UnixNano()
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "laptop~old",
		Project:   "test-proj",
		Machine:   "laptop",
		Agent:     "claude",
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(512),
		FileMtime: int64Ptr(mtime),
		FileHash:  strPtr("s3:fingerprint:stable"),
	}))

	claudeFactory, ok := parser.ProviderFactoryByType(parser.AgentClaude)
	require.True(t, ok, "claude provider factory must be registered")
	e := &Engine{
		db: database,
		providerFactories: map[parser.AgentType]parser.ProviderFactory{
			parser.AgentClaude: claudeFactory,
		},
	}

	source := parser.SourceRef{
		Provider:       parser.AgentClaude,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
	}
	// Cutoff is after the object's mtime and the object is unchanged, so it must
	// be filtered out. A registered factory means discoveredFileEffectiveMtime
	// would (pre-fix) route the s3:// source into provider Fingerprint and error.
	got := e.filterFilesByMtime(context.Background(), []parser.DiscoveredFile{{
		Agent:             parser.AgentClaude,
		Path:              path,
		Project:           "test-proj",
		Machine:           "laptop",
		SourceSize:        512,
		SourceMtime:       mtime,
		SourceFingerprint: "s3:fingerprint:stable",
		ProviderSource:    &source,
		ProviderProcess:   true,
	}}, time.Unix(0, mtime).Add(time.Hour))

	assert.Empty(t, got,
		"an unchanged old provider-discovered Claude S3 object must be cut off")
}
