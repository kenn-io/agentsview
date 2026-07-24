package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

func TestBuildTarCommand(t *testing.T) {
	dirs := map[parser.AgentType][]string{
		parser.AgentClaude: {"/home/wes/.claude/projects"},
		parser.AgentCodex:  {"/home/wes/.codex/sessions"},
	}
	cmd := buildTarCommand(dirs, nil, []string{"/home/wes/.codex/session_index.jsonl"})

	assert.Contains(t, cmd, "| tar cf - -C / -T -", "bad tar pipe: %s", cmd)
	assert.NotContains(t, tarCommandLine(t, cmd), "home/wes/.claude/projects",
		"tar invocation must read paths from stdin, not argv")
	// Paths are shell-quoted in the streamed path list and prefixed with
	// ./ so tar cannot treat option-shaped file-list entries as options.
	assert.Contains(t, cmd, "'./home/wes/.claude/projects'")
	assert.Contains(t, cmd, "'./home/wes/.codex/sessions'")
	// Extra files are included in the path list, with no leading slash.
	assert.Contains(t, cmd, "'./home/wes/.codex/session_index.jsonl'")
	// No leading slash in path args.
	assert.NotContains(t, cmd, "'/home/", "path has leading slash: %s", cmd)
}

func TestBuildTarCommandSkipsFileScopedWindsurfDirs(t *testing.T) {
	dirs := map[parser.AgentType][]string{
		parser.AgentWindsurf: {"/home/wes/Windsurf/User"},
	}
	files := map[parser.AgentType][]string{
		parser.AgentWindsurf: {
			"/home/wes/Windsurf/User/workspaceStorage/a/state.vscdb",
			"/home/wes/Windsurf/User/workspaceStorage/a/workspace.json",
		},
	}

	cmd := buildTarCommand(dirs, files, nil)

	assert.Contains(t, cmd, "'./home/wes/Windsurf/User/workspaceStorage/a/state.vscdb'")
	assert.Contains(t, cmd, "'./home/wes/Windsurf/User/workspaceStorage/a/workspace.json'")
	assert.NotContains(t, cmd, "'./home/wes/Windsurf/User'",
		"file-scoped Windsurf root must not be archived recursively: %s", cmd)
}

func TestTarListPathProtectsOptionShapedPath(t *testing.T) {
	assert.Equal(t, "./-dash/session.jsonl", tarListPath("/-dash/session.jsonl"))
	assert.Equal(t, "./home/wes/file.jsonl", tarListPath("/home/wes/file.jsonl"))
	assert.Equal(t, "./already/relative.jsonl", tarListPath("./already/relative.jsonl"))
	assert.Empty(t, tarListPath("/"))
	assert.Empty(t, tarListPath("/home/wes/bad\npath.jsonl"))
	assert.Empty(t, tarListPath("/home/wes/bad\rpath.jsonl"))
	assert.Empty(t, tarListPath("/home/wes/bad\x00path.jsonl"))
}

func TestBuildTarCommandStreamsPathListToTar(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote tar script uses POSIX paths; local Windows paths are not representative")
	}

	root := t.TempDir()
	claudeDir := filepath.Join(root, "home", "wes", ".claude", "projects")
	claudeFile := filepath.Join(claudeDir, "session.jsonl")
	windsurfDir := filepath.Join(root, "home", "wes", "Windsurf", "User", "workspaceStorage", "a")
	stateDB := filepath.Join(windsurfDir, parser.WindsurfStateDBName)
	workspaceJSON := filepath.Join(windsurfDir, "workspace.json")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.MkdirAll(windsurfDir, 0o755))
	require.NoError(t, os.WriteFile(claudeFile, []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(stateDB, []byte("state"), 0o644))
	require.NoError(t, os.WriteFile(workspaceJSON, []byte("{}\n"), 0o644))

	script := buildTarCommand(
		map[parser.AgentType][]string{
			parser.AgentClaude:   {claudeDir},
			parser.AgentWindsurf: {filepath.Dir(filepath.Dir(windsurfDir))},
		},
		map[parser.AgentType][]string{
			parser.AgentWindsurf: {stateDB, workspaceJSON},
		},
		nil,
	)
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	archive, err := cmd.Output()
	require.NoError(t, err)
	names := tarNames(t, archive)
	assert.Contains(t, names, archivePathForTest(claudeFile))
	assert.Contains(t, names, archivePathForTest(stateDB))
	assert.Contains(t, names, archivePathForTest(workspaceJSON))
}

