package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentCursor)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestCursorProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	projectDir := "Users-fiona-Documents-demo"
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	flatTxt := cursorProviderWriteTranscript(t, transcriptsDir, "flat.txt", "old")
	flatJSONL := cursorProviderWriteJSONLTranscript(t, transcriptsDir, "flat.jsonl", "new")
	nestedTxt := cursorProviderWriteTranscript(t, transcriptsDir, filepath.Join("nested", "nested.txt"), "old")
	nestedJSONL := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("nested", "nested.jsonl"), "new",
	)
	cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, filepath.Join("nested", "subagents", "child.jsonl"), "child",
	)
	cursorProviderWriteJSONLTranscript(t, transcriptsDir, filepath.Join("mismatch", "other.jsonl"), "other")

	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl", "*.txt"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{flatJSONL, nestedJSONL}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
	})
	for _, source := range discovered {
		assert.Equal(t, AgentCursor, source.Provider)
		assert.Equal(t, DecodeCursorProjectDir(projectDir), source.ProjectHint)
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

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "flat txt promotes to jsonl", path: flatTxt, want: flatJSONL},
		{name: "flat jsonl", path: flatJSONL, want: flatJSONL},
		{name: "nested txt promotes to jsonl", path: nestedTxt, want: nestedJSONL},
		{name: "nested jsonl", path: nestedJSONL, want: nestedJSONL},
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
		ChangedPathRequest{
			Path:      filepath.Join(transcriptsDir, "nested", "subagents", "child.jsonl"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored)

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
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	sourcePath := cursorProviderWriteJSONLTranscript(
		t, transcriptsDir, "parse.jsonl", "parse question",
	)
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
