//go:build unix

package sshremote

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/shellquote"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const sourceTestHost = "devbox"

const sourceTestDestination = "user@devbox.example.test"

// sourceEnv is a fake remote reached through the fake ssh, whose remote
// agentsview is a shell wrapper that execs this test binary.
type sourceEnv struct {
	ssh         fakeSSH
	build       LocalBuild
	sessionPath string
	database    *db.DB
	dataDir     string
}

func newSourceEnv(t *testing.T) sourceEnv {
	t.Helper()
	ssh := newFakeSSH(t)
	// The real PATH may hold an installed agentsview, which must not answer
	// the probe in place of the test binary.
	t.Setenv("FAKE_SSH_PATH", remoteToolDir(t))
	sessionPath := filepath.Join(ssh.Home, ".claude", "projects", "project-a", "session.jsonl")
	writeSession(t, sessionPath, "over ssh")
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	executable := writeExecutable(t, t.TempDir(), "agentsview",
		"#!/bin/sh\nexec "+shellquote.POSIX(os.Args[0])+" \"$@\"\n")
	return sourceEnv{
		ssh:         ssh,
		build:       LocalBuild{Version: "dev", Executable: executable},
		sessionPath: sessionPath,
		database:    database,
		dataDir:     t.TempDir(),
	}
}

func (e sourceEnv) source() *Source {
	return NewSource(Provisioner{
		Runner:  Runner{Destination: sourceTestDestination, SSHPath: e.ssh.Path},
		Build:   e.build,
		DataDir: e.dataDir,
		Host:    sourceTestHost,
	})
}

func (e sourceEnv) run(t *testing.T, source remotesync.Source) (remotesync.SyncStats, error) {
	t.Helper()
	return remotesync.RemoteSync{
		Host:    sourceTestHost,
		DataDir: e.dataDir,
		DB:      e.database,
		Source:  source,
	}.Run(t.Context())
}

// onlySession returns the one archived session with its source columns.
func (e sourceEnv) onlySession(t *testing.T) db.Session {
	t.Helper()
	page, err := e.database.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	session, err := e.database.GetSessionFull(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.NotNil(t, session)
	return *session
}

// writeSession writes a one-message Claude session on the remote.
func writeSession(t *testing.T, path, message string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		testjsonl.NewSessionBuilder().AddClaudeUser("2024-01-01T00:00:00Z", message).String(),
	), 0o644))
}

// installTestBinary places the shell wrapper in the fake remote's cache as
// build, so Ensure finds it without an upload.
func installTestBinary(t *testing.T, home string, build LocalBuild) {
	t.Helper()
	id, err := build.ContentID()
	require.NoError(t, err)
	dir := filepath.Join(home, ".cache", "agentsview", "bin", id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	dst := filepath.Join(dir, "agentsview")
	if os.Link(build.Executable, dst) == nil {
		return
	}
	data, err := os.ReadFile(build.Executable)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, data, 0o755))
}

func uniqueLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var lines []string
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSuffix(line, "\n")
		if !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestSSHSourceSyncsRemoteHome(t *testing.T) {
	env := newSourceEnv(t)
	installTestBinary(t, env.ssh.Home, env.build)

	stats, err := env.run(t, env.source())

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	session := env.onlySession(t)
	assert.True(t, strings.HasPrefix(session.ID, sourceTestHost+"~"), session.ID)
	assert.Equal(t, sourceTestHost, session.Machine)
	require.NotNil(t, session.FilePath)
	assert.Equal(t, sourceTestHost+":"+env.sessionPath, *session.FilePath)
	assert.Equal(t, []string{sourceTestDestination}, uniqueLines(t, env.ssh.Log))
}

// Remote sync reads only local archive settings, so an invalid section the
// remote host's server would reject must not stop collection from it.
func TestSSHSourceSyncsRemoteWithInvalidUnusedConfig(t *testing.T) {
	env := newSourceEnv(t)
	installTestBinary(t, env.ssh.Home, env.build)
	configDir := filepath.Join(env.ssh.Home, ".agentsview")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.toml"),
		[]byte("[insights]\nendpoint = 'http://models.example/v1'\nmodel = 'remote'\n"), 0o600))

	stats, err := env.run(t, env.source())

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
}

func TestSSHSourceReportsNoisyShellStartUp(t *testing.T) {
	env := newSourceEnv(t)
	installTestBinary(t, env.ssh.Home, env.build)
	noisy := strings.Replace(fakeSSHScript, `/bin/sh -c "$*"`, `/bin/sh -c "echo banner; $*"`, 1)
	require.NotEqual(t, fakeSSHScript, noisy)
	env.ssh.Path = writeExecutable(t, t.TempDir(), "ssh", noisy)

	_, err := env.run(t, env.source())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell start-up")
	assert.Contains(t, remotesync.FailureSummary(err), "shell start-up")
	assert.False(t, remotesync.IsHostUnavailable(err))
}

