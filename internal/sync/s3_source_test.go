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

// scopeRecordingS3Factory is a Claude provider factory whose discovery emits a
// fixed s3:// source and records the roots each provider was constructed with,
// so a scoped pass's root filtering is observable.
type scopeRecordingS3Factory struct {
	source parser.SourceRef
	roots  [][]string
}

func (f *scopeRecordingS3Factory) Definition() parser.AgentDef {
	return parser.AgentDef{
		Type:        parser.AgentClaude,
		DisplayName: "Claude Code",
		FileBased:   true,
	}
}

func (f *scopeRecordingS3Factory) Capabilities() parser.Capabilities {
	return parser.Capabilities{
		Source: parser.SourceCapabilities{
			DiscoverSources: parser.CapabilitySupported,
		},
	}
}

func (f *scopeRecordingS3Factory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	f.roots = append(f.roots, append([]string(nil), cfg.Roots...))
	return scopeRecordingS3Provider{
		ProviderBase: parser.ProviderBase{
			Def:  f.Definition(),
			Caps: f.Capabilities(),
		},
		source: f.source,
	}
}

type scopeRecordingS3Provider struct {
	parser.ProviderBase
	source parser.SourceRef
}

func (p scopeRecordingS3Provider) Discover(
	context.Context,
) ([]parser.SourceRef, error) {
	return []parser.SourceRef{p.source}, nil
}

func (p scopeRecordingS3Provider) FindSource(
	context.Context, parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	return parser.SourceRef{}, false, nil
}

func (p scopeRecordingS3Provider) Fingerprint(
	context.Context, parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{}, nil
}

func (p scopeRecordingS3Provider) Parse(
	context.Context, parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

// TestSyncRootsSinceScopedRemoteRootSyncsNewObject covers the seam the
// daemon's periodic remote-source pass relies on: SyncRootsSince scoped to a
// configured s3:// root discovers and syncs a new remote object, and the
// provider is constructed with only the remote root so the pass never walks
// the provider's local roots.
func TestSyncRootsSinceScopedRemoteRootSyncsNewObject(t *testing.T) {
	database := openTestDB(t)
	localRoot := t.TempDir()
	const remoteRoot = "s3://bucket/remote-box/raw/claude"
	const uri = remoteRoot + "/test-proj/new-session.jsonl"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "Hello from S3").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "Hi.").
		String()
	mtime := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC).UnixNano()

	oldFetch := fetchS3Object
	oldStat := statS3Object
	t.Cleanup(func() {
		fetchS3Object = oldFetch
		statS3Object = oldStat
	})
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		require.Equal(t, uri, got)
		return io.NopCloser(strings.NewReader(content)), nil
	}
	statS3Object = func(string) (parser.S3Object, error) {
		return parser.S3Object{}, missingS3ObjectError()
	}

	factory := &scopeRecordingS3Factory{source: parser.SourceRef{
		Provider:       parser.AgentClaude,
		Key:            uri,
		DisplayPath:    uri,
		FingerprintKey: uri,
		ProjectHint:    "test-proj",
		Opaque: parser.S3DiscoveredSource{
			URI:         uri,
			Project:     "test-proj",
			Machine:     "remote-box",
			Size:        int64(len(content)),
			MtimeNS:     mtime,
			Fingerprint: "s3:fingerprint:new-session",
		},
	}}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {localRoot, remoteRoot},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncRootsSince(
		context.Background(), []string{remoteRoot}, time.Time{}, nil,
	)

	assert.Equal(t, 1, stats.Synced, "the new S3 object must sync")
	assert.Zero(t, stats.Failed)
	require.Equal(t, [][]string{{remoteRoot}}, factory.roots,
		"the scoped pass must construct the provider with only the remote root")
	sess, err := database.GetSessionFull(
		context.Background(), "remote-box~new-session",
	)
	require.NoError(t, err)
	require.NotNil(t, sess, "the synced S3 session must be queryable")
	assert.Equal(t, "remote-box", sess.Machine)
	require.NotNil(t, sess.FilePath)
	assert.Equal(t, uri, *sess.FilePath,
		"the stored source must be the s3:// URI, not a temp path")
}
