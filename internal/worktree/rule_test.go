package worktree

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizePath(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
	}{
		{"drive parent traversal", "C:/../code/app.dev", "C:/code/app.dev"},
		{"drive root", `C:\repo\..\..\`, "C:/"},
		{"drive relative", "C:code/app.dev", "C:code/app.dev"},
		{"UNC parent traversal", "//host/share/../other.dev/app.dev", "//host/share/other.dev/app.dev"},
		{"UNC share root", `\\host\share\repo\..\..\..\`, "//host/share/"},
		{"UNC split separators", "//host//share//repo/../app.dev", "//host/share/app.dev"},
		{"POSIX root", "/../code/app.dev", "/code/app.dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizePath(tc.path))
		})
	}
}
