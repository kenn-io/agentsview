package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
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
	rendered, err := skills.Render(skills.HarnessClaude, version, skills.Remote{})
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
				assert := assert.New(t)
				require := require.New(t)

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

				assert.Contains(out, path)

				refused := tt.wantMsgForced != "" && !force
				if refused {
					assert.Contains(out, tt.wantMsgNoForce)
					require.Error(err, "expected a refusal error")
					assert.Equal(seedContent, readFileString(t, path),
						"refused install must not touch the file")
					return
				}

				wantMsg := tt.wantMsgNoForce
				if force && tt.wantMsgForced != "" {
					wantMsg = tt.wantMsgForced
				}
				assert.Contains(out, wantMsg)

				require.NoError(err, "output: %s", out)
				assert.Equal(freshClaudeSkill(t).Content, readFileString(t, path))
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
	assert := assert.New(t)

	home := t.TempDir()
	setTestHome(t, home)

	out, err := executeCommand(newRootCommand(), "skills", "install")
	require.NoError(t, err, "output: %s", out)

	assert.Contains(out, claudeSkillPath(home))
	assert.Contains(out, agentsSkillPath(home))
	assert.FileExists(claudeSkillPath(home))
	assert.FileExists(agentsSkillPath(home))
}

func TestSkillsInstall_UnknownHarnessErrors(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	_, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown --harness")
}

func TestSkillsInstall_RefusalStillInstallsOtherTargets(t *testing.T) {
	assert := assert.New(t)

	home := t.TempDir()
	setTestHome(t, home)
	writeSkillFile(t, claudeSkillPath(home), foreignClaudeContent)

	out, err := executeCommand(newRootCommand(), "skills", "install")
	require.Error(t, err, "one refused target must still fail the command")

	assert.Contains(out, "was modified (or not generated); use --force to overwrite")
	assert.Contains(out, "installed "+agentsSkillPath(home))
	assert.FileExists(agentsSkillPath(home))
	assert.Equal(foreignClaudeContent, readFileString(t, claudeSkillPath(home)),
		"the refused claude target must be untouched")
}

func TestSkillsInstall_FilePermissions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	setTestHome(t, home)

	_, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude")
	require.NoError(err)

	info, err := os.Stat(claudeSkillPath(home))
	require.NoError(err)
	// The process umask may strip group/other bits from the 0644 requested by
	// os.WriteFile, so only assert the file is a regular, non-executable file
	// readable/writable by its owner rather than the exact resulting mode.
	assert.True(info.Mode().IsRegular())
	assert.Zero(info.Mode().Perm()&0o111, "installed skill file must not be executable")
	assert.Equal(os.FileMode(0o600), info.Mode().Perm()&0o600,
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
			assert := assert.New(t)
			require := require.New(t)

			home := t.TempDir()
			setTestHome(t, home)
			path := claudeSkillPath(home)
			if tt.seed != nil {
				tt.seed(t, path)
			}

			out, err := executeCommand(newRootCommand(), "skills", "list", "--format", "json")
			require.NoError(err, "output: %s", out)

			var rows []skillListRow
			require.NoError(json.Unmarshal([]byte(out), &rows), "output: %s", out)

			var claudeRow *skillListRow
			for i := range rows {
				if rows[i].Harness == string(skills.HarnessClaude) {
					claudeRow = &rows[i]
				}
			}
			require.NotNil(claudeRow, "no claude row in %+v", rows)
			assert.Equal(tt.want, claudeRow.State)
			assert.Equal("user", claudeRow.Level)
			assert.Equal(path, claudeRow.Path)
		})
	}
}

func TestSkillsList_HumanTableHasHeaderAndColumns(t *testing.T) {
	assert := assert.New(t)

	home := t.TempDir()
	setTestHome(t, home)

	out, err := executeCommand(newRootCommand(), "skills", "list")
	require.NoError(t, err, "output: %s", out)

	assert.Contains(out, "HARNESS")
	assert.Contains(out, "LEVEL")
	assert.Contains(out, "STATE")
	assert.Contains(out, "PATH")
	assert.Contains(out, "claude")
	assert.Contains(out, "agents")
	assert.Contains(out, "missing")
	assert.Contains(out, claudeSkillPath(home))
}

// initTestGitRepo runs `git init` in a fresh temp directory. No commit is
// required: gitrepo.Root only needs a `.git` directory to resolve a root.
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "init", "-q", "-b", "main")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	resolved, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	return resolved
}

func TestSkillsInstall_ProjectFlagUsesGitRoot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	setTestHome(t, home)

	repo := initTestGitRepo(t)
	nested := filepath.Join(repo, "a", "b")
	require.NoError(os.MkdirAll(nested, 0o755))
	t.Chdir(nested)

	out, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude", "--project")
	require.NoError(err, "output: %s", out)

	wantPath := claudeSkillPath(repo)
	assert.Contains(out, wantPath)
	assert.FileExists(wantPath)
	// Must not have installed under the user home directory instead.
	assert.NoFileExists(claudeSkillPath(home))
}

func TestSkillsList_ProjectFlagReportsProjectLevel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	setTestHome(t, home)

	repo := initTestGitRepo(t)
	t.Chdir(repo)

	out, err := executeCommand(newRootCommand(), "skills", "list", "--project", "--format", "json")
	require.NoError(err, "output: %s", out)

	var rows []skillListRow
	require.NoError(json.Unmarshal([]byte(out), &rows), "output: %s", out)
	require.NotEmpty(rows)
	for _, r := range rows {
		assert.Equal("project", r.Level)
		assert.True(strings.HasPrefix(r.Path, repo), "path %q must be under repo root %q", r.Path, repo)
	}
}

