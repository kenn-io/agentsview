package sync

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// countS3Fetches serves objects from a map and counts fetches per URI.
func countS3Fetches(t *testing.T, objects map[string]string) func(string) int {
	t.Helper()
	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	var mu gosync.Mutex
	counts := make(map[string]int)
	fetchS3Object = func(uri string) (io.ReadCloser, error) {
		mu.Lock()
		counts[uri]++
		mu.Unlock()
		content, ok := objects[uri]
		if !ok {
			return nil, missingS3ObjectError()
		}
		return io.NopCloser(strings.NewReader(content)), nil
	}
	return func(uri string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[uri]
	}
}

func TestStartWorkersDownloadsSharedS3CodexParentOnce(t *testing.T) {
	const root = "s3://bucket/laptop/raw/codex"
	const parentID = "11111111-1111-4111-8111-111111111111"
	const ts = "2024-01-01T10:00:00Z"
	parentPath := root + "/2026/08/12/rollout-2026-08-12T00-00-00-" +
		parentID + ".jsonl"
	childIDs := []string{
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	}
	objects := map[string]string{
		parentPath: testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(parentID, "/workspace/project", "codex_cli_rs", ts),
			testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "parent-turn", ts),
		),
	}
	var files []parser.DiscoveredFile
	for _, childID := range childIDs {
		childPath := root + "/2026/08/13/rollout-2026-08-13T00-00-00-" +
			childID + ".jsonl"
		objects[childPath] = testjsonl.JoinJSONL(
			testjsonl.CodexForkedSessionMetaJSON(childID, parentID, "/workspace/project", "codex_cli_rs", ts),
			testjsonl.CodexSessionMetaJSON(parentID, "/workspace/project", "codex_cli_rs", ts),
			testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "parent-turn", ts),
			testjsonl.CodexMsgJSON("user", "replayed parent task", ts),
			testjsonl.CodexMsgJSON("assistant", "replayed parent answer", ts),
			testjsonl.CodexTurnContextWithIDJSON("gpt-5.5", "child-turn-"+childID, "2024-01-01T10:00:01Z"),
			testjsonl.CodexMsgJSON("user", "child task "+childID, "2024-01-01T10:00:01Z"),
			testjsonl.CodexMsgJSON("assistant", "child answer", "2024-01-01T10:00:05Z"),
		)
		files = append(files, parser.DiscoveredFile{
			Agent: parser.AgentCodex, Path: childPath, Machine: "laptop",
			SourceSize: int64(len(objects[childPath])), SourceMtime: 1,
		})
	}
	oldFindParent := findCodexS3ParentSessionURIs
	t.Cleanup(func() { findCodexS3ParentSessionURIs = oldFindParent })
	findCodexS3ParentSessionURIs = func(_, _, gotParent string) []string {
		require.Equal(t, parentID, gotParent)
		return []string{parentPath}
	}
	fetches := countS3Fetches(t, objects)

	e := &Engine{db: openTestDB(t), machine: "central"}
	firstMessages := make(map[string]string)
	for job := range e.startWorkers(t.Context(), files) {
		require.NoError(t, job.err)
		require.Len(t, job.results, 1)
		assert.Zero(t, job.deferredCount, "the fork parent must resolve for %s", job.path)
		require.NotEmpty(t, job.results[0].Messages)
		firstMessages[job.results[0].Session.ID] = job.results[0].Messages[0].Content
		job.releaseRetention()
	}

	assert.Equal(t, 1, fetches(parentPath),
		"children of one parent must share a single parent download")
	for _, file := range files {
		assert.Equal(t, 1, fetches(file.Path))
	}
	assert.Equal(t, map[string]string{
		"laptop~codex:" + childIDs[0]: "child task " + childIDs[0],
		"laptop~codex:" + childIDs[1]: "child task " + childIDs[1],
	}, firstMessages, "replayed parent turns must stay filtered for every child")
}

