//go:build unix

package sshremote

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotesync"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const sourceTestHost = "devbox"

const sourceTestDestination = "user@devbox.example.test"

// sourceEnv is a fake remote reached through the fake ssh, whose remote
// agentsview is this test binary.
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
	return sourceEnv{
		ssh:         ssh,
		build:       LocalBuild{Version: "dev", Executable: os.Args[0]},
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

// sessionAt returns the archived session read from the remote file path.
func (e sourceEnv) sessionAt(t *testing.T, path string) db.Session {
	t.Helper()
	page, err := e.database.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(t, err)
	for _, listed := range page.Sessions {
		session, err := e.database.GetSessionFull(t.Context(), listed.ID)
		require.NoError(t, err)
		if session != nil && session.FilePath != nil &&
			*session.FilePath == sourceTestHost+":"+path {
			return *session
		}
	}
	require.FailNow(t, "no archived session for "+path)
	return db.Session{}
}

// writeSession writes a one-message Claude session on the remote.
func writeSession(t *testing.T, path, message string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		testjsonl.NewSessionBuilder().AddClaudeUser("2024-01-01T00:00:00Z", message).String(),
	), 0o644))
}

// mirroredSessionFiles lists the mirror copies of the remote session file.
func (e sourceEnv) mirroredSessionFiles(t *testing.T) []string {
	t.Helper()
	var found []string
	root := remotesync.MirrorDir(e.dataDir, sourceTestHost)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && entry.Name() == filepath.Base(e.sessionPath) {
			found = append(found, path)
		}
		return nil
	})
	require.NoError(t, err)
	return found
}

// installTestBinary places the test binary in the fake remote's cache as
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

// recordingSource records every archive request a sync makes.
type recordingSource struct {
	remotesync.Source
	mu       sync.Mutex
	archives []remotesync.ArchiveRequest
}

func (s *recordingSource) Archive(
	ctx context.Context, request remotesync.ArchiveRequest,
) (*remotesync.ArchiveStream, error) {
	s.mu.Lock()
	s.archives = append(s.archives, request)
	s.mu.Unlock()
	return s.Source.Archive(ctx, request)
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

func TestSSHSourceSyncsAppendedSessionAsDelta(t *testing.T) {
	env := newSourceEnv(t)
	installTestBinary(t, env.ssh.Home, env.build)
	// Sync fetches a full archive when at least half the files changed, so
	// two unchanged sessions keep the change below that share.
	projects := filepath.Dir(filepath.Dir(env.sessionPath))
	writeSession(t, filepath.Join(projects, "project-b", "other-1.jsonl"), "unchanged one")
	writeSession(t, filepath.Join(projects, "project-c", "other-2.jsonl"), "unchanged two")
	_, err := env.run(t, env.source())
	require.NoError(t, err)
	before := env.sessionAt(t, env.sessionPath)
	appended := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:01:00Z", "second message over ssh").String()
	f, err := os.OpenFile(env.sessionPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	recorder := &recordingSource{Source: env.source()}
	_, err = env.run(t, recorder)

	require.NoError(t, err)
	require.Len(t, recorder.archives, 1)
	assert.NotNil(t, recorder.archives[0].DeltaFiles, "second sync must request a delta")
	after := env.sessionAt(t, env.sessionPath)
	assert.Equal(t, before.ID, after.ID)
	assert.Greater(t, after.MessageCount, before.MessageCount)
}

func TestSSHSourceRemovesDeletedFileFromMirrorAndKeepsSession(t *testing.T) {
	env := newSourceEnv(t)
	installTestBinary(t, env.ssh.Home, env.build)
	_, err := env.run(t, env.source())
	require.NoError(t, err)
	session := env.onlySession(t)
	require.Len(t, env.mirroredSessionFiles(t), 1)
	require.NoError(t, os.Remove(env.sessionPath))

	_, err = env.run(t, env.source())

	require.NoError(t, err)
	assert.Empty(t, env.mirroredSessionFiles(t))
	kept, err := env.database.GetSession(t.Context(), session.ID)
	require.NoError(t, err)
	assert.NotNil(t, kept, "archived session must survive deletion on the remote")
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

func TestSSHSourceUploadsItselfWhenRemoteHasNoAgentsview(t *testing.T) {
	env := newSourceEnv(t)

	stats, err := env.run(t, env.source())

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	id, err := env.build.ContentID()
	require.NoError(t, err)
	assert.FileExists(t,
		filepath.Join(env.ssh.Home, ".cache", "agentsview", "bin", id, "agentsview"))
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
