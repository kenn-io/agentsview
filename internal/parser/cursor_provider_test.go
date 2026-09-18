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
	parentAssert := assert.New(t)
	parentRequire := require.New(t)

	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	resolverRoot := filepath.Join(root, "resolver")
	resolvedWorkspace := filepath.Join(resolverRoot, "Users", "fiona", "Documents", "demo")
	parentRequire.NoError(os.MkdirAll(resolvedWorkspace, 0o755))
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
	parentRequire.True(ok)
	setCursorTestResolver(t, provider, resolverRoot)

	plan, err := provider.WatchPlan(t.Context())
	parentRequire.NoError(err)
	parentRequire.Len(plan.Roots, 1)
	parentAssert.Equal(root, plan.Roots[0].Path)
	parentAssert.True(plan.Roots[0].Recursive)
	parentAssert.Equal([]string{"*.jsonl", "*.txt"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	parentRequire.NoError(err)
	parentRequire.Len(discovered, 3)
	parentAssert.ElementsMatch([]string{flatJSONL, nestedJSONL, childJSONL}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
		discovered[2].DisplayPath,
	})
	for _, source := range discovered {
		parentAssert.Equal(AgentCursor, source.Provider)
		parentAssert.Equal(DecodeCursorProjectDir(projectDir), source.ProjectHint)
		parentAssert.Equal(SourceCwdResolved, source.CwdResolution.State)
		parentAssert.Equal(normalizeCursorDir(resolvedWorkspace), source.CwdResolution.Path)
	}

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "remote~cursor:flat",
	})
	parentRequire.NoError(err)
	parentRequire.True(ok)
	parentAssert.Equal(flatJSONL, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: flatTxt,
	})
	parentRequire.NoError(err)
	parentRequire.True(ok)
	parentAssert.Equal(flatJSONL, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "nested",
	})
	parentRequire.NoError(err)
	parentRequire.True(ok)
	parentAssert.Equal(nestedJSONL, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	parentRequire.NoError(err)
	parentAssert.Equal(nestedJSONL, fingerprint.Key)
	parentAssert.Positive(fingerprint.Size)
	parentAssert.Positive(fingerprint.MTimeNS)
	parentAssert.NotEmpty(fingerprint.Hash)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "child",
	})
	parentRequire.NoError(err)
	parentRequire.True(ok)
	parentAssert.Equal(childJSONL, found.DisplayPath)

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
				t.Context(),
				ChangedPathRequest{Path: tc.path, EventKind: "write", WatchRoot: root},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, tc.want, changed[0].DisplayPath)
		})
	}

	ignored, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: orphan, EventKind: "write", WatchRoot: root},
	)
	parentRequire.NoError(err)
	parentAssert.Empty(ignored,
		"a subagents directory directly under agent-transcripts has no parent session")

	wrongRoot, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      flatJSONL,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "..", "other-root"),
		},
	)
	parentRequire.NoError(err)
	parentAssert.Empty(wrongRoot)
}

func TestCursorStreamingDiscoveryUsesCanonicalMixedLayoutPrecedence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	if filepath.Separator == '\\' {
		projectDir = "C-Users-fiona-Documents-demo"
	}
	resolverRoot := filepath.Join(root, "resolver")
	resolvedWorkspace := filepath.Join(resolverRoot, "Users", "fiona", "Documents", "demo")
	require.NoError(os.MkdirAll(resolvedWorkspace, 0o755))
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	flatJSONL := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, "mixed.jsonl", "canonical flat source",
	)
	cursorProviderWriteTranscript(
		t, transcriptsDir, filepath.Join("mixed", "mixed.txt"),
		"older nested source",
	)
	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	setCursorTestResolver(t, provider, resolverRoot)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(flatJSONL, discovered[0].DisplayPath)

	var streamed []SourceRef
	err = provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	)
	require.NoError(err)
	require.Len(streamed, 1)
	assert.Equal(flatJSONL, streamed[0].DisplayPath)
	assert.Equal(SourceCwdResolved, streamed[0].CwdResolution.State)
	assert.Equal(normalizeCursorDir(resolvedWorkspace), streamed[0].CwdResolution.Path)
}

