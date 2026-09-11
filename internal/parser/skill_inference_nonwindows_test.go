//go:build !windows

package parser

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillNameFromFrontmatterRejectsSymlink(t *testing.T) {
	real := writeTestSkill(t, "index", "data-analytics:index")

	// A SKILL.md symlink pointing at the real skill must not have
	// its frontmatter read; resolution falls back to the parent
	// directory name of the symlink path instead.
	linkDir := filepath.Join(t.TempDir(), "evil")
	require.NoError(t, os.MkdirAll(linkDir, 0o755))
	link := filepath.Join(linkDir, "SKILL.md")
	require.NoError(t, os.Symlink(real, link))

	assert.Equal(t, "evil", skillNameFromPath(t.Context(), link, ""))
}

func TestSkillNameFromFrontmatterRejectsNonRegularFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "qa")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	fifo := filepath.Join(dir, "SKILL.md")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	// Must not block on the FIFO open and must not read frontmatter;
	// resolution falls back to the parent directory name.
	assert.Equal(t, "qa", skillNameFromPath(t.Context(), fifo, ""))
}
