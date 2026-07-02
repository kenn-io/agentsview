package sync

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChangedPathWithinRoot(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		name string
		path string
		root string
		want bool
	}{
		{"exact root", "/home/u/.claude/projects", "/home/u/.claude/projects", true},
		{"nested file", "/home/u/.claude/projects/p/s.jsonl", "/home/u/.claude/projects", true},
		{"deeply nested", "/home/u/.claude/projects/p/sub/agents/a.jsonl", "/home/u/.claude/projects", true},
		{"sibling sharing prefix", "/home/u/.claude-backup/s.jsonl", "/home/u/.claude", false},
		{"outside root", "/home/u/.cortex/sessions/s.jsonl", "/home/u/.claude/projects", false},
		{"root with trailing separator", "/home/u/.gemini/tmp/s.json", "/home/u/.gemini/tmp" + sep, true},
		{"dot segments resolving inside", "/home/u/.claude/x/../projects/s.jsonl", "/home/u/.claude/projects", true},
		{"dot segments resolving outside", "/home/u/.claude/projects/../../other/s.jsonl", "/home/u/.claude/projects", false},
		{"archive member form", "/home/u/ws/state.vscdb#session-1", "/home/u/ws", true},
		{"root itself archive-suffixed", "/home/u/ws#member", "/home/u/ws", true},
		{"empty root", "/home/u/x", "", false},
		{"dot root", "/home/u/x", ".", false},
		{"filesystem root", "/home/u/x", sep, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.FromSlash(strings.ReplaceAll(tc.path, "/", sep))
			root := tc.root
			if root != "" && root != "." && root != sep {
				root = filepath.FromSlash(strings.ReplaceAll(root, "/", sep))
			}
			assert.Equal(t, tc.want, changedPathWithinRoot(path, root))
		})
	}
}

func TestChangedPathWithinAnyRoot(t *testing.T) {
	roots := []string{
		filepath.FromSlash("/home/u/.claude/projects"),
		filepath.FromSlash("/home/u/.config/opencode"),
	}
	assert.True(t, changedPathWithinAnyRoot(
		filepath.FromSlash("/home/u/.config/opencode/storage/s.json"), roots))
	assert.False(t, changedPathWithinAnyRoot(
		filepath.FromSlash("/home/u/.cortex/s.jsonl"), roots))
	assert.False(t, changedPathWithinAnyRoot(
		filepath.FromSlash("/home/u/.cortex/s.jsonl"), nil))
}