func TestCursorProviderParseCarriesPathDerivedCwd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := "Users-helix-Code-app"
	if filepath.Separator == '\\' {
		projectDir = "C-Users-helix-Code-app"
	}
	resolverRoot := filepath.Join(root, "resolver")
	resolvedWorkspace := filepath.Join(resolverRoot, "Users", "helix", "Code", "app")
	require.NoError(os.MkdirAll(resolvedWorkspace, 0o755))
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	sourcePath := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, "path-derived.jsonl", "path-derived",
	)
	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	setCursorTestResolver(t, provider, resolverRoot)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal(normalizeCursorDir(resolvedWorkspace), outcome.Results[0].Result.Session.Cwd)
	assert.Equal(sourcePath,
		outcome.Results[0].Result.Session.File.Path)
}

func TestCursorStreamingDiscoveryPropagatesAuthoritativeResolutionErrors(t *testing.T) {
	t.Run("configured root", func(t *testing.T) {
		require := require.New(t)

		parent := t.TempDir()
		root := filepath.Join(parent, "broken-root")
		require.NoError(os.Symlink(filepath.Join(parent, "missing"), root))
		provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
		require.True(ok)

		err := provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(SourceRef) error {
			return nil
		})

		require.Error(err)
		assert.Contains(t, err.Error(), "resolve cursor root")
	})

	t.Run("project transcripts", func(t *testing.T) {
		require := require.New(t)

		root := t.TempDir()
		project := filepath.Join(root, "Users-demo")
		require.NoError(os.MkdirAll(project, 0o755))
		require.NoError(os.Symlink(
			filepath.Join(root, "missing-transcripts"),
			filepath.Join(project, "agent-transcripts"),
		))
		provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
		require.True(ok)

		err := provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(SourceRef) error {
			return nil
		})

		require.Error(err)
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
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	assert.ElementsMatch([]string{firstJSONL, secondJSONL}, sourceDisplayPaths(discovered))

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: secondTxt,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(secondJSONL, found.DisplayPath)
	assert.Equal(DecodeCursorProjectDir(secondProject), found.ProjectHint)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: secondTxt, EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(secondJSONL, changed[0].DisplayPath)
	assert.Equal(DecodeCursorProjectDir(secondProject), changed[0].ProjectHint)
}

func TestCursorProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	resolverRoot := filepath.Join(root, "resolver")
	resolvedWorkspace := filepath.Join(resolverRoot, "Users", "fiona", "Documents", "demo")
	require.NoError(os.MkdirAll(resolvedWorkspace, 0o755))
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	sourcePath := filepath.Join(transcriptsDir, "parse.jsonl")
	recordedCwd := filepath.Join(root, "recorded-workspace")
	require.NoError(os.MkdirAll(recordedCwd, 0o755))
	require.NoError(os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	recordedCwdJSON, err := json.Marshal(recordedCwd)
	require.NoError(err)
	require.NoError(os.WriteFile(sourcePath, []byte(
		`{"role":"user","message":{"content":"parse question"}}
		{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"working_directory":`+string(recordedCwdJSON)+`}},{"type":"tool_use","name":"Shell","parameters":{"working_directory":`+string(recordedCwdJSON)+`}},{"type":"text","text":"Done."}]}}`,
	), 0o644))
	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	setCursorTestResolver(t, provider, resolverRoot)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.False(outcome.ForceReplace)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal("cursor:parse", result.Result.Session.ID)
	assert.Equal(AgentCursor, result.Result.Session.Agent)
	assert.Equal(DecodeCursorProjectDir(projectDir), result.Result.Session.Project)
	assert.Equal(normalizeCursorDir(resolvedWorkspace), result.Result.Session.Cwd)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(sourcePath, result.Result.Session.File.Path)
	assert.Equal(fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Equal("parse question", result.Result.Session.FirstMessage)
	assert.Len(result.Result.Messages, 2)
}

func TestCursorProviderFingerprintSkipsOversizedTranscriptHash(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	sourcePath := filepath.Join(transcriptsDir, "oversized.jsonl")
	require.NoError(os.MkdirAll(transcriptsDir, 0o755))
	file, err := os.Create(sourcePath)
	require.NoError(err)
	require.NoError(file.Truncate(maxCursorTranscriptSize + 1))
	require.NoError(file.Close())

	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	assert.Equal(sourcePath, fingerprint.Key)
	assert.Equal(int64(maxCursorTranscriptSize+1), fingerprint.Size)
	assert.Positive(fingerprint.MTimeNS)
	assert.Empty(fingerprint.Hash)

	_, err = provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.Error(err)
	assert.Contains(err.Error(), "file too large")
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
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(os.MkdirAll(workspace, 0o755))

	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(ok)
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
	require.NoError(err)
	require.Len(discovered, 2)
	assert.Equal(1, calls)

	var streamed []SourceRef
	err = provider.(StreamingDiscoverer).DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	)
	require.NoError(err)
	assert.Len(streamed, 2)
	assert.Equal(2, calls)

	_, err = provider.Discover(t.Context())
	require.NoError(err)
	assert.Equal(3, calls, "a second discovery gets a fresh operation cache")

	require.NoError(os.MkdirAll(filepath.Join(workspaceRoot, "Users", "helix", "Code-app"), 0o755))
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: first,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(SourceCwdAmbiguous, found.CwdResolution.State)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: first, WatchRoot: root,
	})
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(SourceCwdAmbiguous, changed[0].CwdResolution.State)
}

