//go:build fts5 && linux

package db

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestUsageCacheUpgradeCopyFailure(t *testing.T) {
	if archive := os.Getenv("USAGE_UPGRADE_LIMIT_ARCHIVE"); archive != "" {
		signal.Ignore(syscall.SIGXFSZ)
		require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1 << 20, Max: 1 << 20}))
		manager := newUsageCacheManager(archive)
		defer manager.Close()
		id := os.Getenv("USAGE_UPGRADE_LIMIT_ID")
		cache, err := manager.Generation(t.Context(), id)
		require.NoError(t, err)
		assert.True(t, cache.temporary)
		assert.FileExists(t, usageCacheGenerationPath(archive, 23, id))
		assert.NoFileExists(t, usageCacheGenerationPath(archive, 24, id))
		return
	}
	archive, old, id := copyUsageV23Fixture(t)
	cache := openRawUsageCacheTestDB(t, old)
	_, err := cache.ExecContext(t.Context(), `CREATE TABLE padding (data BLOB); INSERT INTO padding VALUES (zeroblob(4194304))`)
	require.NoError(t, err)
	require.NoError(t, cache.Close())
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUsageCacheUpgradeCopyFailure$")
	cmd.Env = append(os.Environ(), "USAGE_UPGRADE_LIMIT_ARCHIVE="+archive, "USAGE_UPGRADE_LIMIT_ID="+id)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.FileExists(t, old)
	assert.NoFileExists(t, usageCacheGenerationPath(archive, 24, id))
	manager := newUsageCacheManager(archive)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	upgraded, err := manager.Generation(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, upgraded.temporary)
	assert.NoFileExists(t, old)
}

func TestUsageCacheUpgradeCandidateModeDuringCopy(t *testing.T) {
	if archive := os.Getenv("USAGE_UPGRADE_MODE_ARCHIVE"); archive != "" {
		syscall.Umask(0o022)
		fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
		require.NoError(t, err)
		defer unix.Close(fd)
		_, err = unix.InotifyAddWatch(fd, filepath.Dir(archive), unix.IN_MODIFY)
		require.NoError(t, err)
		manager := newUsageCacheManager(archive)
		t.Cleanup(func() { require.NoError(t, manager.Close()) })
		done := make(chan error, 1)
		go func() {
			_, err := manager.Generation(t.Context(), os.Getenv("USAGE_UPGRADE_MODE_ID"))
			done <- err
		}()
		buffer := make([]byte, 4096)
		var observed bool
		for !observed {
			ready, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 10000)
			require.NoError(t, err)
			require.NotZero(t, ready, "timed out waiting for candidate writes")
			n, err := unix.Read(fd, buffer)
			if errors.Is(err, unix.EAGAIN) {
				continue
			}
			require.NoError(t, err)
			for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
				length := int(binary.NativeEndian.Uint32(buffer[offset+12:]))
				name := strings.TrimRight(string(buffer[offset+unix.SizeofInotifyEvent:offset+unix.SizeofInotifyEvent+length]), "\x00")
				offset += unix.SizeofInotifyEvent + length
				if !strings.HasPrefix(name, ".usage-cache-init-") || !strings.HasSuffix(name, ".db") {
					continue
				}
				info, err := os.Stat(filepath.Join(filepath.Dir(archive), name))
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				require.NoError(t, err)
				if info.Size() == 0 {
					continue
				}
				select {
				case err := <-done:
					require.NoError(t, err)
					require.FailNow(t, "copy completed before observing a candidate write")
				default:
				}
				assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
				observed = true
				break
			}
		}
		require.NoError(t, <-done)
		return
	}
	archive, old, id := copyUsageV23Fixture(t)
	require.NoError(t, os.Chmod(filepath.Dir(archive), 0o755))
	cache := openRawUsageCacheTestDB(t, old)
	_, err := cache.ExecContext(t.Context(), `CREATE TABLE padding (data BLOB); INSERT INTO padding VALUES (zeroblob(67108864))`)
	require.NoError(t, err)
	require.NoError(t, cache.Close())
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUsageCacheUpgradeCandidateModeDuringCopy$")
	cmd.Env = append(os.Environ(), "USAGE_UPGRADE_MODE_ARCHIVE="+archive, "USAGE_UPGRADE_MODE_ID="+id)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