func TestBuildTarCommandSkipsMissingFileScopedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote tar script uses POSIX paths; local Windows paths are not representative")
	}

	root := t.TempDir()
	windsurfDir := filepath.Join(root, "home", "wes", "Windsurf", "User", "workspaceStorage", "a")
	stateDB := filepath.Join(windsurfDir, parser.WindsurfStateDBName)
	missingWAL := stateDB + "-wal"
	require.NoError(t, os.MkdirAll(windsurfDir, 0o755))
	require.NoError(t, os.WriteFile(stateDB, []byte("state"), 0o644))

	script := buildTarCommand(
		map[parser.AgentType][]string{
			parser.AgentWindsurf: {filepath.Dir(filepath.Dir(windsurfDir))},
		},
		map[parser.AgentType][]string{
			parser.AgentWindsurf: {stateDB, missingWAL},
		},
		nil,
	)
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	archive, err := cmd.Output()
	require.NoError(t, err)
	names := tarNames(t, archive)
	assert.Contains(t, names, archivePathForTest(stateDB))
	assert.NotContains(t, names, archivePathForTest(missingWAL))
}

func TestBuildTarCommandSnapshotsHermesStateDBWithoutSidecars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote snapshot script uses POSIX paths; local Windows paths are not representative")
	}
	root := t.TempDir()
	stateDB := filepath.Join(root, "profile", "state.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(stateDB), 0o755))
	writer, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	_, err = writer.Exec(`
		CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT);
		INSERT INTO sessions (id, title) VALUES ('session', 'Main database');
	`)
	require.NoError(t, err)
	var journalMode string
	require.NoError(t,
		writer.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
	_, err = writer.Exec(`PRAGMA wal_autocheckpoint = 0`)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE sessions SET title = 'Committed in WAL'`)
	require.NoError(t, err)
	wal := stateDB + "-wal"
	shm := stateDB + "-shm"
	require.FileExists(t, wal)

	script := buildTarCommand(
		map[parser.AgentType][]string{parser.AgentHermes: {stateDB}},
		nil,
		[]string{wal, shm, stateDB + "-journal"},
	)
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	archive, err := cmd.CombinedOutput()
	require.NoError(t, err, "snapshot command output: %s", archive)
	names := tarNames(t, archive)
	assert.Equal(t, []string{archivePathForTest(stateDB)}, names)

	extracted := t.TempDir()
	_, err = remotesync.ExtractTarStream(
		context.Background(), bytes.NewReader(archive), extracted,
	)
	require.NoError(t, err)
	extractedDB := filepath.Join(extracted, strings.TrimPrefix(stateDB, "/"))
	snapshot, err := sql.Open("sqlite3", extractedDB)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, snapshot.Close()) })
	var title string
	require.NoError(t,
		snapshot.QueryRow(`SELECT title FROM sessions WHERE id = 'session'`).Scan(&title))
	assert.Equal(t, "Committed in WAL", title)
}

func TestBuildTarCommandExcludesOmnigentAuthenticationDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote archive script uses POSIX paths; local Windows paths are not representative")
	}
	root := t.TempDir()
	chatDB := filepath.Join(root, "chat.db")
	require.NoError(t, os.WriteFile(chatDB, []byte("authentication state"), 0o600))

	script := buildTarCommand(
		map[parser.AgentType][]string{parser.AgentOmnigent: {root}},
		map[parser.AgentType][]string{
			parser.AgentOmnigent: {chatDB},
		},
		nil,
	)
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	archive, err := cmd.CombinedOutput()
	require.NoError(t, err, "archive command output: %s", archive)
	assert.Empty(t, tarNames(t, archive),
		"Omnigent authentication state must never enter an SSH archive")
}

func TestBuildTarCommandRejectsSymlinkedHermesSQLitePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote snapshot script uses POSIX symlinks and paths")
	}
	t.Run("database", func(t *testing.T) {
		root := t.TempDir()
		externalDB := filepath.Join(root, "outside.db")
		writeSSHTestSQLiteDB(t, externalDB)
		stateDB := filepath.Join(root, "profile", "state.db")
		require.NoError(t, os.MkdirAll(filepath.Dir(stateDB), 0o755))
		require.NoError(t, os.Symlink(externalDB, stateDB))

		script := buildTarCommand(
			map[parser.AgentType][]string{parser.AgentHermes: {stateDB}},
			nil, nil,
		)
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(script)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		archive, err := cmd.Output()
		require.NoError(t, err, "snapshot command stderr: %s", stderr.String())
		assert.Empty(t, tarNames(t, archive))
		assert.Contains(t, stderr.String(), stateDB)
	})

	t.Run("sidecar", func(t *testing.T) {
		root := t.TempDir()
		stateDB := filepath.Join(root, "profile", "state.db")
		require.NoError(t, os.MkdirAll(filepath.Dir(stateDB), 0o755))
		writeSSHTestSQLiteDB(t, stateDB)
		external := filepath.Join(root, "outside-wal")
		require.NoError(t, os.WriteFile(external, []byte("outside"), 0o644))
		wal := stateDB + "-wal"
		require.NoError(t, os.Symlink(external, wal))

		script := buildTarCommand(
			map[parser.AgentType][]string{parser.AgentHermes: {stateDB}},
			nil, []string{wal},
		)
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(script)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		archive, err := cmd.Output()
		require.NoError(t, err, "snapshot command stderr: %s", stderr.String())
		assert.Empty(t, tarNames(t, archive))
		assert.Contains(t, stderr.String(), stateDB)
		assert.Contains(t, stderr.String(), wal)
	})
}

func TestBuildTarCommandSkipsFailedHermesSnapshotAndKeepsOtherData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote snapshot script uses POSIX paths; local Windows paths are not representative")
	}
	root := t.TempDir()
	badProfile := filepath.Join(root, "0-bad")
	goodProfile := filepath.Join(root, "1-good")
	badSessions := filepath.Join(badProfile, "sessions")
	goodSessions := filepath.Join(goodProfile, "sessions")
	require.NoError(t, os.MkdirAll(badSessions, 0o755))
	require.NoError(t, os.MkdirAll(goodSessions, 0o755))
	badTranscript := filepath.Join(badSessions, "bad.jsonl")
	goodTranscript := filepath.Join(goodSessions, "good.jsonl")
	require.NoError(t, os.WriteFile(badTranscript, []byte("bad transcript"), 0o644))
	require.NoError(t, os.WriteFile(goodTranscript, []byte("good transcript"), 0o644))
	badStateDB := filepath.Join(badProfile, "state.db")
	goodStateDB := filepath.Join(goodProfile, "state.db")
	require.NoError(t, os.WriteFile(badStateDB, []byte("not sqlite"), 0o644))
	writeSSHTestSQLiteDB(t, goodStateDB)

	script := buildTarCommand(
		map[parser.AgentType][]string{
			parser.AgentHermes: {badSessions, goodSessions},
		},
		nil, []string{badStateDB, goodStateDB},
	)
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	archive, err := cmd.Output()
	require.NoError(t, err, "snapshot command stderr: %s", stderr.String())
	names := tarNames(t, archive)
	assert.Contains(t, names, archivePathForTest(badTranscript))
	assert.Contains(t, names, archivePathForTest(goodTranscript))
	assert.Contains(t, names, archivePathForTest(goodStateDB))
	assert.NotContains(t, names, archivePathForTest(badStateDB))
	assert.Contains(t, stderr.String(), badStateDB)
	assert.NotContains(t, stderr.String(), goodStateDB)
}

func TestBuildTarCommandWithoutPythonKeepsTranscriptsAndOtherAgents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote snapshot script uses POSIX paths; local Windows paths are not representative")
	}
	root := t.TempDir()
	hermesSessions := filepath.Join(root, "hermes", "sessions")
	hermesTranscript := filepath.Join(hermesSessions, "session.jsonl")
	hermesStateDB := filepath.Join(root, "hermes", "state.db")
	claudeDir := filepath.Join(root, "claude", "projects")
	claudeTranscript := filepath.Join(claudeDir, "session.jsonl")
	require.NoError(t, os.MkdirAll(hermesSessions, 0o755))
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.WriteFile(hermesTranscript, []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(claudeTranscript, []byte("{}\n"), 0o644))
	writeSSHTestSQLiteDB(t, hermesStateDB)

	script := buildTarCommand(
		map[parser.AgentType][]string{
			parser.AgentHermes: {hermesSessions},
			parser.AgentClaude: {claudeDir},
		},
		nil, []string{hermesStateDB},
	)
	tarPath, err := exec.LookPath("tar")
	require.NoError(t, err)
	remoteBin := t.TempDir()
	require.NoError(t, os.Symlink(tarPath, filepath.Join(remoteBin, "tar")))

	cmd := exec.Command("sh")
	cmd.Env = []string{"PATH=" + remoteBin}
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	archive, err := cmd.Output()
	require.NoError(t, err, "snapshot command stderr: %s", stderr.String())
	names := tarNames(t, archive)
	assert.Contains(t, names, archivePathForTest(hermesTranscript))
	assert.Contains(t, names, archivePathForTest(claudeTranscript))
	assert.NotContains(t, names, archivePathForTest(hermesStateDB))
	assert.Contains(t, stderr.String(), "Python 3")
	assert.Contains(t, stderr.String(), hermesStateDB)
}

func TestDownloadAndExtractReportsSuccessfulSSHStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake SSH transport uses a POSIX shell")
	}
	var archive bytes.Buffer
	archiveWriter := tar.NewWriter(&archive)
	require.NoError(t, archiveWriter.Close())
	archivePath := filepath.Join(t.TempDir(), "archive.tar")
	require.NoError(t, os.WriteFile(archivePath, archive.Bytes(), 0o600))

	catPath, err := exec.LookPath("cat")
	require.NoError(t, err)
	fakeBin := t.TempDir()
	fakeSSH := filepath.Join(fakeBin, "ssh")
	fakeScript := "#!/bin/sh\n" +
		shellQuote(catPath) + " >/dev/null\n" +
		"printf '%s\\n' 'warning: skipped Hermes state.db snapshot: /remote/state.db' >&2\n" +
		shellQuote(catPath) + " " + shellQuote(archivePath) + "\n"
	require.NoError(t, os.WriteFile(fakeSSH, []byte(fakeScript), 0o700))
	t.Setenv("PATH", fakeBin)

	originalStderr := os.Stderr
	stderrReader, stderrWriter, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = stderrWriter
	extracted, syncErr := downloadAndExtract(
		context.Background(), "remote", "", 0, nil, nil, nil, nil,
	)
	closeErr := stderrWriter.Close()
	os.Stderr = originalStderr
	warnings, readErr := io.ReadAll(stderrReader)
	require.NoError(t, stderrReader.Close())
	if extracted != "" {
		t.Cleanup(func() { require.NoError(t, os.RemoveAll(extracted)) })
	}

	require.NoError(t, syncErr)
	require.NoError(t, closeErr)
	require.NoError(t, readErr)
	assert.Contains(t, string(warnings), "skipped Hermes state.db snapshot: /remote/state.db")
}

func writeSSHTestSQLiteDB(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	require.NoError(t, database.Close())
}

func TestBuildTarCommandSkipsLineDelimitedUnsafePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote tar script uses POSIX paths; local Windows paths are not representative")
	}

	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	safeFile := filepath.Join(dir, "safe.jsonl")
	unsafeFile := filepath.Join(dir, "bad\nname.jsonl")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(safeFile, []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(unsafeFile, []byte("{}\n"), 0o644))

	script := buildTarCommand(nil, nil, []string{safeFile, unsafeFile})
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	archive, err := cmd.Output()
	require.NoError(t, err)
	names := tarNames(t, archive)
	assert.Contains(t, names, archivePathForTest(safeFile))
	assert.NotContains(t, names, archivePathForTest(unsafeFile))
}

func tarCommandLine(t *testing.T, script string) string {
	t.Helper()
	for line := range strings.SplitSeq(script, "\n") {
		if strings.Contains(line, "tar cf") {
			return line
		}
	}
	require.FailNow(t, "tar command line not found", "script: %s", script)
	return ""
}

func tarNames(t *testing.T, archive []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
}

func archivePathForTest(path string) string {
	return "./" + strings.TrimPrefix(filepath.ToSlash(path), "/")
}

func TestRemapPath(t *testing.T) {
	// Use filepath.Join so the local paths are OS-native.
	// remapToRemotePath always returns forward-slash paths.
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := "/home/wes/.claude"
	localPath := filepath.Join(
		"tmp", "sync-123", "home", "wes", ".claude", "foo.jsonl",
	)
	got := remapToRemotePath(tempDir, remoteDir, localPath)
	assert.Equal(t, "/home/wes/.claude/foo.jsonl", got)
}

func TestRemappedDir(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := "/home/wes/.claude"
	got := remappedDir(tempDir, remoteDir)
	want := filepath.Join("tmp", "sync-123", "home", "wes", ".claude")
	assert.Equal(t, want, got)
}