func TestCursorProviderPathRewriterMakesResolutionRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := "Users-helix-Code-app"
	path := cursorProviderWriteJSONLTranscript(
		t, filepath.Join(root, projectDir, "agent-transcripts"),
		"11111111-2222-4333-8444-555555555555.jsonl", "remote",
	)
	workspaceRoot := t.TempDir()
	require.NoError(os.MkdirAll(
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
		Roots:        []string{root},
		PathRewriter: func(string) string { return "remote:" + path },
		MetadataDirs: map[string][]string{root: {filepath.Join(root, "chats")}},
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(SourceCwdRemote, sources[0].CwdResolution.State)
	assert.Zero(probeCalls)
	assert.Zero(readDirCalls)
	assert.Zero(statCalls)
	watchPlan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	for _, watchRoot := range watchPlan.Roots {
		assert.False(samePath(watchRoot.Path, filepath.Join(root, "chats")))
	}
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:      filepath.Join(root, "chats", "store.db"),
		WatchRoot: filepath.Join(root, "chats"),
		EventKind: "write",
	})
	require.NoError(err)
	assert.Empty(changed)
}

func TestCursorProviderSourceMachineMakesResolutionRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(SourceCwdRemote, sources[0].CwdResolution.State)
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: path, WatchRoot: root,
	})
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(SourceCwdRemote, changed[0].CwdResolution.State)
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
			assert := assert.New(t)

			paths := make([]string, 0, len(sources))
			for _, source := range sources {
				paths = append(paths, source.DisplayPath)
				assert.Equal(DecodeCursorProjectDir(f.projectDir), source.ProjectHint,
					"%s inherits the delegating session's project", source.DisplayPath)
				assert.Equal(SourceCwdResolved, source.CwdResolution.State)
				assert.Equal(normalizeCursorDir(f.resolvedWorkspace), source.CwdResolution.Path)
			}
			assert.ElementsMatch(f.expectedPaths(), paths)
			assert.NotContains(paths, f.orphan)
			assert.NotContains(paths, f.grandchild)
		})
	}
}

