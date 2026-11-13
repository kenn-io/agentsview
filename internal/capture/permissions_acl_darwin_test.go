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

// The ACL flags (chmod +a, ls -e) exist only in the macOS tools. Calling
// them by absolute path keeps GNU coreutils earlier in PATH, as in a Nix
// or Homebrew shell, from running instead.
const (
	macChmod = "/bin/chmod"
	macLs    = "/bin/ls"
)

func addTestDirectoryACL(t *testing.T, dir string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), macChmod, "+a", testDirectoryACL, dir).CombinedOutput()
	require.NoError(t, err, "adding test ACL: %s", out)
}

func TestCreateStateRejectsUntrustedParentACL(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(parent, 0o700))
	addTestDirectoryACL(t, parent)

	_, err := createState(filepath.Join(parent, "capture"), permissionTestManifest(t))

	require.ErrorContains(t, err, "extended ACL")
}

func TestSecureCaptureDirectoryClearsExtendedACL(t *testing.T) {
	dir := t.TempDir()
	addTestDirectoryACL(t, dir)

	require.NoError(t, secureCaptureDirectory(dir))

	listing, err := exec.CommandContext(t.Context(), macLs, "-lde", dir).Output()
	require.NoError(t, err)
	assert.NotContains(t, strings.TrimSpace(string(listing)), "\n 0:")
}
