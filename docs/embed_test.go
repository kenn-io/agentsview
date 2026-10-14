package docs

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The changelog and contributor guide are published but are not product
// guides, and README.md is the unpublished maintainer guide.
var notEmbedded = map[string]bool{
	"README.md": true, "changelog.md": true, "contributing.md": true,
}

func TestPublicEmbedsEveryTopLevelGuide(t *testing.T) {
	onDisk, err := filepath.Glob("*.md")
	require.NoError(t, err)
	var want []string
	for _, name := range onDisk {
		if !notEmbedded[name] {
			want = append(want, name)
		}
	}
	embedded, err := fs.Glob(Public, "*.md")
	require.NoError(t, err)
	assert.ElementsMatch(t, want, embedded,
		"add new public guides to the go:embed list in embed.go")
}
