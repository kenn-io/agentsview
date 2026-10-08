package pathutil

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGitPointerPath(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	assert.Equal(t, filepath.Join(base, "metadata"), GitPointerPath("metadata", base, cwd))
	assert.Equal(t, cwd, GitPointerPath(cwd, base, base))
	assert.Empty(t, GitPointerPath("", base, cwd))
	if runtime.GOOS != "windows" {
		return
	}
	for _, tc := range []struct{ pointer, cwd, want string }{
		{`\metadata`, `D:\checkout`, `D:\metadata`},
		{`/metadata`, `D:\checkout`, `D:\metadata`},
		{`E:\metadata`, `D:\checkout`, `E:\metadata`},
		{`..\common`, `D:\checkout`, `C:\meta\common`},
		{`C:metadata`, `D:\checkout`, ""},
		{`\\server\share\metadata`, `D:\checkout`, `\\server\share\metadata`},
		{`\metadata`, `\\server\share\checkout`, ""},
	} {
		t.Run(tc.pointer+tc.cwd, func(t *testing.T) {
			assert.Equal(t, tc.want, GitPointerPath(tc.pointer, `C:\meta\worktree`, tc.cwd))
		})
	}
}
