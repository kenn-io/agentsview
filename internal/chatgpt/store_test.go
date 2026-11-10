package chatgpt

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialPermissionsAndLogout(t *testing.T) {
	c := New(t.TempDir())
	require.NoError(t, c.withLock(t.Context(), func() error { return c.saveCredential(t.Context(), credential{Reconnect: true}) }))
	before, err := os.Stat(filepath.Join(c.directory(), ".lock"))
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		for name, mode := range map[string]os.FileMode{"": 0o700, "credentials.json": 0o600, ".lock": 0o600} {
			info, err := os.Stat(filepath.Join(c.directory(), name))
			require.NoError(t, err)
			assert.Equal(t, mode, info.Mode().Perm())
		}
		require.NoError(t, os.Chmod(filepath.Join(c.directory(), "credentials.json"), 0o644))
		_, err := c.Status(t.Context())
		require.ErrorContains(t, err, "private")
		require.NoError(t, os.Chmod(filepath.Join(c.directory(), "credentials.json"), 0o600))
	}
	require.NoError(t, c.Logout(t.Context()))
	after, err := os.Stat(filepath.Join(c.directory(), ".lock"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after))
	_, err = os.Stat(filepath.Join(c.directory(), "credentials.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err := os.ReadDir(c.directory())
	require.NoError(t, err)
	assert.Len(t, entries, 2) // stable lock and revision only
}

func TestCredentialSymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges")
	}
	c := New(t.TempDir())
	require.NoError(t, c.secureDirectory(t.Context(), true))
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte(`{"reconnect_required":true}`), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(c.directory(), "credentials.json")))
	_, err := c.Status(t.Context())
	require.ErrorContains(t, err, "regular")
}

func TestStoreCrossProcessLock(t *testing.T) {
	const helper = "AGENTSVIEW_CHATGPT_TEST_LOCK_DIR"
	if dir := os.Getenv(helper); dir != "" {
		c := New(dir)
		err := c.withLock(t.Context(), func() error {
			_, err := io.WriteString(os.Stdout, "locked\n")
			if err != nil {
				return err
			}
			_, err = io.Copy(io.Discard, os.Stdin)
			return err
		})
		require.NoError(t, err)
		return
	}
	c := New(t.TempDir())
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStoreCrossProcessLock$")
	// Only the explicit fixture directory enters the helper; it has no account
	// configuration overrides or application data source defaults.
	command.Env = []string{helper + "=" + c.dataDir, "PATH=" + os.Getenv("PATH")}
	input, err := command.StdinPipe()
	require.NoError(t, err)
	output, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() { input.Close(); _ = command.Process.Kill(); _ = command.Wait() })
	reader := bufio.NewReader(output)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "locked\n", line)
	require.NoError(t, c.saveCredential(t.Context(), credential{Reconnect: true}))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.ErrorIs(t, c.Logout(ctx), context.DeadlineExceeded)
	require.NoError(t, input.Close())
	_, err = io.Copy(io.Discard, reader)
	require.NoError(t, err)
	require.NoError(t, command.Wait())
	require.NoError(t, c.Logout(t.Context()))
}

func TestOversizedCredentialPreservesPreviousRegression(t *testing.T) {
	c := New(t.TempDir())
	original := credential{Reconnect: true}
	require.NoError(t, c.saveCredential(t.Context(), original))
	account := strings.Repeat("x", 120000)
	cr, err := credentialFromResponse(authResponse{AccessToken: token(t, account, time.Now().Add(time.Hour)), RefreshToken: "refresh"}, "")
	require.NoError(t, err)
	require.Error(t, c.saveCredential(t.Context(), cr))
	got, err := c.loadCredential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestStatusWaitsForStoreLockRegression(t *testing.T) {
	c := New(t.TempDir())
	require.NoError(t, c.saveCredential(t.Context(), credential{Reconnect: true}))
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- c.withLock(t.Context(), func() error {
			f, err := os.Open(filepath.Join(c.directory(), "credentials.json"))
			if err != nil {
				return err
			}
			defer f.Close()
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := c.Status(ctx)
	close(release)
	require.NoError(t, <-done)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, c.Logout(t.Context()))
}
