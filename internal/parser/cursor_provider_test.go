package parser

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setCursorTestResolver(t *testing.T, provider Provider, root string) {
	t.Helper()
	p, ok := provider.(*cursorProvider)
	require.True(t, ok)
	p.sources.resolver = func(projectDir string) string {
		if filepath.Separator == '\\' && strings.HasPrefix(projectDir, "C-") {
			projectDir = strings.TrimPrefix(projectDir, "C-")
		}
		resolved, ambiguous := ResolveCursorWorkspaceDirIn(root, projectDir)
		if ambiguous {
			return ""
		}
		return resolved
	}
}

func TestCursorProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	resolverRoot := filepath.Join(root, "resolver")
	resolvedWorkspace := filepath.Join(resolverRoot, "Users", "fiona", "Documents", "demo")
	require.NoError(t, os.MkdirAll(resolvedWorkspace, 0o755))
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	flatTxt := cursorProviderWriteTranscript(t, transcriptsDir, "flat.txt", "old")
	flatJSONL := cursorProviderWriteJSONLTranscript(t, transcriptsDir, "flat.jsonl", "new")
	nestedTxt := cursorProviderWriteTranscript(t, transcriptsDir, filepath.Join("nested", "nested.txt"), "old")
	nestedJSONL := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("nested", "nested.jsonl"), "new",
	)
	childJSONL := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("nested", "subagents", "child.jsonl"), "child",
	)
	cursorProviderWriteJSONLTranscript(t, transcriptsDir, filepath.Join("mismatch", "other.jsonl"), "other")
	orphan := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("subagents", "orphan.jsonl"), "orphan",
	)

	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	setCursorTestResolver(t, provider, resolverRoot)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl", "*.txt"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 3)
	assert.ElementsMatch(t, []string{flatJSONL, nestedJSONL, childJSONL}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
		discovered[2].DisplayPath,
	})
	for _, source := range discovered {
		assert.Equal(t, AgentCursor, source.Provider)
		assert.Equal(t, DecodeCursorProjectDir(projectDir), source.ProjectHint)
		assert.Equal(t, SourceCwdResolved, source.CwdResolution.State)
		assert.Equal(t, normalizeCursorDir(resolvedWorkspace), source.CwdResolution.Path)
	}

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "remote~cursor:flat",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, flatJSONL, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: flatTxt,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, flatJSONL, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "nested",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, nestedJSONL, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, nestedJSONL, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "child",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, childJSONL, found.DisplayPath)

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "flat txt promotes to jsonl", path: flatTxt, want: flatJSONL},
		{name: "flat jsonl", path: flatJSONL, want: flatJSONL},
		{name: "nested txt promotes to jsonl", path: nestedTxt, want: nestedJSONL},
		{name: "nested jsonl", path: nestedJSONL, want: nestedJSONL},
		{name: "subagent child", path: childJSONL, want: childJSONL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{Path: tc.path, EventKind: "write", WatchRoot: root},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, tc.want, changed[0].DisplayPath)
		})
	}

	ignored, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: orphan, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored,
		"a subagents directory directly under agent-transcripts has no parent session")

	wrongRoot, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      flatJSONL,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "..", "other-root"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, wrongRoot)
}

func TestCursorStreamingDiscoveryUsesCanonicalMixedLayoutPrecedence(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	if filepath.Separator == '\\' {
		projectDir = "C-Users-fiona-Documents-demo"
	}
	resolverRoot := filepath.Join(root, "resolver")
	resolvedWorkspace := filepath.Join(resolverRoot, "Users", "fiona", "Documents", "demo")
	require.NoError(t, os.MkdirAll(resolvedWorkspace, 0o755))
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	flatJSONL := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, "mixed.jsonl", "canonical flat source",
	)
	cursorProviderWriteTranscript(
		t, transcriptsDir, filepath.Join("mixed", "mixed.txt"),
		"older nested source",
	)
	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	setCursorTestResolver(t, provider, resolverRoot)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, flatJSONL, discovered[0].DisplayPath)

	var streamed []SourceRef
	err = provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, streamed, 1)
	assert.Equal(t, flatJSONL, streamed[0].DisplayPath)
	assert.Equal(t, SourceCwdResolved, streamed[0].CwdResolution.State)
	assert.Equal(t, normalizeCursorDir(resolvedWorkspace), streamed[0].CwdResolution.Path)
}

