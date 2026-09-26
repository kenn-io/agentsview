package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodebuffFindFile_RejectsHostileRawIDs pins the fail-closed
// traversal defense in codebuffFindFile: raw IDs containing path
// separators, "." or ".." segments, absolute paths, empty segments,
// or non-timestamp session names must never resolve — in particular
// they must never reach the decoy chat-messages.json staged outside
// the root, which is exactly where "..:<ts>" would land if the
// single-component checks were removed.
func TestCodebuffFindFile_RejectsHostileRawIDs(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	ts := "2026-07-15T20-01-32.065Z"
	valid := filepath.Join(root, "proj", "chats", ts, "chat-messages.json")
	codebuffWriteFile(t, valid, "[]")
	// Decoy outside the root: <base>/chats/<ts>/chat-messages.json is
	// the file filepath.Join(root, "..", "chats", ts, ...) collapses to.
	decoy := filepath.Join(base, "chats", ts, "chat-messages.json")
	codebuffWriteFile(t, decoy, "[]")

	sep := string(filepath.Separator)
	cases := []struct {
		name  string
		rawID string
	}{
		{"empty", ""},
		{"project dot-dot", "..:" + ts},
		{"project dot", ".:" + ts},
		{"project empty", ":" + ts},
		{"project with slash", "a/b:" + ts},
		{"project with platform separator", "a" + sep + "b:" + ts},
		{"project absolute path", sep + "abs:" + ts},
		{"timestamp with traversal", "proj:../" + ts},
		{"timestamp dot-dot", "proj:.."},
		{"non-timestamp session name", "proj:not-a-timestamp"},
		{"legacy dot-dot", ".."},
		{"legacy dot", "."},
		{"legacy with slash", "../" + ts},
		{"legacy with platform separator", ".." + sep + ts},
		{"legacy absolute path", decoy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			match, ok := codebuffFindFile(root, tc.rawID)
			assert.False(t, ok, "hostile rawID %q must fail closed", tc.rawID)
			assert.NotEqual(t, decoy, match.Path,
				"hostile rawID %q must never resolve to a file outside the root", tc.rawID)
			assert.Empty(t, match.Path)
		})
	}
}

// TestCodebuffFindFile_ResolvesValidIDs pins the two accepted rawID
// shapes: "project:timestamp" resolves directly, and a bare legacy
// timestamp searches all project subdirectories.
func TestCodebuffFindFile_ResolvesValidIDs(t *testing.T) {
	root := t.TempDir()
	ts := "2026-07-15T20-01-32.065Z"
	valid := filepath.Join(root, "proj", "chats", ts, "chat-messages.json")
	codebuffWriteFile(t, valid, "[]")

	match, ok := codebuffFindFile(root, "proj:"+ts)
	require.True(t, ok, "project:timestamp rawID must resolve")
	assert.Equal(t, valid, match.Path)
	assert.Equal(t, "proj", match.ProjectHint)

	match, ok = codebuffFindFile(root, ts)
	require.True(t, ok, "legacy bare timestamp rawID must resolve")
	assert.Equal(t, valid, match.Path)
	assert.Equal(t, "proj", match.ProjectHint)

	_, ok = codebuffFindFile(root, "other:"+ts)
	assert.False(t, ok, "wrong project must not resolve")
	_, ok = codebuffFindFile(root, "proj:2030-01-01T00-00-00.000Z")
	assert.False(t, ok, "unknown timestamp must not resolve")
}

