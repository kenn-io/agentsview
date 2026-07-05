package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/skills"
)

// skillHeaderFormat mirrors the private header format in internal/skills so
// tests here can synthesize stale/modified fixtures without reaching into
// unexported package internals.
const skillHeaderFormat = "# generated-by: agentsview %s hash:%s — do not edit; " +
	"re-run `agentsview skills install`"

// sha256Hex returns the hex sha256 digest of body, matching internal/skills'
// own bodyHash so synthesized headers classify the way production ones do.
func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// claudeSkillPath returns the SKILL.md path the CLI installs for the Claude
// harness under home.
func claudeSkillPath(home string) string {
	return filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName)
}

// agentsSkillPath returns the SKILL.md path the CLI installs for the Agents
// harness under home.
func agentsSkillPath(home string) string {
	return filepath.Join(skills.TargetDir(skills.HarnessAgents, home), skillFileName)
}

func freshClaudeSkill(t *testing.T) skills.Rendered {
	t.Helper()
	rendered, err := skills.Render(skills.HarnessClaude, version)
	require.NoError(t, err)
	return rendered
}

// writeSkillFile writes content at path, creating parent directories.
func writeSkillFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// setTestHome points the process home at dir for both Unix (HOME) and
// Windows (USERPROFILE), since os.UserHomeDir reads a different variable
// per platform.
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// staleClaudeContent returns a well-formed generated-by file whose recorded
// hash matches an older body, so Classify reports StateStale.
func staleClaudeContent() string {
	oldBody := "---\nname: agentsview-finding-history\n---\n" +
		"an earlier revision of the skill body, no longer current\n"
	header := fmt.Sprintf(skillHeaderFormat, "0.0.1", sha256Hex(oldBody))
	return "---\n" + header + "\n" + strings.TrimPrefix(oldBody, "---\n")
}

// modifiedClaudeContent returns a fresh render whose body was hand-edited
// after the header hash was recorded, so Classify reports StateModified.
func modifiedClaudeContent(t *testing.T) string {
	t.Helper()
	return freshClaudeSkill(t).Content + "\nan uninvited local edit\n"
}

const foreignClaudeContent = "# Just a hand-written file\n\nNo generated-by header here.\n"

func TestSkillsInstall_StatesAndForce(t *testing.T) {
	const refusalMsg = "was modified (or not generated); use --force to overwrite"

	tests := []struct {
		name           string
		seed           func(t *testing.T, path string) // nil means the file is missing
		wantMsgNoForce string
		wantMsgForced  string // message once --force overrides a refusal; "" when force changes nothing
	}{
		{
			name:           "missing",
			seed:           nil,
			wantMsgNoForce: "installed",
		},
		{
			name: "current",
			seed: func(t *testing.T, path string) {
				writeSkillFile(t, path, freshClaudeSkill(t).Content)
			},
			wantMsgNoForce: "up to date",
		},
		{
			name: "stale",
			seed: func(t *testing.T, path string) {
				writeSkillFile(t, path, staleClaudeContent())
			},
			wantMsgNoForce: "updated",
		},
		{
			name: "modified",
			seed: func(t *testing.T, path string) {
				writeSkillFile(t, path, modifiedClaudeContent(t))
			},
			wantMsgNoForce: refusalMsg,
			wantMsgForced:  "updated",
		},
		{
			name: "foreign",
			seed: func(t *testing.T, path string) {
				writeSkillFile(t, path, foreignClaudeContent)
			},
			wantMsgNoForce: refusalMsg,
			wantMsgForced:  "updated",
		},
	}

	for _, tt := range tests {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%v", tt.name, force), func(t *testing.T) {
				home := t.TempDir()
				setTestHome(t, home)
				path := claudeSkillPath(home)

				var seedContent string
				if tt.seed != nil {
					tt.seed(t, path)
					seedContent = readFileString(t, path)
				}

				args := []string{"skills", "install", "--harness", "claude"}
				if force {
					args = append(args, "--force")
				}
				out, err := executeCommand(newRootCommand(), args...)

				assert.Contains(t, out, path)

				refused := tt.wantMsgForced != "" && !force
				if refused {
					assert.Contains(t, out, tt.wantMsgNoForce)
					require.Error(t, err, "expected a refusal error")
					assert.Equal(t, seedContent, readFileString(t, path),
						"refused install must not touch the file")
					return
				}

				wantMsg := tt.wantMsgNoForce
				if force && tt.wantMsgForced != "" {
					wantMsg = tt.wantMsgForced
				}
				assert.Contains(t, out, wantMsg)

				require.NoError(t, err, "output: %s", out)
				assert.Equal(t, freshClaudeSkill(t).Content, readFileString(t, path))
			})
		}
	}
}

// readFileString reads path, failing the test on error.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", path)
	return string(b)
}

func TestSkillsInstall_DefaultHarnessesInstallBoth(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	out, err := executeCommand(newRootCommand(), "skills", "install")
	require.NoError(t, err, "output: %s", out)

	assert.Contains(t, out, claudeSkillPath(home))
	assert.Contains(t, out, agentsSkillPath(home))
	assert.FileExists(t, claudeSkillPath(home))
	assert.FileExists(t, agentsSkillPath(home))
}

func TestSkillsInstall_UnknownHarnessErrors(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	_, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown --harness")
}