func TestCursorProviderParseCarriesPathDerivedCwd(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-helix-Code-app"
	if filepath.Separator == '\\' {
		projectDir = "C-Users-helix-Code-app"
	}
	resolverRoot := filepath.Join(root, "resolver")
	resolvedWorkspace := filepath.Join(resolverRoot, "Users", "helix", "Code", "app")
	require.NoError(t, os.MkdirAll(resolvedWorkspace, 0o755))
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	sourcePath := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, "path-derived.jsonl", "path-derived",
	)
	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	setCursorTestResolver(t, provider, resolverRoot)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, normalizeCursorDir(resolvedWorkspace), outcome.Results[0].Result.Session.Cwd)
	assert.Equal(t, sourcePath,
		outcome.Results[0].Result.Session.File.Path)
}

func TestCursorStreamingDiscoveryPropagatesAuthoritativeResolutionErrors(t *testing.T) {
	t.Run("configured root", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "broken-root")
		require.NoError(t, os.Symlink(filepath.Join(parent, "missing"), root))
		provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
		require.True(t, ok)

		err := provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(SourceRef) error {
			return nil
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolve cursor root")
	})

	t.Run("project transcripts", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "Users-demo")
		require.NoError(t, os.MkdirAll(project, 0o755))
		require.NoError(t, os.Symlink(
			filepath.Join(root, "missing-transcripts"),
			filepath.Join(project, "agent-transcripts"),
		))
		provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
		require.True(t, ok)

		err := provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(SourceRef) error {
			return nil
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolve cursor transcripts")
	})
}

func TestCursorStreamingDiscoverySkipsTranscriptVanishedBeforeStat(t *testing.T) {
	root := t.TempDir()
	transcriptsDir := filepath.Join(root, "Users-demo", "agent-transcripts")
	healthy := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, "healthy.jsonl", "still on disk",
	)
	ctx := withStreamingDirectoryReader(t.Context(), func(
		_ context.Context, dir string, yield func(os.DirEntry) error,
	) error {
		if samePath(dir, transcriptsDir) {
			if err := yield(failingInfoDirEntry{
				name: "a-vanished.jsonl", err: os.ErrNotExist,
			}); err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := yield(entry); err != nil {
				return err
			}
		}
		return nil
	})
	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	var streamed []string
	err := provider.(StreamingDiscoverer).DiscoverEach(ctx, func(source SourceRef) error {
		streamed = append(streamed, source.DisplayPath)
		return nil
	})

	require.NoError(t, err,
		"a transcript deleted between enumeration and stat must not fail discovery")
	assert.Equal(t, []string{healthy}, streamed)
}

func TestCursorProviderResolvesDuplicateStemsWithinProject(t *testing.T) {
	root := t.TempDir()
	firstProject := "Users-fiona-Documents-first"
	secondProject := "Users-fiona-Documents-second"
	firstDir := filepath.Join(root, firstProject, "agent-transcripts")
	secondDir := filepath.Join(root, secondProject, "agent-transcripts")
	firstJSONL := cursorProviderWriteJSONLTranscript(t, firstDir, "shared.jsonl", "first")
	secondTxt := cursorProviderWriteTranscript(t, secondDir, "shared.txt", "second old")
	secondJSONL := cursorProviderWriteJSONLTranscript(t, secondDir, "shared.jsonl", "second new")

	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{firstJSONL, secondJSONL}, sourceDisplayPaths(discovered))

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: secondTxt,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, secondJSONL, found.DisplayPath)
	assert.Equal(t, DecodeCursorProjectDir(secondProject), found.ProjectHint)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: secondTxt, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, secondJSONL, changed[0].DisplayPath)
	assert.Equal(t, DecodeCursorProjectDir(secondProject), changed[0].ProjectHint)
}

