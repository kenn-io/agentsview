package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectRootMemoRepeatedCwdCost(t *testing.T) {
	_, cwd, _ := projectRootMemoFixture(t)
	origStat, origLstat := osStat, osLstat
	defer func() { osStat, osLstat = origStat, origLstat }()
	var calls atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		calls.Add(1)
		return origStat(path)
	}
	osLstat = func(path string) (os.FileInfo, error) {
		calls.Add(1)
		return origLstat(path)
	}

	ctx := WithProjectRootMemo(context.Background())
	first := ExtractProjectFromCwdWithBranchContext(ctx, cwd, "")
	firstCalls := calls.Load()
	for range 7 {
		assert.Equal(t, first,
			ExtractProjectFromCwdWithBranchContext(ctx, cwd, ""))
	}
	assert.Equal(t, "main_repo", first)
	assert.Equal(t, firstCalls, calls.Load(),
		"repeated resolutions must not repeat the root fill")
	t.Logf("root fills=1; underlying calls after first=%d final=%d; project=%s",
		firstCalls, calls.Load(), first)
}

func TestProjectRootMemoAncestorScanKeepsCwdAssociation(t *testing.T) {
	container, knownCwd, _ := projectRootMemoFixture(t)
	otherCwd := filepath.Join(container, "unrelated-child")
	ctx := WithProjectRootMemo(context.Background())

	assert.Equal(t, "main_repo",
		ExtractProjectFromCwdWithBranchContext(ctx, knownCwd, ""))
	require.NoError(t, os.MkdirAll(filepath.Join(
		container, "other_repo", ".git",
	), 0o755))
	assert.Equal(t, "unrelated_child",
		ExtractProjectFromCwdWithBranchContext(ctx, otherCwd, ""))
}

func TestProjectRootMemoConcurrentFill(t *testing.T) {
	_, cwd, _ := projectRootMemoFixture(t)
	origStat, origLstat := osStat, osLstat
	defer func() { osStat, osLstat = origStat, origLstat }()
	var calls atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		calls.Add(1)
		return origStat(path)
	}
	osLstat = func(path string) (os.FileInfo, error) {
		calls.Add(1)
		return origLstat(path)
	}

	baselineMemo := WithProjectRootMemo(context.Background())
	_ = ExtractProjectFromCwdWithBranchContext(baselineMemo, cwd, "")
	baselineCalls := calls.Load()

	calls.Store(0)
	started := make(chan struct{})
	release := make(chan struct{})
	var blocked atomic.Bool
	osStat = func(path string) (os.FileInfo, error) {
		calls.Add(1)
		if blocked.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		return origStat(path)
	}
	ctx := WithProjectRootMemo(context.Background())
	results := make([]string, 16)
	var wg sync.WaitGroup
	ready := make(chan struct{}, len(results))
	goStart := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready <- struct{}{}
			<-goStart
			results[i] = ExtractProjectFromCwdWithBranchContext(ctx, cwd, "")
		}(i)
	}
	for range results {
		<-ready
	}
	close(goStart)
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, baselineCalls, calls.Load())
	for _, result := range results[1:] {
		assert.Equal(t, results[0], result)
	}
	t.Logf("concurrent fills=1; underlying calls baseline=%d concurrent=%d; project=%s",
		baselineCalls, calls.Load(), results[0])
}

func TestProjectRootMemoAbsentContextValue(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "missing-project", "src")
	assert.Equal(t, "src", ExtractProjectFromCwd(cwd))
	assert.Equal(t, "src",
		ExtractProjectFromCwdWithBranchContext(context.Background(), cwd, ""))
}