func TestSkillsInstall_BakesServerFlags(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	setTestHome(t, home)

	out, err := executeCommand(newRootCommand(),
		"skills", "install", "--harness", "claude",
		"--server", "https://example.invalid",
		"--server-token-file", "token")
	require.NoError(err, "output: %s", out)

	body := readFileString(t, claudeSkillPath(home))
	assert.Contains(body, "--server https://example.invalid")
	assert.Contains(body, "--server-token-file token")
	assert.Equal(skills.Remote{
		Server: "https://example.invalid", TokenFile: "token",
	}, skills.ParseRemote(body))

	listOut, err := executeCommand(newRootCommand(),
		"skills", "list", "--format", "json")
	require.NoError(err, "output: %s", listOut)
	assert.Contains(listOut, "\"state\":\"current\"")
}

func TestSkillsInstall_ReinstallWithoutFlagsKeepsBakedServer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	setTestHome(t, home)

	_, err := executeCommand(newRootCommand(),
		"skills", "install", "--harness", "claude",
		"--server", "https://example.invalid")
	require.NoError(err)

	out, err := executeCommand(newRootCommand(),
		"skills", "install", "--harness", "claude")
	require.NoError(err, "output: %s", out)
	assert.Contains(out, "up to date")
	assert.Contains(readFileString(t, claudeSkillPath(home)),
		"--server https://example.invalid")
}

func TestSkillsInstall_TokenFileRequiresServer(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	_, err := executeCommand(newRootCommand(),
		"skills", "install", "--harness", "claude",
		"--server-token-file", "token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires --server")
}

func TestSkillsInstall_EnvBakesServer(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("AGENTSVIEW_SKILLS_SERVER", "https://example.invalid")

	_, err := executeCommand(newRootCommand(),
		"skills", "install", "--harness", "claude")
	require.NoError(t, err)
	assert.Contains(t, readFileString(t, claudeSkillPath(home)),
		"--server https://example.invalid")
}

// TestSkillsInstall_ExplicitEmptyServerUnbakes pins the only escape hatch
// back to a local-SQLite skill: an explicitly passed empty --server clears a
// previously baked remote instead of falling through to the file or the
// environment.
func TestSkillsInstall_ExplicitEmptyServerUnbakes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("AGENTSVIEW_SKILLS_SERVER", "https://env.invalid")

	_, err := executeCommand(newRootCommand(),
		"skills", "install", "--harness", "claude",
		"--server", "https://example.invalid")
	require.NoError(err)

	out, err := executeCommand(newRootCommand(),
		"skills", "install", "--harness", "claude", "--server", "")
	require.NoError(err, "output: %s", out)

	body := readFileString(t, claudeSkillPath(home))
	assert.NotContains(body, "--server https://example.invalid")
	assert.NotContains(body, "--server https://env.invalid")
	assert.True(skills.ParseRemote(body).Empty())
}

// TestSkillsList_EnvDoesNotOverrideBakedRemote pins that an exported
// AGENTSVIEW_SKILLS_SERVER cannot make an already-installed file look stale.
// The baked remote is recorded intent; the environment is ambient.
func TestSkillsList_EnvDoesNotOverrideBakedRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	setTestHome(t, home)

	_, err := executeCommand(newRootCommand(),
		"skills", "install", "--harness", "claude")
	require.NoError(err)

	t.Setenv("AGENTSVIEW_SKILLS_SERVER", "https://env.invalid")
	out, err := executeCommand(newRootCommand(),
		"skills", "list", "--format", "json")
	require.NoError(err, "output: %s", out)

	var rows []skillListRow
	require.NoError(json.Unmarshal([]byte(out), &rows), "output: %s", out)
	require.NotEmpty(rows)
	for _, r := range rows {
		if r.Harness == string(skills.HarnessClaude) {
			assert.Equal("current", r.State,
				"an ambient env var must not mark an installed skill stale")
		}
	}

	assert.NotContains(readFileString(t, claudeSkillPath(home)),
		"--server https://env.invalid")
}

// TestSkillsInstall_QuotesServerValuesWithSpaces pins that a token path with
// a space stays a single shell word in the generated examples, which agents
// are told to run verbatim.
func TestSkillsInstall_QuotesServerValuesWithSpaces(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	_, err := executeCommand(newRootCommand(),
		"skills", "install", "--harness", "claude",
		"--server", "https://example.invalid",
		"--server-token-file", "/tmp/My Tokens/tok")
	require.NoError(t, err)

	body := readFileString(t, claudeSkillPath(home))
	assert.Contains(t, body, `--server-token-file '/tmp/My Tokens/tok'`)
	assert.Equal(t, "/tmp/My Tokens/tok", skills.ParseRemote(body).TokenFile,
		"quoting must not leak into the recorded remote")
}

func TestSkillsInstall_ProjectFlagOutsideRepoFallsBackToCWD(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	setTestHome(t, home)

	outsideRepo := t.TempDir()
	resolvedOutside, err := filepath.EvalSymlinks(outsideRepo)
	require.NoError(err)
	t.Chdir(resolvedOutside)

	out, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude", "--project")
	require.NoError(err, "output: %s", out)

	wantPath := claudeSkillPath(resolvedOutside)
	assert.Contains(out, wantPath)
	assert.FileExists(wantPath)
}
