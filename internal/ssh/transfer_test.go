package ssh

import (
	"archive/tar"
	"bytes"
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