func TestProjectRootMemoPolicyGates(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "protected-project", "src")
	origProbe, origStat, origLstat := probeGitRootForCwd, osStat, osLstat
	t.Cleanup(func() {
		probeGitRootForCwd, osStat, osLstat = origProbe, origStat, origLstat
	})
	osStat = func(path string) (os.FileInfo, error) {
		assert.Fail(t, "policy gates must skip filesystem discovery", path)
		return origStat(path)
	}
	osLstat = func(path string) (os.FileInfo, error) {
		assert.Fail(t, "policy gates must skip filesystem discovery", path)
		return origLstat(path)
	}

	disabled := WithProjectRootMemo(
		WithoutFilesystemProjectDiscovery(context.Background()),
	)
	assert.Equal(t, "src",
		ExtractProjectFromCwdWithBranchContext(disabled, cwd, ""))

	probeGitRootForCwd = func(string) bool { return false }
	refused := WithProjectRootMemo(context.Background())
	assert.Equal(t, "src",
		ExtractProjectFromCwdWithBranchContext(refused, cwd, ""))

	foreign := `C:\Users\agent\project\src`
	if runtime.GOOS == "windows" {
		foreign = "/Users/agent/project/src"
	}
	winPath := looksLikeWindowsPath(foreign)
	norm := foreign
	if winPath {
		norm = filepath.ToSlash(foreign)
	}
	cleaned := filepath.Clean(norm)
	if filepath.IsAbs(cleaned) && isForeignOSPath(foreign, cleaned, winPath) {
		foreignCtx := WithProjectRootMemo(context.Background())
		assert.Equal(t, "src",
			ExtractProjectFromCwdWithBranchContext(foreignCtx, foreign, ""))
	}
}

func TestProjectRootMemoStandaloneRefreshes(t *testing.T) {
	root := t.TempDir()
	container := filepath.Join(root, "container")
	cwd := filepath.Join(container, "deleted-child", "src")
	repoA := filepath.Join(container, "repo-a")
	makeProjectMemoRepo(t, repoA, "deleted-child")
	assert.Equal(t, "repo_a", ExtractProjectFromCwd(cwd))

	require.NoError(t, os.RemoveAll(repoA))
	repoB := filepath.Join(container, "repo-b")
	makeProjectMemoRepo(t, repoB, "deleted-child")
	assert.Equal(t, "repo_b", ExtractProjectFromCwd(cwd))
}

func TestProjectRootMemoProviderRoutes(t *testing.T) {
	t.Run("OpenCode storage", func(t *testing.T) {
		root, cwd := projectRootMemoProviderFixture(t)
		sessionPath := filepath.Join(
			root, "storage", "session", "global", "session.json",
		)
		writeOpenCodeStorageFile(t, sessionPath, map[string]any{
			"id": "session", "directory": cwd, "title": "Storage",
			"time": map[string]any{
				"created": int64(1700000000000),
				"updated": int64(1700000060000),
			},
		})
		writeOpenCodeStorageFile(t, filepath.Join(
			root, "storage", "message", "session", "msg.json",
		), map[string]any{
			"id": "msg", "sessionID": "session", "role": "user",
			"time": map[string]any{"created": int64(1700000000000)},
		})
		writeOpenCodeStorageFile(t, filepath.Join(
			root, "storage", "part", "msg", "part.json",
		), map[string]any{
			"id": "part", "sessionID": "session", "messageID": "msg",
			"type": "text", "text": "storage message",
			"time": map[string]any{"created": int64(1700000000000)},
		})

		provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
		require.True(t, ok)
		sources, err := provider.Discover(context.Background())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
		require.NoError(t, err)
		assertProjectRootMemoProviderRoute(t, provider, ParseRequest{
			Source: sources[0], Fingerprint: fingerprint,
		})
	})

	t.Run("Command Code JSONL", func(t *testing.T) {
		root, cwd := projectRootMemoProviderFixture(t)
		path := filepath.Join(root, "project", "command-session.jsonl")
		writeSourceFile(t, path, fmt.Sprintf(
			`{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"command-session","role":"user","content":[{"type":"text","text":"Question"}],"metadata":{"cwd":%q}}
{"id":"m2","timestamp":"2026-06-01T10:00:03Z","sessionId":"command-session","role":"assistant","content":[{"type":"text","text":"Answer"}]}`,
			cwd,
		))
		provider, ok := NewProvider(AgentCommandCode, ProviderConfig{Roots: []string{root}})
		require.True(t, ok)
		sources, err := provider.Discover(context.Background())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		assertProjectRootMemoProviderRoute(t, provider, ParseRequest{Source: sources[0]})
	})

	t.Run("Kiro legacy JSONL", func(t *testing.T) {
		root, cwd := projectRootMemoProviderFixture(t)
		path := filepath.Join(root, "kiro-session.jsonl")
		writeSourceFile(t, path, kiroProviderJSONLFixture("Question"))
		writeSourceFile(t, filepath.Join(root, "kiro-session.json"), fmt.Sprintf(
			`{"session_id":"kiro-session","cwd":%q,"title":"kiro-session","created_at":"2026-06-01T10:00:00Z","updated_at":"2026-06-01T10:01:00Z"}`+"\n",
			cwd,
		))
		provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
		require.True(t, ok)
		sources, err := provider.Discover(context.Background())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		assertProjectRootMemoProviderRoute(t, provider, ParseRequest{Source: sources[0]})
	})

	t.Run("Kiro current JSONL", func(t *testing.T) {
		root, cwd := projectRootMemoProviderFixture(t)
		rawID := "sess_0123456789abcdef"
		path := filepath.Join(root, "workspace", rawID, "messages.jsonl")
		writeSourceFile(t, path,
			`{"payload":{"type":"user","content":"Question"}}`+"\n"+
				`{"payload":{"type":"assistant","content":"Answer"}}`+"\n")
		writeSourceFile(t, filepath.Join(filepath.Dir(path), "session.json"),
			fmt.Sprintf(`{"workspacePaths":[%q],"createdAt":"2026-06-01T10:00:00Z","lastModifiedAt":"2026-06-01T10:01:00Z"}`+"\n", cwd))
		provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
		require.True(t, ok)
		sources, err := provider.Discover(context.Background())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		assertProjectRootMemoProviderRoute(t, provider, ParseRequest{Source: sources[0]})
	})
}