func TestSkillsInstall_RefusalStillInstallsOtherTargets(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	writeSkillFile(t, claudeSkillPath(home), foreignClaudeContent)

	out, err := executeCommand(newRootCommand(), "skills", "install")
	require.Error(t, err, "one refused target must still fail the command")

	assert.Contains(t, out, "was modified (or not generated); use --force to overwrite")
	assert.Contains(t, out, "installed "+agentsSkillPath(home))
	assert.FileExists(t, agentsSkillPath(home))
	assert.Equal(t, foreignClaudeContent, readFileString(t, claudeSkillPath(home)),
		"the refused claude target must be untouched")
}

func TestSkillsInstall_FilePermissions(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	_, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude")
	require.NoError(t, err)

	info, err := os.Stat(claudeSkillPath(home))
	require.NoError(t, err)
	// The process umask may strip group/other bits from the 0644 requested by
	// os.WriteFile, so only assert the file is a regular, non-executable file
	// readable/writable by its owner rather than the exact resulting mode.
	assert.True(t, info.Mode().IsRegular())
	assert.Zero(t, info.Mode().Perm()&0o111, "installed skill file must not be executable")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm()&0o600,
		"owner must be able to read and write the installed skill file")
}

func TestSkillsList_ReportsEachState(t *testing.T) {
	tests := []struct {
		name string
		seed func(t *testing.T, path string)
		want string
	}{
		{name: "missing", seed: nil, want: "missing"},
		{
			name: "current",
			seed: func(t *testing.T, path string) {
				writeSkillFile(t, path, freshClaudeSkill(t).Content)
			},
			want: "current",
		},
		{
			name: "stale",
			seed: func(t *testing.T, path string) {
				writeSkillFile(t, path, staleClaudeContent())
			},
			want: "stale",
		},
		{
			name: "modified",
			seed: func(t *testing.T, path string) {
				writeSkillFile(t, path, modifiedClaudeContent(t))
			},
			want: "modified",
		},
		{
			name: "foreign",
			seed: func(t *testing.T, path string) {
				writeSkillFile(t, path, foreignClaudeContent)
			},
			want: "foreign",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			setTestHome(t, home)
			path := claudeSkillPath(home)
			if tt.seed != nil {
				tt.seed(t, path)
			}

			out, err := executeCommand(newRootCommand(), "skills", "list", "--format", "json")
			require.NoError(t, err, "output: %s", out)

			var rows []skillListRow
			require.NoError(t, json.Unmarshal([]byte(out), &rows), "output: %s", out)

			var claudeRow *skillListRow
			for i := range rows {
				if rows[i].Harness == string(skills.HarnessClaude) {
					claudeRow = &rows[i]
				}
			}
			require.NotNil(t, claudeRow, "no claude row in %+v", rows)
			assert.Equal(t, tt.want, claudeRow.State)
			assert.Equal(t, "user", claudeRow.Level)
			assert.Equal(t, path, claudeRow.Path)
		})
	}
}

func TestSkillsList_HumanTableHasHeaderAndColumns(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	out, err := executeCommand(newRootCommand(), "skills", "list")
	require.NoError(t, err, "output: %s", out)

	assert.Contains(t, out, "HARNESS")
	assert.Contains(t, out, "LEVEL")
	assert.Contains(t, out, "STATE")
	assert.Contains(t, out, "PATH")
	assert.Contains(t, out, "claude")
	assert.Contains(t, out, "agents")
	assert.Contains(t, out, "missing")
	assert.Contains(t, out, claudeSkillPath(home))
}

// initTestGitRepo runs `git init` in a fresh temp directory. No commit is
// required: gitrepo.Root only needs a `.git` directory to resolve a root.
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	resolved, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	return resolved
}

func TestSkillsInstall_ProjectFlagUsesGitRoot(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	repo := initTestGitRepo(t)
	nested := filepath.Join(repo, "a", "b")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	t.Chdir(nested)

	out, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude", "--project")
	require.NoError(t, err, "output: %s", out)

	wantPath := claudeSkillPath(repo)
	assert.Contains(t, out, wantPath)
	assert.FileExists(t, wantPath)
	// Must not have installed under the user home directory instead.
	assert.NoFileExists(t, claudeSkillPath(home))
}

func TestSkillsList_ProjectFlagReportsProjectLevel(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	repo := initTestGitRepo(t)
	t.Chdir(repo)

	out, err := executeCommand(newRootCommand(), "skills", "list", "--project", "--format", "json")
	require.NoError(t, err, "output: %s", out)

	var rows []skillListRow
	require.NoError(t, json.Unmarshal([]byte(out), &rows), "output: %s", out)
	require.NotEmpty(t, rows)
	for _, r := range rows {
		assert.Equal(t, "project", r.Level)
		assert.True(t, strings.HasPrefix(r.Path, repo), "path %q must be under repo root %q", r.Path, repo)
	}
}

func TestSkillsInstall_ProjectFlagOutsideRepoFallsBackToCWD(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	outsideRepo := t.TempDir()
	resolvedOutside, err := filepath.EvalSymlinks(outsideRepo)
	require.NoError(t, err)
	t.Chdir(resolvedOutside)

	out, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude", "--project")
	require.NoError(t, err, "output: %s", out)

	wantPath := claudeSkillPath(resolvedOutside)
	assert.Contains(t, out, wantPath)
	assert.FileExists(t, wantPath)
}
