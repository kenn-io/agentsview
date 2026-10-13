package codexcloud

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mirrored makes threadID a known thread by creating its mirror file.
func mirrored(t *testing.T, s *Store, threadID string) {
	t.Helper()
	path, err := s.MirrorPath(passKey, threadID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
}

// link links child to parent and returns its root and the threads newly
// counted under a root, in the order they were counted.
func link(t *testing.T, s *Store, child, parent string) (string, []string) {
	t.Helper()
	mirrored(t, s, child)
	var moved []string
	root, err := s.LinkParent(passKey, child, parent, func(id, _ string) error {
		moved = append(moved, id)
		return nil
	})
	require.NoError(t, err)
	return root, moved
}

func descendants(t *testing.T, s *Store, root string) []string {
	t.Helper()
	ids, err := s.LoadDescendants(passKey, root)
	require.NoError(t, err)
	return ids
}

func TestNestedDescendantsGroupedUnderRoot(t *testing.T) {
	s := NewStore(t.TempDir())
	root, moved := link(t, s, "p", "")
	assert.Equal(t, "p", root)
	assert.Empty(t, moved)

	root, moved = link(t, s, "c", "p")
	assert.Equal(t, "p", root)
	assert.Equal(t, []string{"c"}, moved)
	root, moved = link(t, s, "gc", "c")
	assert.Equal(t, "p", root, "a grandchild counts toward the top root")
	assert.Equal(t, []string{"gc"}, moved)

	assert.Equal(t, []string{"c", "gc"}, descendants(t, s, "p"))
	assert.Empty(t, descendants(t, s, "c"), "only roots have a group")

	_, moved = link(t, s, "gc", "c")
	assert.Empty(t, moved, "linking again moves nothing")
	assert.Equal(t, []string{"c", "gc"}, descendants(t, s, "p"), "no duplicate IDs")

	group, err := s.UsageGroupFor(passKey, "p")
	require.NoError(t, err)
	assert.Equal(t, []string{"c", "gc"}, group.Descendants)
}

func TestLateParentMovesChildAndGrandchildren(t *testing.T) {
	s := NewStore(t.TempDir())
	root, _ := link(t, s, "c", "p")
	assert.Equal(t, "c", root, "a child whose parent is not mirrored is its own root")
	root, moved := link(t, s, "gc", "c")
	assert.Equal(t, "c", root)
	assert.Equal(t, []string{"gc"}, moved)
	waiting, err := s.LoadWaiting(passKey, "p")
	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, waiting)

	root, moved = link(t, s, "p", "")

	assert.Equal(t, "p", root)
	assert.Equal(t, []string{"c", "gc"}, moved, "the late parent takes the child and its descendants")
	assert.Equal(t, []string{"c", "gc"}, descendants(t, s, "p"))
	assert.Empty(t, descendants(t, s, "c"))
	waiting, err = s.LoadWaiting(passKey, "p")
	require.NoError(t, err)
	assert.Empty(t, waiting)
	for _, id := range []string{"c", "gc"} {
		got, err := s.RootOf(passKey, id)
		require.NoError(t, err)
		assert.Equal(t, "p", got)
	}
}

func TestDelegatedChildWaitsForUnlistedParent(t *testing.T) {
	s := NewStore(t.TempDir())
	// A dot task names the dot's own conversation, which is never listed.
	root, moved := link(t, s, "dot-child", "dot-conversation")

	assert.Equal(t, "dot-child", root)
	assert.Empty(t, moved)
	waiting, err := s.LoadWaiting(passKey, "dot-conversation")
	require.NoError(t, err)
	assert.Equal(t, []string{"dot-child"}, waiting)
	group, err := s.UsageGroupFor(passKey, "dot-child")
	require.NoError(t, err)
	assert.Equal(t, "dot-child", group.ThreadID)
	assert.Empty(t, group.Descendants)
}

func TestChangedParentLeavesOldWaitingFile(t *testing.T) {
	s := NewStore(t.TempDir())
	link(t, s, "c", "p")
	link(t, s, "q", "")
	_, moved := link(t, s, "c", "q")
	assert.Equal(t, []string{"c"}, moved)

	_, moved = link(t, s, "p", "")

	assert.Empty(t, moved, "c now belongs to q, not to the parent it once waited for")
	assert.Equal(t, []string{"c"}, descendants(t, s, "q"))
	assert.Empty(t, descendants(t, s, "p"))
}

// TestInterruptedLinkCountsEveryMovedThreadOnRetry covers a link that stops
// while it writes "counted under" records, as a crash would: the retry
// still counts every thread the late parent takes.
func TestInterruptedLinkCountsEveryMovedThreadOnRetry(t *testing.T) {
	s := NewStore(t.TempDir())
	link(t, s, "c", "p")
	link(t, s, "gc", "c")
	mirrored(t, s, "p")
	interrupted := errors.New("interrupted")
	_, err := s.LinkParent(passKey, "p", "", func(string, string) error { return interrupted })
	require.ErrorIs(t, err, interrupted)

	root, moved := link(t, s, "p", "")

	assert.Equal(t, "p", root)
	assert.Equal(t, []string{"c", "gc"}, moved, "every moved thread is counted on the retry")
	assert.Equal(t, []string{"c", "gc"}, descendants(t, s, "p"))
	assert.Empty(t, descendants(t, s, "c"))
}
