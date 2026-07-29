package pathutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "home", path: "~", want: home},
		{name: "child", path: "~/nested/file", want: filepath.Join(home, "nested", "file")},
		{name: "relative", path: "relative/path", want: "relative/path"},
		{name: "mid-string tilde", path: "relative/~marker", want: "relative/~marker"},
		{name: "named user", path: "~someone/file", want: "~someone/file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandHome(tt.path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
