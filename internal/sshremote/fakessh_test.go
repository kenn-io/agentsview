//go:build unix

package sshremote

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeSSHScript stands in for ssh: it drops the options agentsview passes,
// records the destination, and runs the remote command locally with a clean
// environment whose HOME is FAKE_SSH_HOME.
const fakeSSHScript = `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    -T) shift ;;
    -o) shift 2 ;;
    --) shift; break ;;
    *) break ;;
  esac
done
dest=$1
shift
printf '%s\n' "$dest" >> "$FAKE_SSH_LOG"
exec env -i HOME="$FAKE_SSH_HOME" PATH="$FAKE_SSH_PATH" TMPDIR="$FAKE_SSH_TMPDIR" \
  FAKE_AV_VERSION="$FAKE_AV_VERSION" FAKE_AV_PROTOCOL="$FAKE_AV_PROTOCOL" \
  AGENTSVIEW_SSHREMOTE_TEST_CHILD=1 /bin/sh -c "$*"
`

// fakeSSHFailScript prints FAKE_SSH_STDERR and exits FAKE_SSH_EXIT, like ssh
// failing before the remote command starts.
const fakeSSHFailScript = `#!/bin/sh
printf '%b' "$FAKE_SSH_STDERR" >&2
exit "$FAKE_SSH_EXIT"
`

type fakeSSH struct {
	Path    string // executable to pass as Runner.SSHPath
	Home    string // remote HOME
	TempDir string // remote TMPDIR
	Log     string // one destination per ssh call
}

func writeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

func newFakeSSH(t *testing.T) fakeSSH {
	t.Helper()
	// Discovery reports canonical paths. On macOS t.TempDir is under /var,
	// a symlink to /private/var, so resolve it before tests build
	// expectations from these paths.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	fake := fakeSSH{
		Path:    writeExecutable(t, dir, "ssh", fakeSSHScript),
		Home:    filepath.Join(dir, "home"),
		TempDir: filepath.Join(dir, "tmp"),
		Log:     filepath.Join(dir, "ssh.log"),
	}
	require.NoError(t, os.MkdirAll(fake.Home, 0o755))
	require.NoError(t, os.MkdirAll(fake.TempDir, 0o755))
	t.Setenv("FAKE_SSH_LOG", fake.Log)
	t.Setenv("FAKE_SSH_HOME", fake.Home)
	t.Setenv("FAKE_SSH_PATH", os.Getenv("PATH"))
	t.Setenv("FAKE_SSH_TMPDIR", fake.TempDir)
	return fake
}

func newFailingSSH(t *testing.T, stderr string, exit int) string {
	t.Helper()
	t.Setenv("FAKE_SSH_STDERR", stderr)
	t.Setenv("FAKE_SSH_EXIT", strconv.Itoa(exit))
	return writeExecutable(t, t.TempDir(), "ssh", fakeSSHFailScript)
}
