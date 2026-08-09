package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codebuffMakeSessionDir creates a minimal Codebuff session
// directory under root/<project>/chats/<rawID> by writing an
// empty chat-messages.json. run-state.json is not written so the
// resolver falls back to whichever roots list the session was
// found in (Codebuff or Freebuff) — identical to what a
// bare-timestamp lookup at the CLI boundary does.
func codebuffMakeSessionDir(
	t *testing.T, root, project, rawID string,
) {
	t.Helper()
	dir := filepath.Join(root, project, "chats", rawID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "chat-messages.json"),
		[]byte("[]"),
		0o644,
	))
}

// TestFindCodebuffFreebuffMatchesSingle pins the happy path:
// when exactly one (agent, project, timestamp) entry exists on
// disk, the helper returns one match whose CanonicalID matches
// the agent type and project directory name.
func TestFindCodebuffFreebuffMatchesSingle(t *testing.T) {
	root := t.TempDir()
	codebuffMakeSessionDir(
		t, root, "my-project", "2026-07-16T00-09-00.236Z",
	)

	matches := FindCodebuffFreebuffMatches(
		[]CodebuffFamilyRoots{
			{Agent: AgentCodebuff, Roots: []string{root}},
			{Agent: AgentFreebuff, Roots: nil},
		},
		"2026-07-16T00-09-00.236Z",
	)
	require.Len(t, matches, 1)
	assert.Equal(t, AgentCodebuff, matches[0].Agent)
	assert.Equal(t, "my-project", matches[0].ProjectHint)
	assert.Equal(t,
		"codebuff:my-project:2026-07-16T00-09-00.236Z",
		matches[0].CanonicalID(),
	)
}

// TestFindCodebuffFreebuffMatchesAmbiguous pins duplicate
// timestamp handling: when two projects share a timestamp the
// helper returns both matches so the caller can surface the
// ambiguity error instead of silently picking one.
func TestFindCodebuffFreebuffMatchesAmbiguous(t *testing.T) {
	root := t.TempDir()
	codebuffMakeSessionDir(
		t, root, "project-a", "2026-07-16T00-09-00.236Z",
	)
	codebuffMakeSessionDir(
		t, root, "project-b", "2026-07-16T00-09-00.236Z",
	)

	matches := FindCodebuffFreebuffMatches(
		[]CodebuffFamilyRoots{
			{Agent: AgentCodebuff, Roots: []string{root}},
		},
		"2026-07-16T00-09-00.236Z",
	)
	require.Len(t, matches, 2)
	cands := []string{
		matches[0].CanonicalID(),
		matches[1].CanonicalID(),
	}
	assert.Contains(t, cands,
		"codebuff:project-a:2026-07-16T00-09-00.236Z")
	assert.Contains(t, cands,
		"codebuff:project-b:2026-07-16T00-09-00.236Z")
}

// TestFindCodebuffFreebuffMatchesFreebuff pins Freebuff prefix
// tagging: matches under the Freebuff roots list must be tagged
// with AgentFreebuff so the canonical ID uses the freebuff:
// prefix that matches the on-disk agentType, not codebuff:.
func TestFindCodebuffFreebuffMatchesFreebuff(t *testing.T) {
	freebuffRoot := t.TempDir()
	codebuffMakeSessionDir(
		t, freebuffRoot, "free-project", "2026-07-16T00-09-00.236Z",
	)

	matches := FindCodebuffFreebuffMatches(
		[]CodebuffFamilyRoots{
			{Agent: AgentCodebuff, Roots: nil},
			{Agent: AgentFreebuff, Roots: []string{freebuffRoot}},
		},
		"2026-07-16T00-09-00.236Z",
	)
	require.Len(t, matches, 1)
	assert.Equal(t, AgentFreebuff, matches[0].Agent)
	assert.Equal(t,
		"freebuff:free-project:2026-07-16T00-09-00.236Z",
		matches[0].CanonicalID(),
	)
}

// TestFindCodebuffFreebuffMatchesMiss pins the no-match path:
// a rawID that does not name any existing chats subdirectory
// returns nil so the caller surfaces the "not found" error
// rather than picking the wrong session.
func TestFindCodebuffFreebuffMatchesMiss(t *testing.T) {
	root := t.TempDir()
	codebuffMakeSessionDir(
		t, root, "my-project", "2026-07-16T00-09-00.236Z",
	)

	matches := FindCodebuffFreebuffMatches(
		[]CodebuffFamilyRoots{
			{Agent: AgentCodebuff, Roots: []string{root}},
		},
		"2030-01-01T00-00-00.000Z",
	)
	assert.Empty(t, matches,
		"unknown timestamp must return no matches")
}

// TestFindCodebuffFreebuffMatchesRejectsTraversal pins the
// fail-closed contract for hostile rawIDs: an ID containing path
// separators or "."/".." segments must return no matches (and not
// panic), even when the traversal-shaped ID points at a real
// chat-messages.json outside the root.
func TestFindCodebuffFreebuffMatchesRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	codebuffMakeSessionDir(
		t, root, "proj", "2026-07-16T00-09-00.236Z",
	)
	// Decoy outside the root: filepath.Join(root, "proj", "chats",
	// "../../../x", "chat-messages.json") collapses to
	// <base>/x/chat-messages.json.
	require.NoError(t, os.MkdirAll(filepath.Join(base, "x"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(base, "x", "chat-messages.json"),
		[]byte("[]"),
		0o644,
	))

	pairs := []CodebuffFamilyRoots{
		{Agent: AgentCodebuff, Roots: []string{root}},
	}
	sep := string(filepath.Separator)
	hostile := []string{
		"../../../x",
		".." + sep + ".." + sep + ".." + sep + "x",
		"..",
		".",
		"a/b",
		"../2026-07-16T00-09-00.236Z",
	}
	for _, rawID := range hostile {
		matches := FindCodebuffFreebuffMatches(pairs, rawID)
		assert.Empty(t, matches,
			"hostile rawID %q must return no matches", rawID)
	}
}

// TestFindCodebuffFreebuffMatchesEmpty pins the empty-input
// path: empty rawID returns nil without touching the
// filesystem or iterating the configured roots.
func TestFindCodebuffFreebuffMatchesEmpty(t *testing.T) {
	matches := FindCodebuffFreebuffMatches(
		[]CodebuffFamilyRoots{
			{Agent: AgentCodebuff,
				Roots: []string{"/definitely/missing"}},
		},
		"",
	)
	assert.Empty(t, matches)
}