func assertProjectRootMemoProviderRoute(
	t *testing.T, provider Provider, req ParseRequest,
) {
	t.Helper()
	origStat, origLstat := osStat, osLstat
	defer func() { osStat, osLstat = origStat, origLstat }()
	var calls atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		calls.Add(1)
		return origStat(path)
	}
	osLstat = func(path string) (os.FileInfo, error) {
		calls.Add(1)
		return origLstat(path)
	}

	parse := func(ctx context.Context) string {
		outcome, err := provider.Parse(ctx, req)
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		return outcome.Results[0].Result.Session.Project
	}
	assert.Equal(t, "src", parse(WithoutFilesystemProjectDiscovery(t.Context())))
	assert.Zero(t, calls.Load(), "disabled discovery must not inspect local repositories")

	uncachedProject := parse(context.Background())
	uncachedProjectAgain := parse(context.Background())
	uncachedCalls := calls.Load()

	calls.Store(0)
	memoCtx := WithProjectRootMemo(context.Background())
	memoProject := parse(memoCtx)
	memoProjectAgain := parse(memoCtx)
	memoCalls := calls.Load()

	assert.Equal(t, uncachedProject, uncachedProjectAgain)
	assert.Equal(t, memoProject, memoProjectAgain)
	assert.Equal(t, uncachedProject, memoProject)
	assert.Greater(t, uncachedCalls, memoCalls)
	assert.Greater(t, memoCalls, int64(0))
	t.Logf("provider=%s uncached_calls=%d memo_calls=%d project=%s",
		provider.Definition().Type, uncachedCalls, memoCalls, memoProject)
}

func projectRootMemoProviderFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	container := filepath.Join(root, "worktrees")
	mainRepo := filepath.Join(container, "main_repo")
	mustMkdirAll(t, filepath.Join(mainRepo, ".git", "worktrees", "deleted-child"))
	return root, filepath.Join(container, "deleted-child", "src")
}

func projectRootMemoFixture(t *testing.T) (string, string, string) {
	t.Helper()
	container := filepath.Join(t.TempDir(), "worktrees")
	mainRepo := filepath.Join(container, "main_repo")
	makeProjectMemoRepo(t, mainRepo, "known-child")
	return container, filepath.Join(container, "known-child", "src"), mainRepo
}

func makeProjectMemoRepo(t *testing.T, repo, child string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(repo, ".git", "worktrees", child))
}
