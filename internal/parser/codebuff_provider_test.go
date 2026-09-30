package parser

import (
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

	match, ok = codebuffFindFile(root, "proj:"+ts+"__subagent__agent-1")
	require.True(t, ok, "subagent rawID must resolve to its parent transcript")
	assert.Equal(t, valid, match.Path)

	_, ok = codebuffFindFile(root, "other:"+ts)
	assert.False(t, ok, "wrong project must not resolve")
	_, ok = codebuffFindFile(root, "proj:2030-01-01T00-00-00.000Z")
	assert.False(t, ok, "unknown timestamp must not resolve")
}

func TestCodebuffChangedPathRelevance(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	ts := "2026-07-15T10-00-00.000Z"
	sessionDir := filepath.Join(rootA, "proj", "chats", ts)
	sourceSet := codebuffSourceSet{singleFileSourceSet{
		agent: AgentCodebuff,
		roots: []string{rootA, rootB},
	}}

	cases := []struct {
		name      string
		watchRoot string
		path      string
		want      ChangedPathRelevance
	}{
		{
			name:      "transcript is data bearing",
			watchRoot: rootA,
			path:      filepath.Join(sessionDir, "chat-messages.json"),
			want:      ChangedPathDataBearing,
		},
		{
			name:      "debug log is non-data",
			watchRoot: rootA,
			path:      filepath.Join(sessionDir, "log.jsonl"),
			want:      ChangedPathNonData,
		},
		{
			name:      "atomic temp sibling is non-data",
			watchRoot: rootA,
			path:      filepath.Join(sessionDir, "chat-messages.json.4242.6f9619ff-8b86-d011-b42d-00c04fc964ff.tmp"),
			want:      ChangedPathNonData,
		},
		{
			name:      "session directory stays unclassified",
			watchRoot: rootA,
			path:      sessionDir,
			want:      ChangedPathUnclassified,
		},
		{
			name:      "path outside the watch root stays unclassified",
			watchRoot: rootB,
			path:      filepath.Join(sessionDir, "log.jsonl"),
			want:      ChangedPathUnclassified,
		},
		{
			name:      "debug log under the second root is non-data",
			watchRoot: rootB,
			path:      filepath.Join(rootB, "proj", "chats", ts, "log.jsonl"),
			want:      ChangedPathNonData,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sourceSet.ChangedPathRelevance(
				t.Context(), ChangedPathRequest{WatchRoot: tc.watchRoot, Path: tc.path},
			)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCodebuffClassifyPathMissingTranscript(t *testing.T) {
	root := t.TempDir()
	chatPath := filepath.Join(
		root, "proj", "chats", "2026-07-15T10-00-00.000Z", "chat-messages.json",
	)

	match, ok := codebuffClassifyPath(root, chatPath, true)
	require.True(t, ok, "a deleted transcript must still map under allowMissing")
	assert.Equal(t, chatPath, match.Path)
}