// TestCodebuffChangedPathRelevance pins the watch prefilter's classification.
// Data files are data-bearing; debug siblings (log.jsonl,
// trace.jsonl) and atomic-write temp siblings are non-data; everything the
// classifier cannot reason about -- session directories, nested unknown
// paths, paths outside the deciding root, or a sibling data file under a
// *different* configured root -- stays unclassified so the engine keeps its
// fallback.
func TestCodebuffChangedPathRelevance(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	ts := "2026-07-15T10-00-00.000Z"
	sessionDir := filepath.Join(rootA, "proj", "chats", ts)
	// The relevance method only consults s.roots; a bare source set with
	// the two roots stands in for a fully configured provider.
	sourceSet := codebuffSourceSet{singleFileSourceSet{
		agent: AgentCodebuff,
		roots: []string{rootA, rootB},
	}}
	relevance := func(watchRoot, path string) ChangedPathRelevance {
		got, err := sourceSet.ChangedPathRelevance(
			t.Context(), ChangedPathRequest{WatchRoot: watchRoot, Path: path},
		)
		require.NoError(t, err)
		return got
	}

	cases := []struct {
		name      string
		watchRoot string
		path      string
		want      ChangedPathRelevance
	}{
		{
			name: "primary transcript is data bearing",
			path: filepath.Join(sessionDir, "chat-messages.json"),
			want: ChangedPathDataBearing,
		},
		{
			name: "run-state companion is data bearing",
			path: filepath.Join(sessionDir, "run-state.json"),
			want: ChangedPathDataBearing,
		},
		{
			name: "chat-meta companion is data bearing",
			path: filepath.Join(sessionDir, "chat-meta.json"),
			want: ChangedPathDataBearing,
		},
		{
			name: "debug log sibling is non-data",
			path: filepath.Join(sessionDir, "log.jsonl"),
			want: ChangedPathNonData,
		},
		{
			name: "trace sibling is non-data",
			path: filepath.Join(sessionDir, "trace.jsonl"),
			want: ChangedPathNonData,
		},
		{
			name: "atomic temp sibling of transcript is non-data",
			path: filepath.Join(sessionDir, "chat-messages.json.4242.6f9619ff-8b86-d011-b42d-00c04fc964ff.tmp"),
			want: ChangedPathNonData,
		},
		{
			name: "atomic temp sibling of companion is non-data",
			path: filepath.Join(sessionDir, "run-state.json.7.0a4a2a1e-1a2b-4c3d-9e8f-001122334455.tmp"),
			want: ChangedPathNonData,
		},
		{
			name: "session directory event stays unclassified",
			path: sessionDir,
			want: ChangedPathUnclassified,
		},
		{
			name: "nested unknown path stays unclassified",
			path: filepath.Join(sessionDir, "nested", "deeper", "file"),
			want: ChangedPathUnclassified,
		},
		{
			name:      "path outside the deciding watch root stays unclassified",
			watchRoot: rootB,
			path:      filepath.Join(sessionDir, "log.jsonl"),
			want:      ChangedPathUnclassified,
		},
		{
			name:      "data file under the deciding watch root is data bearing",
			watchRoot: rootA,
			path:      filepath.Join(sessionDir, "chat-messages.json"),
			want:      ChangedPathDataBearing,
		},
		{
			name: "debug sibling under the second configured root is non-data",
			path: filepath.Join(rootB, "proj", "chats", ts, "log.jsonl"),
			want: ChangedPathNonData,
		},
		{
			name: "data file outside every configured root stays unclassified",
			path: filepath.Join(t.TempDir(), "proj", "chats", ts, "chat-messages.json"),
			want: ChangedPathUnclassified,
		},
		{
			name: "path above the root stays unclassified",
			path: filepath.Join(rootA, "proj", "chats", "other.txt"),
			want: ChangedPathUnclassified,
		},
		{
			name: "temp sibling of non-data debug file is also non-data",
			path: filepath.Join(sessionDir, "log.jsonl.9.11111111-2222-3333-4444-555555555555.tmp"),
			want: ChangedPathNonData,
		},
		{
			name: "dot segments collapsed inside the root classify normally",
			path: filepath.Join(sessionDir, "..", ts, "chat-messages.json"),
			want: ChangedPathDataBearing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := relevance(tc.watchRoot, tc.path)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("empty watch root falls back to provider roots", func(t *testing.T) {
		assert.Equal(t, ChangedPathDataBearing,
			relevance("", filepath.Join(sessionDir, "chat-messages.json")))
		assert.Equal(t, ChangedPathNonData,
			relevance("", filepath.Join(sessionDir, "log.jsonl")))
		// The path is under rootB, so rootA's answer (unclassified) does not
		// shadow rootB's data-bearing answer.
		assert.Equal(t, ChangedPathNonData,
			relevance("", filepath.Join(rootB, "proj", "chats", ts, "log.jsonl")))
		// Outside every provider root: unclassified.
		assert.Equal(t, ChangedPathUnclassified,
			relevance("", filepath.Join(t.TempDir(), "chat-messages.json")))
	})

	t.Run("data file set matches the parser inputs", func(t *testing.T) {
		require.Contains(t, codebuffDataFilenames, "chat-messages.json")
		require.Contains(t, codebuffDataFilenames, "run-state.json")
		require.Contains(t, codebuffDataFilenames, "chat-meta.json")
	})
}

// TestCodebuffClassifyPathDataBearingAndMissing pins the narrowed classifier's
// remaining contracts: the three data files map to the session transcript,
// non-data and temp siblings no longer do, and a deleted transcript still
// maps under allowMissing (the tombstone path).
func TestCodebuffClassifyPathDataBearingAndMissing(t *testing.T) {
	root := t.TempDir()
	ts := "2026-07-15T10-00-00.000Z"
	sessionDir := filepath.Join(root, "proj", "chats", ts)
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "chat-messages.json"), []byte("[]"), 0o644))

	for _, name := range []string{
		"chat-messages.json", "run-state.json", "chat-meta.json",
	} {
		match, ok := codebuffClassifyPath(
			root, filepath.Join(sessionDir, name), false,
		)
		require.True(t, ok, "%s must classify as a data file", name)
		assert.Equal(t, filepath.Join(sessionDir, "chat-messages.json"), match.Path)
		assert.Equal(t, "proj", match.ProjectHint)
	}

	for _, name := range []string{
		"log.jsonl", "trace.jsonl",
		"chat-messages.json.4242.6f9619ff-8b86-d011-b42d-00c04fc964ff.tmp",
	} {
		_, ok := codebuffClassifyPath(
			root, filepath.Join(sessionDir, name), false,
		)
		assert.False(t, ok, "%s must not map to the transcript", name)
	}

	// allowMissing: the deleted transcript still maps so deletion events
	// can reach the tombstone path.
	require.NoError(t, os.Remove(filepath.Join(sessionDir, "chat-messages.json")))
	match, ok := codebuffClassifyPath(
		root, filepath.Join(sessionDir, "chat-messages.json"), true,
	)
	require.True(t, ok, "a deleted transcript must still map under allowMissing")
	assert.Equal(t, filepath.Join(sessionDir, "chat-messages.json"), match.Path)
}

// TestCodebuffWatchPlanRecursiveWithIncludeGlobs pins the watch plan shape
// the changed-path relevance prefilter assumes: recursive roots so nested
// <project>/chats/<timestamp> session directories are observed. Nobody
// should "fix" the debug-sibling noise by narrowing the watch plan instead
// of classifying paths -- that would break new-session discovery.
func TestCodebuffWatchPlanRecursiveWithIncludeGlobs(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	plan := codebuffWatchRoots(roots)
	require.Len(t, plan, len(roots))
	for i, root := range plan {
		assert.Equal(t, roots[i], root.Path)
		assert.True(t, root.Recursive,
			"codebuff watch roots must stay recursive so nested session "+
				"directories are discovered")
		assert.Equal(t,
			[]string{"chat-messages.json", "run-state.json", "chat-meta.json"},
			root.IncludeGlobs,
			"include globs must keep declaring the three data files")
	}
}