func TestCursorProviderParse(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	resolverRoot := filepath.Join(root, "resolver")
	resolvedWorkspace := filepath.Join(resolverRoot, "Users", "fiona", "Documents", "demo")
	require.NoError(t, os.MkdirAll(resolvedWorkspace, 0o755))
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	sourcePath := filepath.Join(transcriptsDir, "parse.jsonl")
	recordedCwd := filepath.Join(root, "recorded-workspace")
	require.NoError(t, os.MkdirAll(recordedCwd, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	recordedCwdJSON, err := json.Marshal(recordedCwd)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sourcePath, []byte(
		`{"role":"user","message":{"content":"parse question"}}
		{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"working_directory":`+string(recordedCwdJSON)+`}},{"type":"tool_use","name":"Shell","parameters":{"working_directory":`+string(recordedCwdJSON)+`}},{"type":"text","text":"Done."}]}}`,
	), 0o644))
	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	setCursorTestResolver(t, provider, resolverRoot)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.False(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "cursor:parse", result.Result.Session.ID)
	assert.Equal(t, AgentCursor, result.Result.Session.Agent)
	assert.Equal(t, DecodeCursorProjectDir(projectDir), result.Result.Session.Project)
	assert.Equal(t, normalizeCursorDir(resolvedWorkspace), result.Result.Session.Cwd)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, sourcePath, result.Result.Session.File.Path)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Equal(t, "parse question", result.Result.Session.FirstMessage)
	assert.Len(t, result.Result.Messages, 2)
}

func TestCursorProviderFingerprintSkipsOversizedTranscriptHash(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	sourcePath := filepath.Join(transcriptsDir, "oversized.jsonl")
	require.NoError(t, os.MkdirAll(transcriptsDir, 0o755))
	file, err := os.Create(sourcePath)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(maxCursorTranscriptSize+1))
	require.NoError(t, file.Close())

	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fingerprint.Key)
	assert.Equal(t, int64(maxCursorTranscriptSize+1), fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.Empty(t, fingerprint.Hash)

	_, err = provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file too large")
}

func TestCursorPathFromSourceMaterializedFile(t *testing.T) {
	s := newCursorSourceSet([]string{t.TempDir()})
	path := filepath.Join(t.TempDir(), "abc.jsonl")
	got, ok := s.pathFromSource(SourceRef{
		Provider: AgentCursor,
		Opaque:   MaterializedFileSource{Path: path},
	})
	require.True(t, ok)
	assert.Equal(t, path, got)
}

func cursorProviderWriteTranscript(
	t *testing.T,
	dir string,
	name string,
	firstMessage string,
) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(
		path,
		[]byte("user:\n<user_query>"+firstMessage+"</user_query>\nassistant:\nDone.\n"),
		0o644,
	))
	return path
}

func cursorProviderWriteJSONLTranscript(
	t *testing.T,
	dir string,
	name string,
	firstMessage string,
) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(
		path,
		[]byte(`{"role":"user","message":{"content":"<user_query>`+firstMessage+`</user_query>"}}`+"\n"+
			`{"role":"assistant","message":{"content":"Done."}}`+"\n"),
		0o644,
	))
	return path
}