func TestSSHSourceProvisionsAgainWhenItsBinaryVanishes(t *testing.T) {
	env := newSourceEnv(t)
	installTestBinary(t, env.ssh.Home, env.build)
	id, err := env.build.ContentID()
	require.NoError(t, err)
	source := env.source()
	bin, err := source.ensure(t.Context())
	require.NoError(t, err)
	require.Equal(t, cachedBinary(id), bin)
	// Another collector's retention removes the binary right after
	// this probe selected it.
	cached := filepath.Join(env.ssh.Home, ".cache", "agentsview", "bin", id)
	require.NoError(t, os.RemoveAll(cached))

	stats, err := env.run(t, source)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	assert.FileExists(t, filepath.Join(cached, "agentsview"), "uploaded again")
}

func TestSSHSourceReportsABinaryItCannotRun(t *testing.T) {
	env := newSourceEnv(t)
	installTestBinary(t, env.ssh.Home, env.build)
	source := env.source()
	_, err := source.ensure(t.Context())
	require.NoError(t, err)
	id, err := env.build.ContentID()
	require.NoError(t, err)
	// A binary that is present but cannot run is not missing:
	// uploading it again would not help, so the failure is reported.
	binary := filepath.Join(env.ssh.Home, ".cache", "agentsview", "bin", id, "agentsview")
	require.NoError(t, os.Remove(binary))
	require.NoError(t, os.WriteFile(binary, []byte("not a program"), 0o644))

	_, err = env.run(t, source)

	require.Error(t, err)
	assert.False(t, binaryMissing(err), "%v", err)
	got, readErr := os.ReadFile(binary)
	require.NoError(t, readErr)
	assert.Equal(t, "not a program", string(got), "nothing uploaded")
}

// vanishBeforeArchive removes a cached binary just before the archive step,
// after targets and manifest ran with it, as another collector's retention
// could.
type vanishBeforeArchive struct {
	*Source
	cached string
}

func (s vanishBeforeArchive) Archive(
	ctx context.Context, request remotesync.ArchiveRequest,
) (*remotesync.ArchiveStream, error) {
	if err := os.RemoveAll(s.cached); err != nil {
		return nil, err
	}
	return s.Source.Archive(ctx, request)
}

func TestSSHSourceProvisionsAgainWhenItsBinaryVanishesBeforeTheArchive(t *testing.T) {
	env := newSourceEnv(t)
	installTestBinary(t, env.ssh.Home, env.build)
	id, err := env.build.ContentID()
	require.NoError(t, err)
	cached := filepath.Join(env.ssh.Home, ".cache", "agentsview", "bin", id)

	stats, err := env.run(t, vanishBeforeArchive{Source: env.source(), cached: cached})

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	assert.FileExists(t, filepath.Join(cached, "agentsview"), "uploaded again")
}

func TestSSHSourceReportsItsUploadThroughTheSyncProgress(t *testing.T) {
	env := newSourceEnv(t)
	var details []string

	_, err := remotesync.RemoteSync{
		Host:    sourceTestHost,
		DataDir: env.dataDir,
		DB:      env.database,
		Source:  env.source(),
		Progress: func(p syncpkg.Progress) {
			details = append(details, p.Detail)
		},
	}.Run(t.Context())

	require.NoError(t, err)
	assert.Contains(t, details, "Installing agentsview on "+sourceTestHost)
	id, err := env.build.ContentID()
	require.NoError(t, err)
	assert.FileExists(t,
		filepath.Join(env.ssh.Home, ".cache", "agentsview", "bin", id, "agentsview"))
}

func TestSSHSourceFullSyncLeavesRemoteHomeUnchanged(t *testing.T) {
	env := newSourceEnv(t)
	installTestBinary(t, env.ssh.Home, env.build)
	binDir := filepath.Join(".cache", "agentsview", "bin")
	withoutBinaries := func(state map[string]string) map[string]string {
		for path := range state {
			if path == binDir || strings.HasPrefix(path, binDir+string(filepath.Separator)) {
				delete(state, path)
			}
		}
		return state
	}
	before := withoutBinaries(treeState(t, env.ssh.Home))

	_, err := env.run(t, env.source())

	require.NoError(t, err)
	assert.Equal(t, before, withoutBinaries(treeState(t, env.ssh.Home)))
}

func TestZstdCheckedPassesStreamsThrough(t *testing.T) {
	var encoded bytes.Buffer
	zw, err := remotesync.NewZstdWriter(&encoded, serveZstdLevel)
	require.NoError(t, err)
	_, err = zw.Write([]byte(`{"ok":true}`))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	tests := []struct {
		name    string
		input   []byte
		want    []byte
		wantErr error
	}{
		{"zstd frame", encoded.Bytes(), encoded.Bytes(), nil},
		{"empty", nil, []byte{}, nil},
		{"shorter than the magic", []byte{0x28, 0xb5}, []byte{0x28, 0xb5}, nil},
		{"shell output", []byte("banner\n" + encoded.String()), []byte{}, errShellOutput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checked := newZstdChecked(io.NopCloser(bytes.NewReader(tt.input)))

			got, err := io.ReadAll(checked)

			require.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, tt.want, got)
			require.NoError(t, checked.Close())
		})
	}
}