func TestCursorProviderParseLinksSubagentToParentSession(t *testing.T) {
	parentAssert := assert.New(t)

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
	parentAssert.Equal("cursor:parent", parent.ID)
	parentAssert.Empty(parent.ParentSessionID)
	parentAssert.Equal(RelNone, parent.RelationshipType)

	for name, tc := range map[string]struct {
		path, id, firstMessage string
	}{
		"jsonl child":  {path: f.childJSONL, id: "cursor:child-a", firstMessage: "child a task"},
		"legacy child": {path: f.childTxt, id: "cursor:child-b", firstMessage: "child b task"},
		"promoted":     {path: f.promotedJSONL, id: "cursor:child-c", firstMessage: "child c task"},
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)

			child := parse(t, tc.path)
			assert.Equal(tc.id, child.ID)
			assert.Equal(parent.ID, child.ParentSessionID)
			assert.Equal(RelSubagent, child.RelationshipType)
			assert.Equal(parent.Project, child.Project)
			assert.Equal(parent.Cwd, child.Cwd)
			assert.Equal(tc.firstMessage, child.FirstMessage)
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
	parentRequire := require.New(t)

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
	parentRequire.True(ok)

	discovered, err := provider.Discover(t.Context())
	parentRequire.NoError(err)
	var streamed []SourceRef
	parentRequire.NoError(provider.(StreamingDiscoverer).DiscoverEach(
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
	parentRequire.NoError(err)
	parentRequire.True(ok)
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

func TestCursorDiscoverySkipsSymlinkedTranscriptsInBothWalks(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	transcriptsDir := filepath.Join(root, "Users-demo", "agent-transcripts")
	parent := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("parent", "parent.jsonl"), "parent",
	)
	child := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("parent", "subagents", "nested.jsonl"), "child",
	)
	target := cursorProviderWriteJSONLTranscript(t, filepath.Join(root, "elsewhere"), "real.jsonl", "real")
	for _, link := range []string{
		filepath.Join(transcriptsDir, "parent", "subagents", "child.jsonl"),
		filepath.Join(transcriptsDir, "nested", "nested.jsonl"),
	} {
		require.NoError(os.MkdirAll(filepath.Dir(link), 0o755))
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
	}
	provider, ok := NewProvider(AgentCursor, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	var streamed []SourceRef
	require.NoError(provider.(StreamingDiscoverer).DiscoverEach(
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
			assert.ElementsMatch(t, []string{parent, child}, paths,
				"symlinked transcripts must neither be yielded nor shadow regular transcripts")
		})
	}
}

func TestCursorProviderDeduplicatesChildStemAcrossParents(t *testing.T) {
	root := t.TempDir()
	transcriptsDir := filepath.Join(root, "Users-demo", "agent-transcripts")
	parentA := cursorProviderWriteJSONLTranscript(t, transcriptsDir, filepath.Join("aaa", "aaa.jsonl"), "a")
	parentB := cursorProviderWriteJSONLTranscript(t, transcriptsDir, filepath.Join("bbb", "bbb.jsonl"), "b")
	// Same child stem under two parents: the first parent in directory order
	// wins for equal extensions, and a .jsonl copy beats a .txt copy anywhere.
	firstParentCopy := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("aaa", "subagents", "same.jsonl"), "from a",
	)
	secondParentCopy := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("bbb", "subagents", "same.jsonl"), "from b",
	)
	legacy := cursorProviderWriteTranscript(
		t, transcriptsDir, filepath.Join("aaa", "subagents", "mixed.txt"), "legacy from a",
	)
	upgraded := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("bbb", "subagents", "mixed.jsonl"), "from b",
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
	want := []string{parentA, parentB, firstParentCopy, upgraded}
	for name, sources := range map[string][]SourceRef{
		"batch": discovered, "streaming": streamed,
	} {
		t.Run(name, func(t *testing.T) {
			paths := make([]string, 0, len(sources))
			for _, source := range sources {
				paths = append(paths, source.DisplayPath)
			}
			assert.ElementsMatch(t, want, paths)
		})
	}
	for _, tt := range []struct{ event, want string }{
		{secondParentCopy, firstParentCopy},
		{legacy, upgraded},
	} {
		t.Run(filepath.Base(tt.event), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path: tt.event, WatchRoot: root,
			})
			require.NoError(err)
			require.Len(changed, 1)
			assert.Equal(tt.want, changed[0].DisplayPath)

			found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				StoredFilePath: tt.event,
			})
			require.NoError(err)
			require.True(ok)
			assert.Equal(tt.want, found.DisplayPath)
		})
	}
}
