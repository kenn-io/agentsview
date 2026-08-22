//go:build darwin && cgo

package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDirectoryACL = "everyone allow list,search,add_file,add_subdirectory,delete_child"

func TestCreateStateRejectsUntrustedParentACL(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(parent, 0o700))
	require.NoError(t, exec.Command("chmod", "+a", testDirectoryACL, parent).Run())

	_, err := createState(filepath.Join(parent, "capture"), permissionTestManifest(t))

	require.ErrorContains(t, err, "extended ACL")
}

func TestSecureCaptureDirectoryClearsExtendedACL(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, exec.Command("chmod", "+a", testDirectoryACL, dir).Run())

	require.NoError(t, secureCaptureDirectory(dir))

	listing, err := exec.Command("ls", "-lde", dir).Output()
	require.NoError(t, err)
	assert.NotContains(t, strings.TrimSpace(string(listing)), "\n 0:")
}