func TestCursorProviderResolutionIsOperationLocalAndFresh(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-helix-Code-app"
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	first := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, "11111111-2222-4333-8444-555555555555.jsonl", "one",
	)
	cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, "22222222-3333-4444-8555-666666666666.jsonl", "two",
	)
	workspaceRoot := t.TempDir()
	workspace := filepath.Join(workspaceRoot, "Users", "helix", "Code", "app")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	p := provider.(*cursorProvider)
	var calls int
	p.sources.resolutionResolver = func(
		project string, mode CursorResolveMode, hint string,
	) SourceCwdResolution {
		calls++
		return ResolveCursorWorkspaceDirResolution(
			workspaceRoot, project, hint, mode,
		)
	}

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, 1, calls)

	var streamed []SourceRef
	err = provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	)
	require.NoError(t, err)
	assert.Len(t, streamed, 2)
	assert.Equal(t, 2, calls)

	_, err = provider.Discover(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, calls, "a second discovery gets a fresh operation cache")

	require.NoError(t, os.MkdirAll(filepath.Join(workspaceRoot, "Users", "helix", "Code-app"), 0o755))
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: first,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, SourceCwdAmbiguous, found.CwdResolution.State)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: first, WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, SourceCwdAmbiguous, changed[0].CwdResolution.State)
}

func TestCursorProviderPathRewriterMakesResolutionRemote(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-helix-Code-app"
	path := cursorProviderWriteJSONLTranscript(
		t, filepath.Join(root, projectDir, "agent-transcripts"),
		"11111111-2222-4333-8444-555555555555.jsonl", "remote",
	)
	workspaceRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(workspaceRoot, "Users", "helix", "Code", "app"), 0o755,
	))
	originalProbe := probeGitRootForCwd
	t.Cleanup(func() { probeGitRootForCwd = originalProbe })
	originalReadDir := cursorReadDir
	t.Cleanup(func() { cursorReadDir = originalReadDir })
	originalStat := osStat
	t.Cleanup(func() { osStat = originalStat })
	var probeCalls int
	var readDirCalls int
	var statCalls int
	probeGitRootForCwd = func(path string) bool {
		probeCalls++
		return true
	}
	cursorReadDir = func(path string) ([]os.DirEntry, error) {
		readDirCalls++
		return os.ReadDir(path)
	}
	osStat = func(path string) (os.FileInfo, error) {
		statCalls++
		return os.Stat(path)
	}

	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots: []string{root}, PathRewriter: func(string) string { return "remote:" + path },
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, SourceCwdRemote, sources[0].CwdResolution.State)
	assert.Zero(t, probeCalls)
	assert.Zero(t, readDirCalls)
	assert.Zero(t, statCalls)
}

func TestCursorProviderSourceMachineMakesResolutionRemote(t *testing.T) {
	root := t.TempDir()
	path := cursorProviderWriteJSONLTranscript(
		t, filepath.Join(root, "Users-helix-Code-app", "agent-transcripts"),
		"11111111-2222-4333-8444-555555555555.jsonl", "remote machine",
	)
	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{root},
		Machine: "localbox",
		SourceMachines: map[string]string{
			root: "archivebox",
		},
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, SourceCwdRemote, sources[0].CwdResolution.State)
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: path, WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, SourceCwdRemote, changed[0].CwdResolution.State)
}

// cursorSubagentFixture adds two shapes Cursor never produces, which must
// stay undiscovered: a subagents directory with no parent session, and a
// subagent nested under another subagent.
type cursorSubagentFixture struct {
	root, projectDir, resolverRoot, resolvedWorkspace string
	parentJSONL, childJSONL, childTxt, promotedJSONL  string
	orphan, grandchild                                string
}

