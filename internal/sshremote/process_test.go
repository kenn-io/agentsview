//go:build unix

package sshremote

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

// processHangGuard bounds a wait that is already blocked on a real event.
const processHangGuard = 60 * time.Second

const snapshotDirPrefix = "agentsview-sqlite-snapshot-"

// hermesFixtureHome returns a HOME whose Hermes data includes a state.db
// large enough that its snapshot is still streaming when a stdout pipe fills.
func hermesFixtureHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	sessionsDir := filepath.Join(home, ".hermes", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionsDir, "session_20240101_000000_fixture.jsonl"),
		[]byte(`{"role":"user","content":"fixture"}`+"\n"), 0o644))

	conn, err := sql.Open("sqlite3", filepath.Join(home, ".hermes", "state.db"))
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE blobs (data BLOB)`)
	require.NoError(t, err)
	for range 40 {
		_, err = conn.ExecContext(t.Context(), `INSERT INTO blobs VALUES (randomblob(1 << 20))`)
		require.NoError(t, err)
	}
	require.NoError(t, conn.Close())
	return home
}

// startServeChild runs the test binary as `agentsview remote-sync serve`
// with only HOME, TMPDIR, PATH, and extraEnv set, and returns its stdout pipe.
func startServeChild(
	t *testing.T, home, tmpdir string, op Op, request serveRequest, extraEnv ...string,
) (*exec.Cmd, io.ReadCloser) {
	t.Helper()
	var stdin bytes.Buffer
	require.NoError(t, json.MarshalWrite(&stdin, request))
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)

	cmd := exec.CommandContext(t.Context(), os.Args[0], "remote-sync", "serve", "--op", string(op))
	cmd.Env = []string{
		testChildEnv + "=1", "HOME=" + home, "TMPDIR=" + tmpdir,
		"PATH=" + os.Getenv("PATH"),
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = &stdin
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	// The test context kills a child that is still running when the test ends.
	t.Cleanup(func() {
		if t.Failed() {
			logged, readErr := os.ReadFile(stderr.Name())
			t.Logf("serve %s stderr: %s (read error: %v)", op, logged, readErr)
		}
		_ = stderr.Close()
	})
	return cmd, stdout
}

// requestFor builds the request for op the way a collector would: manifest
// and archive requests carry the targets the remote reported.
func requestFor(t *testing.T, home string, op Op) serveRequest {
	t.Helper()
	request := serveRequest{ProtocolVersion: remotesync.ProtocolVersion}
	switch op {
	case OpTargets:
	case OpManifest:
		targets := childTargets(t, home)
		request.Targets = &targets
	case OpArchive:
		request.Archive = &remotesync.ArchiveRequest{TargetSet: childTargets(t, home)}
	}
	return request
}

// childTargets runs the targets op for home and checks that the fixture
// resolves its Hermes state.db as a snapshot target.
func childTargets(t *testing.T, home string) remotesync.TargetSet {
	t.Helper()
	cmd, stdout := startServeChild(t, home, t.TempDir(), OpTargets,
		serveRequest{ProtocolVersion: remotesync.ProtocolVersion})
	zr, err := remotesync.NewZstdReader(stdout)
	require.NoError(t, err)
	var targets remotesync.TargetSet
	require.NoError(t, json.UnmarshalRead(zr, &targets))
	zr.Close()
	_, err = io.Copy(io.Discard, stdout)
	require.NoError(t, err)
	require.NoError(t, cmd.Wait())
	require.Contains(t, targets.ProviderExtraFiles[parser.AgentHermes],
		filepath.Join(home, ".hermes", "state.db"),
		"fixture must resolve a Hermes state.db snapshot target")
	return targets
}

// treeState records each path under root with its mode, size, and mtime.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		state[rel] = fmt.Sprintf("%v %d %d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	require.NoError(t, err)
	return state
}

func snapshotDirs(t *testing.T, tmpdir string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(tmpdir, snapshotDirPrefix+"*"))
	require.NoError(t, err)
	return dirs
}

// waitExit waits for cmd and returns its exit error, or nil for status 0.
func waitExit(t *testing.T, cmd *exec.Cmd) *exec.ExitError {
	t.Helper()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err == nil {
			return nil
		}
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		require.True(t, ok, "wait for serve child: %v", err)
		return exitErr
	case <-time.After(processHangGuard):
		require.FailNow(t, fmt.Sprintf("serve child did not exit within %s", processHangGuard))
		return nil
	}
}

// watchSnapshotDirs signals once when a SQLite snapshot directory appears in
// tmpdir. Call it before starting the child so the event cannot be missed.
func watchSnapshotDirs(t *testing.T, tmpdir string) <-chan struct{} {
	t.Helper()
	watcher, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	require.NoError(t, watcher.Add(tmpdir))
	created := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Create) &&
					strings.HasPrefix(filepath.Base(event.Name), snapshotDirPrefix) {
					select {
					case created <- struct{}{}:
					default:
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				assert.NoError(t, err, "watch %s", tmpdir)
			}
		}
	}()
	t.Cleanup(func() {
		assert.NoError(t, watcher.Close())
		<-done
	})
	return created
}

func waitForSnapshotDir(t *testing.T, created <-chan struct{}) {
	t.Helper()
	select {
	case <-created:
	case <-time.After(processHangGuard):
		require.FailNow(t,
			fmt.Sprintf("no SQLite snapshot directory appeared within %s", processHangGuard))
	}
}

func TestServeProcessLeavesNoState(t *testing.T) {
	for _, op := range []Op{OpTargets, OpManifest, OpArchive} {
		t.Run(string(op), func(t *testing.T) {
			home := hermesFixtureHome(t)
			tmpdir := t.TempDir()
			before := treeState(t, home)
			cmd, stdout := startServeChild(t, home, tmpdir, op, requestFor(t, home, op))
			_, err := io.Copy(io.Discard, stdout)
			require.NoError(t, err)
			require.NoError(t, cmd.Wait())

			assert.Equal(t, before, treeState(t, home))
			entries, err := os.ReadDir(tmpdir)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestServeProcessCleansUpWhenReaderCloses(t *testing.T) {
	home := hermesFixtureHome(t)
	tmpdir := t.TempDir()
	created := watchSnapshotDirs(t, tmpdir)
	cmd, stdout := startServeChild(t, home, tmpdir, OpArchive, requestFor(t, home, OpArchive))
	waitForSnapshotDir(t, created)

	require.NoError(t, stdout.Close())
	exitErr := waitExit(t, cmd)

	require.NotNil(t, exitErr)
	assert.Equal(t, ServeExitCode, exitErr.ExitCode())
	assert.Empty(t, snapshotDirs(t, tmpdir))
}

func TestServeProcessCleansUpOnSIGTERMWithStalledReader(t *testing.T) {
	home := hermesFixtureHome(t)
	tmpdir := t.TempDir()
	created := watchSnapshotDirs(t, tmpdir)
	// One P makes the zstd encoder write each block from the serve goroutine,
	// so a full pipe blocks that goroutine in a stdout write, past every
	// context check, instead of blocking only an encoder goroutine.
	cmd, stdout := startServeChild(t, home, tmpdir, OpArchive,
		requestFor(t, home, OpArchive), "GOMAXPROCS=1")
	t.Cleanup(func() { _ = stdout.Close() })
	waitForSnapshotDir(t, created)
	// The encoder emits its first block only once the snapshot body is
	// streaming, and that block does not fit in the pipe. After one byte,
	// stop reading: the child stays blocked in a stdout write while the read
	// end stays open, so only closing its stdout can end it.
	_, err := io.ReadFull(stdout, make([]byte, 1))
	require.NoError(t, err)

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	exitErr := waitExit(t, cmd)

	require.NotNil(t, exitErr)
	assert.Equal(t, ServeExitCode, exitErr.ExitCode())
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	assert.False(t, status.Signaled())
	assert.Empty(t, snapshotDirs(t, tmpdir))
}

func TestServeProcessRestoresBlockingStdout(t *testing.T) {
	tests := []struct {
		name     string
		request  serveRequest
		wantExit int
	}{
		{name: "success", request: serveRequest{ProtocolVersion: remotesync.ProtocolVersion}},
		{name: "failure", request: serveRequest{}, wantExit: ServeExitCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdin bytes.Buffer
			require.NoError(t, json.MarshalWrite(&stdin, tt.request))
			// The child's fd 1 shares this file's open file description, as a
			// terminal or wrapper process would share it.
			stdout, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
			require.NoError(t, err)
			defer stdout.Close()
			cmd := exec.CommandContext(t.Context(), os.Args[0],
				"remote-sync", "serve", "--op", string(OpTargets))
			cmd.Env = []string{
				testChildEnv + "=1", "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(),
				"PATH=" + os.Getenv("PATH"),
			}
			cmd.Stdin = &stdin
			cmd.Stdout = stdout

			err = cmd.Run()

			if tt.wantExit == 0 {
				require.NoError(t, err)
			} else {
				exitErr, ok := errors.AsType[*exec.ExitError](err)
				require.True(t, ok, "serve exited without a status: %v", err)
				assert.Equal(t, tt.wantExit, exitErr.ExitCode())
			}
			flags, err := unix.FcntlInt(stdout.Fd(), unix.F_GETFL, 0)
			require.NoError(t, err)
			assert.Zero(t, flags&unix.O_NONBLOCK, "stdout is left non-blocking")
		})
	}
}