func TestS3CodexParentCacheMaterialize(t *testing.T) {
	const a = "s3://bucket/raw/codex/a.jsonl"
	const b = "s3://bucket/raw/codex/b.jsonl"
	const missing = "s3://bucket/raw/codex/missing.jsonl"
	objects := map[string]string{a: "aaaa", b: "bbbb"}
	for _, tt := range []struct {
		name     string
		maxBytes int64
		steps    []string
		close    bool
		want     map[string]int
	}{
		{
			name: "reuses a cached object", maxBytes: 1 << 20,
			steps: []string{a, a, b, a}, want: map[string]int{a: 1, b: 1},
		},
		{
			name: "evicts the least recently used object", maxBytes: 4,
			steps: []string{a, b, a}, want: map[string]int{a: 2, b: 1},
		},
		{
			name: "retries a failed download", maxBytes: 1 << 20,
			steps: []string{missing, missing}, want: map[string]int{missing: 2},
		},
		{
			name: "fetches directly after close", maxBytes: 1 << 20,
			steps: []string{a, a}, close: true, want: map[string]int{a: 2},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fetches := countS3Fetches(t, objects)
			cache := newS3CodexParentCache(tt.maxBytes)
			cache.tempRoot = t.TempDir()
			if tt.close {
				cache.close()
			}
			for i, uri := range tt.steps {
				dst := filepath.Join(t.TempDir(), filepath.Base(uri))
				err := cache.materialize(t.Context(), uri, dst)
				if uri == missing {
					require.Error(t, err, "step %d", i)
					assert.NoFileExists(t, dst)
					continue
				}
				require.NoError(t, err, "step %d", i)
				got, err := os.ReadFile(dst)
				require.NoError(t, err)
				assert.Equal(t, objects[uri], string(got))
			}
			for uri, want := range tt.want {
				assert.Equal(t, want, fetches(uri), uri)
			}
			cache.close()
			leftovers, err := os.ReadDir(cache.tempRoot)
			require.NoError(t, err)
			assert.Empty(t, leftovers, "close must remove the cached files")
		})
	}
}

func TestS3CodexParentCacheSharesInFlightDownload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const uri = "s3://bucket/raw/codex/parent.jsonl"
		oldFetch := fetchS3Object
		t.Cleanup(func() { fetchS3Object = oldFetch })
		release := make(chan struct{})
		var mu gosync.Mutex
		fetches := 0
		fetchS3Object = func(string) (io.ReadCloser, error) {
			mu.Lock()
			fetches++
			mu.Unlock()
			<-release
			return io.NopCloser(strings.NewReader("parent")), nil
		}
		cache := newS3CodexParentCache(1 << 20)
		defer cache.close()
		dir := t.TempDir()
		const callers = 4
		errs := make(chan error, callers)
		for i := range callers {
			go func() {
				dst := filepath.Join(dir, "child"+string(rune('a'+i)))
				errs <- cache.materialize(t.Context(), uri, dst)
			}()
		}
		// Every caller is now parked on the one download or its result.
		synctest.Wait()
		close(release)
		for range callers {
			require.NoError(t, <-errs)
		}
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, 1, fetches)
	})
}

func TestS3CodexParentCacheWaitHonorsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const uri = "s3://bucket/raw/codex/parent.jsonl"
		oldFetch := fetchS3Object
		t.Cleanup(func() { fetchS3Object = oldFetch })
		release := make(chan struct{})
		fetchS3Object = func(string) (io.ReadCloser, error) {
			<-release
			return io.NopCloser(strings.NewReader("parent")), nil
		}
		cache := newS3CodexParentCache(1 << 20)
		defer cache.close()
		dir := t.TempDir()
		go func() {
			_ = cache.materialize(t.Context(), uri, filepath.Join(dir, "first"))
		}()
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := cache.materialize(ctx, uri, filepath.Join(dir, "second"))
		require.ErrorIs(t, err, context.Canceled)
		close(release)
		synctest.Wait()
	})
}