func newCursorSubagentFixture(t *testing.T) cursorSubagentFixture {
	t.Helper()
	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	if filepath.Separator == '\\' {
		projectDir = "C-Users-fiona-Documents-demo"
	}
	resolverRoot := filepath.Join(root, "resolver")
	resolvedWorkspace := filepath.Join(resolverRoot, "Users", "fiona", "Documents", "demo")
	require.NoError(t, os.MkdirAll(resolvedWorkspace, 0o755))
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	subagents := filepath.Join("parent", "subagents")
	f := cursorSubagentFixture{
		root: root, projectDir: projectDir,
		resolverRoot: resolverRoot, resolvedWorkspace: resolvedWorkspace,
	}
	f.parentJSONL = cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("parent", "parent.jsonl"), "delegate this",
	)
	f.childJSONL = cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join(subagents, "child-a.jsonl"), "child a task",
	)
	f.childTxt = cursorProviderWriteTranscript(
		t, transcriptsDir, filepath.Join(subagents, "child-b.txt"), "child b task",
	)
	cursorProviderWriteTranscript(
		t, transcriptsDir, filepath.Join(subagents, "child-c.txt"), "child c legacy",
	)
	f.promotedJSONL = cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join(subagents, "child-c.jsonl"), "child c task",
	)
	f.orphan = cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("subagents", "orphan.jsonl"), "orphan",
	)
	f.grandchild = cursorProviderWriteJSONLTranscript(
		t, transcriptsDir,
		filepath.Join(subagents, "child-a", "subagents", "grandchild.jsonl"),
		"grandchild",
	)
	return f
}

func (f cursorSubagentFixture) expectedPaths() []string {
	return []string{f.parentJSONL, f.childJSONL, f.childTxt, f.promotedJSONL}
}

func (f cursorSubagentFixture) newProvider(t *testing.T) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{f.root},
		Machine: "devbox",
	})
	require.True(t, ok)
	setCursorTestResolver(t, provider, f.resolverRoot)
	return provider
}

func TestCursorProviderDiscoversSubagentTranscriptsInBothWalks(t *testing.T) {
	f := newCursorSubagentFixture(t)
	provider := f.newProvider(t)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)

	var streamed []SourceRef
	err = provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	)
	require.NoError(t, err)

	for name, sources := range map[string][]SourceRef{
		"batch": discovered, "streaming": streamed,
	} {
		t.Run(name, func(t *testing.T) {
			paths := make([]string, 0, len(sources))
			for _, source := range sources {
				paths = append(paths, source.DisplayPath)
				assert.Equal(t, DecodeCursorProjectDir(f.projectDir), source.ProjectHint,
					"%s inherits the delegating session's project", source.DisplayPath)
				assert.Equal(t, SourceCwdResolved, source.CwdResolution.State)
				assert.Equal(t, normalizeCursorDir(f.resolvedWorkspace), source.CwdResolution.Path)
			}
			assert.ElementsMatch(t, f.expectedPaths(), paths)
			assert.NotContains(t, paths, f.orphan)
			assert.NotContains(t, paths, f.grandchild)
		})
	}
}

func TestCursorProviderParseLinksSubagentToParentSession(t *testing.T) {
	f := newCursorSubagentFixture(t)
	provider := f.newProvider(t)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	byPath := make(map[string]SourceRef, len(sources))
	for _, source := range sources {
		byPath[source.DisplayPath] = source
	}

	parse := func(t *testing.T, path string) ParsedSession {
		t.Helper()
		source, ok := byPath[path]
		require.True(t, ok, "discovered %s", path)
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		return outcome.Results[0].Result.Session
	}

	parent := parse(t, f.parentJSONL)
	assert.Equal(t, "cursor:parent", parent.ID)
	assert.Empty(t, parent.ParentSessionID)
	assert.Equal(t, RelNone, parent.RelationshipType)

	for name, tc := range map[string]struct {
		path, id, firstMessage string
	}{
		"jsonl child":  {path: f.childJSONL, id: "cursor:child-a", firstMessage: "child a task"},
		"legacy child": {path: f.childTxt, id: "cursor:child-b", firstMessage: "child b task"},
		"promoted":     {path: f.promotedJSONL, id: "cursor:child-c", firstMessage: "child c task"},
	} {
		t.Run(name, func(t *testing.T) {
			child := parse(t, tc.path)
			assert.Equal(t, tc.id, child.ID)
			assert.Equal(t, parent.ID, child.ParentSessionID)
			assert.Equal(t, RelSubagent, child.RelationshipType)
			assert.Equal(t, parent.Project, child.Project)
			assert.Equal(t, parent.Cwd, child.Cwd)
			assert.Equal(t, tc.firstMessage, child.FirstMessage)
		})
	}
}

func TestCursorDiscoveryPrefersTopLevelOverSubagentStem(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	if filepath.Separator == '\\' {
		projectDir = "C-Users-fiona-Documents-demo"
	}
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	// "aaa" sorts before "zzz.jsonl", so the subagent copy is listed first;
	// the top-level copy must still win in both walks.
	parent := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("aaa", "aaa.jsonl"), "parent",
	)
	cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("aaa", "subagents", "zzz.jsonl"), "child copy",
	)
	flat := cursorProviderWriteJSONLTranscript(t, transcriptsDir, "zzz.jsonl", "top-level copy")
	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	var streamed []SourceRef
	require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	))

	for name, sources := range map[string][]SourceRef{
		"batch": discovered, "streaming": streamed,
	} {
		t.Run(name, func(t *testing.T) {
			paths := make([]string, 0, len(sources))
			for _, source := range sources {
				paths = append(paths, source.DisplayPath)
			}
			assert.ElementsMatch(t, []string{parent, flat}, paths)
		})
	}
}

func TestCursorTopLevelTranscriptOutranksSubagentCopyRegardlessOfExtension(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	if filepath.Separator == '\\' {
		projectDir = "C-Users-fiona-Documents-demo"
	}
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	parent := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("parent", "parent.jsonl"), "parent",
	)
	topLevelTxt := cursorProviderWriteTranscript(t, transcriptsDir, "child.txt", "top-level copy")
	subagentJSONL := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("parent", "subagents", "child.jsonl"), "subagent copy",
	)
	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	var streamed []SourceRef
	require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	))
	for name, sources := range map[string][]SourceRef{
		"batch": discovered, "streaming": streamed,
	} {
		t.Run(name, func(t *testing.T) {
			paths := make([]string, 0, len(sources))
			for _, source := range sources {
				paths = append(paths, source.DisplayPath)
			}
			assert.ElementsMatch(t, []string{parent, topLevelTxt}, paths)
		})
	}

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "child"})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, topLevelTxt, found.DisplayPath, "find-by-ID follows the same tier rule")

	// A watcher event on either copy resolves to the canonical top-level
	// file, like a .txt event promoting to its .jsonl sibling.
	for name, path := range map[string]string{
		"top-level event": topLevelTxt, "subagent event": subagentJSONL,
	} {
		t.Run(name, func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path: path, EventKind: "write", WatchRoot: root,
			})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, topLevelTxt, changed[0].DisplayPath)
		})
	}
}

func TestCursorDiscoverySkipsSymlinkedSubagentTranscriptInBothWalks(t *testing.T) {
	root := t.TempDir()
	transcriptsDir := filepath.Join(root, "Users-demo", "agent-transcripts")
	parent := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("parent", "parent.jsonl"), "parent",
	)
	target := cursorProviderWriteJSONLTranscript(t, filepath.Join(root, "elsewhere"), "real.jsonl", "real")
	link := filepath.Join(transcriptsDir, "parent", "subagents", "child.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	var streamed []SourceRef
	require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	))
	for name, sources := range map[string][]SourceRef{
		"batch": discovered, "streaming": streamed,
	} {
		t.Run(name, func(t *testing.T) {
			paths := make([]string, 0, len(sources))
			for _, source := range sources {
				paths = append(paths, source.DisplayPath)
			}
			assert.Equal(t, []string{parent}, paths,
				"a symlinked transcript cannot be opened by parseSession, so no walk may yield it")
		})
	}
}
