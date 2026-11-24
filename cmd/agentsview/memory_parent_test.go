package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/testutil"
)

const (
	claudeLauncherID = "11111111-1111-4111-8111-111111111111"
	codexLauncherID  = "22222222-2222-4222-8222-222222222222"
	childSessionID   = "33333333-3333-4333-8333-333333333333"
)

// fakeProcess is one row of a stand-in process table. The walk starts at
// this test's parent PID, which plays the hook's shell.
type fakeProcess struct {
	name string
	// args is the command line after the program name. Unset, an agent runs
	// one-shot (-p or exec); set it to an empty slice for an interactive run.
	args []string
	env  []string
	cwd  string
}

func stubLaunchProcesses(t *testing.T, chain ...fakeProcess) {
	t.Helper()
	original := lookupLaunchProcess
	t.Cleanup(func() { lookupLaunchProcess = original })
	start := int32(os.Getppid())
	lookupLaunchProcess = func(_ context.Context, pid int32) (launchProcess, error) {
		i := int(pid - start)
		if pid < start || i >= len(chain) {
			return launchProcess{}, fmt.Errorf("no process %d", pid)
		}
		p := chain[i]
		next := pid + 1
		if i == len(chain)-1 {
			next = 1
		}
		return launchProcess{Name: p.name, PPID: next, Cwd: func(context.Context) (string, error) { return p.cwd, nil }, Cmdline: func(context.Context) ([]string, error) {
			if p.args != nil {
				return append([]string{p.name}, p.args...), nil
			}
			switch processAgent(p.name) {
			case "claude":
				return []string{p.name, "-p"}, nil
			case "codex":
				return []string{p.name, "exec"}, nil
			}
			return []string{p.name}, nil
		}, Environ: func(context.Context) ([]string, error) {
			if p.env == nil {
				return nil, errors.New("environment unavailable")
			}
			return p.env, nil
		}}, nil
	}
}

func stubLaunchArchive(t *testing.T) *db.DB {
	t.Helper()
	database := dbtest.OpenTestDB(t)
	original := resolveLaunchAnnotator
	t.Cleanup(func() {
		resolveLaunchAnnotator = original
	})
	resolveLaunchAnnotator = func(context.Context, *cobra.Command) (service.SessionAnnotator, func(), error) {
		return service.NewDirectBackend(database, nil).(service.SessionAnnotator), func() {}, nil
	}
	return database
}

func runLaunchHook(t *testing.T, payload string) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetIn(strings.NewReader(payload))
	cmd.SetErr(&stderr)
	return stderr.String(), captureLaunchParent(t.Context(), cmd)
}

func sessionStartPayload(source string) string {
	return fmt.Sprintf(`{"session_id":%q,"hook_event_name":"SessionStart","source":%q,"cwd":"/tmp","transcript_path":"/tmp/t.jsonl","model":"m","permission_mode":"default"}`, childSessionID, source)
}

func TestCaptureLaunchParentLinksEachProviderPair(t *testing.T) {
	chain := func(child, launcher string, env, args []string) []fakeProcess {
		return []fakeProcess{{name: "sh"}, {name: child, env: env, args: args}, {name: "bash"}, {name: launcher}}
	}
	for _, tc := range []struct {
		name, wantChild, wantParent string
		chain                       []fakeProcess
	}{
		{"detached claude", childSessionID, claudeLauncherID, []fakeProcess{{name: "sh"}, {name: "claude", env: []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}}}},
		{"detached codex", childSessionID, "codex:" + codexLauncherID, []fakeProcess{{name: "sh"}, {name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}}}},
		{"claude from claude", childSessionID, claudeLauncherID, chain("claude.exe", "claude", []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}, nil)},
		{"claude from codex", childSessionID, "codex:" + codexLauncherID, chain("claude", "codex", []string{"CODEX_THREAD_ID=" + codexLauncherID}, nil)},
		{"codex from claude", "codex:" + childSessionID, claudeLauncherID, chain("codex", "claude.exe", []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}, nil)},
		{"codex from codex", "codex:" + childSessionID, "codex:" + codexLauncherID, chain("codex.exe", "codex", []string{"CODEX_SESSION_ID=" + codexLauncherID}, nil)},
		{"closest codex", childSessionID, "codex:" + codexLauncherID, []fakeProcess{
			{name: "sh"},
			{name: "claude", env: []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID, "CODEX_THREAD_ID=" + codexLauncherID}},
			{name: "bash"},
			{name: "codex"},
			{name: "bash"},
			{name: "claude"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			chain := tc.chain
			if strings.HasPrefix(tc.wantParent, "codex:") {
				home := t.TempDir()
				writeCodexState(t, home, codexLauncherID, codexLauncherID)
				chain[1].env = append(chain[1].env, "CODEX_HOME="+home)
			}
			stubLaunchProcesses(t, chain...)
			stderr, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err, stderr)

			// The link is stored before either transcript syncs.
			link, err := database.GetSessionExternalParent(t.Context(), tc.wantChild)
			require.NoError(t, err)
			assert.Equal(t, tc.wantParent, link.ParentSessionID)
		})
	}
}

func TestCaptureLaunchParentDetachedMixedAncestry(t *testing.T) {
	for _, tc := range []struct {
		name, child, parent, want string
		pageParent                bool
	}{
		{"claude nearer", "claude", "codex", claudeLauncherID, false},
		{"codex nearer", "claude", "claude", "codex:" + codexLauncherID, false},
		{"codex child from claude", "codex", "codex", claudeLauncherID, false},
		{"codex child from codex", "codex", "claude", "codex:" + codexLauncherID, false},
		{"unrelated", "claude", "", "", false},
		{"claude nearer after revert", "claude", "codex", claudeLauncherID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeCodexState(t, home, codexLauncherID, codexLauncherID)
			database := stubLaunchArchive(t)
			for _, s := range []db.Session{
				{ID: claudeLauncherID, Agent: "claude", Project: "example", Machine: "example"},
				{ID: "codex:" + codexLauncherID, Agent: "codex", Project: "example", Machine: "example"},
			} {
				require.NoError(t, database.UpsertSession(t.Context(), s))
			}
			codexParent := "codex:" + codexLauncherID
			if tc.pageParent {
				codexParent += "_44444444-4444-4444-8444-444444444444"
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: codexParent, Agent: "codex", Project: "example", Machine: "example"}))
			}
			switch tc.parent {
			case "claude":
				_, err := database.SetSessionExternalParent(t.Context(), "codex:"+codexLauncherID, claudeLauncherID)
				require.NoError(t, err)
			case "codex":
				_, err := database.SetSessionExternalParent(t.Context(), claudeLauncherID, codexParent)
				require.NoError(t, err)
			}
			stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: tc.child, env: []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID, "CODEX_THREAD_ID=" + codexLauncherID, "CODEX_HOME=" + home}})
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			child := childSessionID
			if tc.child == "codex" {
				child = "codex:" + child
			}
			link, err := database.GetSessionExternalParent(t.Context(), child)
			if tc.want == "" {
				require.ErrorIs(t, err, sql.ErrNoRows)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, link.ParentSessionID)
			}
		})
	}
}

func TestCaptureLaunchParentRecordsNothing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		payload   string
		chain     []fakeProcess
		wantErr   bool
		wantChild string
	}{
		{"no launcher", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{}}, {name: "bash"},
		}, false, childSessionID},
		{"no agent ancestor", sessionStartPayload("startup"), []fakeProcess{{name: "sh"}, {name: "bash"}}, false, childSessionID},
		{"missing launcher variable", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{"PATH=/bin"}}, {name: "codex"},
		}, false, childSessionID},
		{"invalid launcher ID", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{"CODEX_THREAD_ID=not-a-uuid"}}, {name: "codex"},
		}, true, childSessionID},
		{"same agent without readable environment", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude"}, {name: "claude"},
		}, false, childSessionID},
		{"interactive claude with stale ID", sessionStartPayload("startup"), []fakeProcess{{name: "sh"}, {name: "claude", args: []string{}, env: []string{"CODEX_THREAD_ID=" + codexLauncherID}}, {name: "bash"}, {name: "terminal"}}, false, childSessionID},
		{"interactive codex with stale ID", sessionStartPayload("startup"), []fakeProcess{{name: "sh"}, {name: "codex", args: []string{}, env: []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}}, {name: "bash"}, {name: "terminal"}}, false, "codex:" + childSessionID},
		{"resume event", sessionStartPayload("resume"), []fakeProcess{{name: "sh"}, {name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}}, {name: "codex"}}, false, childSessionID},
		{"clear event", sessionStartPayload("clear"), []fakeProcess{{name: "sh"}, {name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}}, {name: "codex"}}, false, childSessionID},
		{"compact event", sessionStartPayload("compact"), []fakeProcess{{name: "sh"}, {name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}}, {name: "codex"}}, false, childSessionID},
		{"unreadable process", sessionStartPayload("startup"), nil, true, childSessionID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			if tc.name == "interactive claude with stale ID" || tc.name == "resume event" || tc.name == "clear event" || tc.name == "compact event" {
				home := t.TempDir()
				writeCodexState(t, home, codexLauncherID, codexLauncherID)
				tc.chain[1].env = append(tc.chain[1].env, "CODEX_HOME="+home)
			}
			stubLaunchProcesses(t, tc.chain...)
			_, err := runLaunchHook(t, tc.payload)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			_, err = database.GetSessionExternalParent(t.Context(), tc.wantChild)
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestCaptureLaunchParentWithoutCodexHome(t *testing.T) {
	homeEnv := "HOME"
	if runtime.GOOS == "windows" {
		homeEnv = "USERPROFILE"
	}
	t.Setenv(homeEnv, "")
	for _, tc := range []struct {
		name, launcher, sessionEnv string
		wantLink                   bool
	}{
		{"codex", "codex", "CODEX_THREAD_ID=" + codexLauncherID, false},
		{"claude", "claude", "CLAUDE_CODE_SESSION_ID=" + claudeLauncherID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			stubLaunchProcesses(t, fakeProcess{name: "sh"},
				fakeProcess{name: "claude", env: []string{tc.sessionEnv}},
				fakeProcess{name: tc.launcher, env: []string{"CODEX_HOME=~"}})
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
			if tc.wantLink {
				require.NoError(t, err)
				assert.Equal(t, claudeLauncherID, link.ParentSessionID)
			} else {
				require.ErrorIs(t, err, sql.ErrNoRows)
			}
		})
	}
}

func TestOneShotRunArguments(t *testing.T) {
	for _, tc := range []struct {
		name, agent string
		args        []string
		want        bool
	}{
		{"codex alias", "codex", []string{"e", "x"}, true},
		{"codex option before exec", "codex", []string{"-m", "model", "exec"}, true},
		{"claude continue before print", "claude", []string{"-c", "-p"}, true},
		{"codex directory named exec", "codex", []string{"-C", "exec"}, false},
		{"claude prompt flag value", "claude", []string{"--append-system-prompt", "-p"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := launchProcess{Cmdline: func(context.Context) ([]string, error) { return append([]string{tc.agent}, tc.args...), nil }}
			assert.Equal(t, tc.want, oneShotRun(t.Context(), p, tc.agent))
		})
	}
}

func TestLookupGopsutilProcessReadsAncestryAndEnvironment(t *testing.T) {
	self, err := lookupGopsutilProcess(t.Context(), int32(os.Getpid()))
	require.NoError(t, err)
	assert.Equal(t, int32(os.Getppid()), self.PPID)
	assert.NotEmpty(t, self.Name)
	args, err := self.Cmdline(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, args)
	env, err := self.Environ(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, env)
}

// flakyAnnotator fails its first `failures` writes, then records the link.
type flakyAnnotator struct {
	service.SessionAnnotator
	failures, calls int
	err             error
	child, parent   string
}

func (a *flakyAnnotator) SetSessionParent(_ context.Context, id, parentID string) (*db.SessionExternalParent, error) {
	a.calls++
	if a.calls <= a.failures {
		return nil, a.err
	}
	a.child, a.parent = id, parentID
	return &db.SessionExternalParent{SessionID: id, ParentSessionID: parentID}, nil
}

func stubFlakyLaunchArchive(t *testing.T, annotator *flakyAnnotator) {
	t.Helper()
	original := resolveLaunchAnnotator
	t.Cleanup(func() { resolveLaunchAnnotator = original })
	resolveLaunchAnnotator = func(context.Context, *cobra.Command) (service.SessionAnnotator, func(), error) {
		return annotator, func() {}, nil
	}
}

func TestCaptureLaunchParentWriteRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		failures  int
		wantCalls int
	}{
		{"recovers", errors.New("database is locked"), 2, 3},
		{"unavailable writer", errors.New("connection refused"), 1 << 30, 0},
		{"rejected link", fmt.Errorf("%w: cycle", db.ErrSessionExternalParentInvalid), 1 << 30, 1},
		{"read-only backend", db.ErrReadOnly, 1 << 30, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeCodexState(t, home, codexLauncherID, codexLauncherID)
			synctest.Test(t, func(t *testing.T) {
				annotator := &flakyAnnotator{failures: tc.failures, err: tc.err}
				stubFlakyLaunchArchive(t, annotator)
				stubLaunchProcesses(t,
					fakeProcess{name: "sh"},
					fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID, "CODEX_HOME=" + home}},
					fakeProcess{name: "codex"},
				)
				ctx, cancel := context.WithTimeout(t.Context(), memorySessionStartTimeout)
				defer cancel()
				cmd := &cobra.Command{}
				cmd.SetIn(strings.NewReader(sessionStartPayload("startup")))
				err := captureLaunchParent(ctx, cmd)
				if tc.wantCalls == 3 {
					require.NoError(t, err)
					assert.Equal(t, tc.wantCalls, annotator.calls)
					assert.Equal(t, childSessionID, annotator.child)
					assert.Equal(t, "codex:"+codexLauncherID, annotator.parent)
					return
				}
				require.ErrorIs(t, err, tc.err)
				if tc.wantCalls == 0 {
					assert.Greater(t, annotator.calls, 1)
				} else {
					assert.Equal(t, tc.wantCalls, annotator.calls)
				}
			})
		})
	}
}

func writeCodexState(t *testing.T, home, thread, page string) {
	t.Helper()
	testutil.WriteCodexState(t, home, "state_5.sqlite", thread, page)
}

func TestCaptureLaunchParentCodexHomeAndPage(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		childHome, launcherHome, sqliteHome bool
		page                                string
	}{
		{"launcher home", true, true, false, codexLauncherID + "_44444444-4444-4444-8444-444444444444"},
		{"sqlite home", false, true, true, codexLauncherID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			childHome, launcherHome, sqliteHome := t.TempDir(), t.TempDir(), t.TempDir()
			home := childHome
			if tc.launcherHome {
				home = launcherHome
			}
			if tc.sqliteHome {
				home = sqliteHome
			}
			writeCodexState(t, home, codexLauncherID, tc.page)
			childEnv := []string{"CODEX_THREAD_ID=" + codexLauncherID}
			if tc.childHome {
				childEnv = append(childEnv, "CODEX_HOME="+childHome)
			}
			launcherEnv := []string{}
			if tc.launcherHome {
				launcherEnv = append(launcherEnv, "CODEX_HOME="+launcherHome)
			}
			if tc.sqliteHome {
				launcherEnv = append(launcherEnv, "CODEX_SQLITE_HOME="+sqliteHome)
			}
			database := stubLaunchArchive(t)
			chain := []fakeProcess{{name: "sh"}, {name: "claude", env: childEnv}}
			if tc.launcherHome {
				chain = append(chain, fakeProcess{name: "codex", env: launcherEnv})
			}
			stubLaunchProcesses(t, chain...)
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
			require.NoError(t, err)
			assert.Equal(t, "codex:"+tc.page, link.ParentSessionID)
		})
	}
}

func TestCaptureLaunchParentCodexConfigOverride(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	page := codexLauncherID + "_44444444-4444-4444-8444-444444444444"
	writeCodexState(t, home, codexLauncherID, codexLauncherID)
	writeCodexState(t, state, codexLauncherID, page)
	database := stubLaunchArchive(t)
	stubLaunchProcesses(t,
		fakeProcess{name: "sh"},
		fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}},
		fakeProcess{name: "codex", env: []string{"CODEX_HOME=" + home}, args: []string{"-c", fmt.Sprintf("sqlite_home=%q", state)}},
	)
	_, err := runLaunchHook(t, sessionStartPayload("startup"))
	require.NoError(t, err)
	link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
	require.NoError(t, err)
	assert.Equal(t, "codex:"+page, link.ParentSessionID)
}

func TestCaptureLaunchParentCodexProfile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     func(string) []string
		override bool
	}{
		{"profile", func(string) []string { return []string{"--profile", "work"} }, false},
		{"short profile", func(string) []string { return []string{"-pwork"} }, false},
		{"override before profile", func(home string) []string {
			return []string{"-c", fmt.Sprintf("sqlite_home=%q", home), "--profile=work"}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, profileHome, overrideHome := t.TempDir(), t.TempDir(), t.TempDir()
			profilePage := codexLauncherID + "_44444444-4444-4444-8444-444444444444"
			overridePage := codexLauncherID + "_55555555-5555-4555-8555-555555555555"
			writeCodexState(t, home, codexLauncherID, codexLauncherID)
			writeCodexState(t, profileHome, codexLauncherID, profilePage)
			writeCodexState(t, overrideHome, codexLauncherID, overridePage)
			require.NoError(t, os.WriteFile(filepath.Join(home, "work.config.toml"), fmt.Appendf(nil, "sqlite_home = %q\n", profileHome), 0o600))
			database := stubLaunchArchive(t)
			stubLaunchProcesses(t,
				fakeProcess{name: "sh"},
				fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}},
				fakeProcess{name: "codex", env: []string{"CODEX_HOME=" + home}, args: tc.args(overrideHome)},
			)
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
			require.NoError(t, err)
			want := profilePage
			if tc.override {
				want = overridePage
			}
			assert.Equal(t, "codex:"+want, link.ParentSessionID)
		})
	}
}

func TestCaptureLaunchParentCodexRelativeOverride(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		args      []string
		dir       string
	}{
		{"config cwd", "", []string{"-c", "sqlite_home=state"}, "state"},
		{"config cd", "", []string{"--cd", "project", "-c", "sqlite_home=state"}, filepath.Join("project", "state")},
		{"environment cwd", " state ", []string{}, "state"},
		{"environment cd", "state", []string{"-C", "project"}, filepath.Join("project", "state")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			state := filepath.Join(cwd, tc.dir)
			require.NoError(t, os.MkdirAll(state, 0o700))
			page := codexLauncherID + "_44444444-4444-4444-8444-444444444444"
			writeCodexState(t, home, codexLauncherID, codexLauncherID)
			writeCodexState(t, state, codexLauncherID, page)
			database := stubLaunchArchive(t)
			stubLaunchProcesses(t,
				fakeProcess{name: "sh"},
				fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}},
				fakeProcess{name: "codex", env: []string{"CODEX_HOME=" + home, "CODEX_SQLITE_HOME=" + tc.env}, cwd: cwd, args: tc.args},
			)
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
			require.NoError(t, err)
			assert.Equal(t, "codex:"+page, link.ParentSessionID)
		})
	}
}

func TestCaptureLaunchParentCodexRelativeHomeWithoutLauncherDirectory(t *testing.T) {
	home := t.TempDir()
	database := stubLaunchArchive(t)
	stubLaunchProcesses(t,
		fakeProcess{name: "sh"},
		fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID, "CODEX_HOME=" + home, "CODEX_SQLITE_HOME=state"}},
	)
	_, err := runLaunchHook(t, sessionStartPayload("startup"))
	require.ErrorContains(t, err, "launcher directory unavailable")
	_, err = database.GetSessionExternalParent(t.Context(), childSessionID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestCaptureLaunchParentCodexSQLiteHomePaths(t *testing.T) {
	for _, tc := range []struct {
		name            string
		tilde, absolute bool
	}{
		{"relative to Codex home", false, false},
		{"home shorthand", true, false},
		{"absolute config overrides environment", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexHome, userHome := t.TempDir(), t.TempDir()
			t.Setenv("HOME", userHome)
			t.Setenv("USERPROFILE", userHome)
			base := codexHome
			path := "state"
			if tc.tilde {
				path = filepath.Join("~", "state")
				base = userHome
			}
			if tc.absolute {
				path = filepath.Join(codexHome, "state")
			}
			state := filepath.Join(base, "state")
			require.NoError(t, os.Mkdir(state, 0o700))
			writeCodexState(t, state, codexLauncherID, codexLauncherID)
			require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), fmt.Appendf(nil, "sqlite_home = %q\n", path), 0o600))
			database := stubLaunchArchive(t)
			stubLaunchProcesses(t, fakeProcess{name: "sh"},
				fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}},
				fakeProcess{name: "codex", env: []string{"CODEX_HOME=" + codexHome, "CODEX_SQLITE_HOME=" + t.TempDir()}})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			cmd := &cobra.Command{}
			cmd.SetIn(strings.NewReader(sessionStartPayload("startup")))
			require.NoError(t, captureLaunchParent(ctx, cmd))
			link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
			require.NoError(t, err)
			assert.Equal(t, "codex:"+codexLauncherID, link.ParentSessionID)
		})
	}
}

func TestMemorySessionStartCaptureHasOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		original := runMemorySessionStart
		t.Cleanup(func() { runMemorySessionStart = original })
		database := stubLaunchArchive(t)
		runMemorySessionStart = func(ctx context.Context, _ memorySessionStartRequest) error {
			_, err := database.GetSessionExternalParent(ctx, childSessionID)
			require.ErrorIs(t, err, sql.ErrNoRows)
			<-ctx.Done()
			return ctx.Err()
		}
		stubLaunchProcesses(t, fakeProcess{name: "sh"},
			fakeProcess{name: "claude", env: []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}},
			fakeProcess{name: "claude"})
		cmd := newRootCommand()
		cmd.SetIn(strings.NewReader(sessionStartPayload("startup")))
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"memory", "session-start", "--hook"})
		_ = cmd.Execute()
		link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
		require.NoError(t, err)
		assert.Equal(t, claudeLauncherID, link.ParentSessionID)
	})
}

func TestMemorySessionStartInvalidModeDoesNotCapture(t *testing.T) {
	database := stubLaunchArchive(t)
	stubLaunchProcesses(t, fakeProcess{name: "sh"},
		fakeProcess{name: "claude", env: []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}},
		fakeProcess{name: "claude"})
	cmd := newRootCommand()
	cmd.SetIn(strings.NewReader(sessionStartPayload("startup")))
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"memory", "session-start", "--hook", "--mode", "invalid"})
	require.NoError(t, cmd.Execute())
	_, err := database.GetSessionExternalParent(t.Context(), childSessionID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestCaptureLaunchParentCodexIndexRetry(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		rowMissing, unreadable, locked, newer, migration bool
	}{
		{"missing index", false, false, false, false, false},
		{"missing row", true, false, false, false, false},
		{"unreadable index", false, true, false, false, false},
		{"locked index", false, false, true, false, false},
		{"newer index", false, false, false, true, false},
		{"newer migration", false, false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			var locked *sql.DB
			if tc.locked {
				writeCodexState(t, home, codexLauncherID, codexLauncherID)
				var err error
				locked, err = sql.Open("sqlite3", filepath.Join(home, "state_5.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, locked.Close()) })
				_, err = locked.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
				require.NoError(t, err)
			}
			if tc.rowMissing {
				writeCodexState(t, home, childSessionID, childSessionID)
			}
			if tc.unreadable {
				require.NoError(t, os.WriteFile(filepath.Join(home, "state_5.sqlite"), []byte("corrupt"), 0o600))
			}
			if tc.newer {
				writeCodexState(t, home, codexLauncherID, codexLauncherID+"_44444444-4444-4444-8444-444444444444")
				require.NoError(t, os.WriteFile(filepath.Join(home, "state_6.sqlite"), nil, 0o600))
			}
			if tc.migration {
				writeCodexState(t, home, codexLauncherID, codexLauncherID+"_44444444-4444-4444-8444-444444444444")
				state, err := sql.Open("sqlite3", filepath.Join(home, "state_5.sqlite"))
				require.NoError(t, err)
				_, err = state.ExecContext(t.Context(), "INSERT INTO _sqlx_migrations VALUES (55)")
				require.NoError(t, err)
				require.NoError(t, state.Close())
			}
			synctest.Test(t, func(t *testing.T) {
				database := stubLaunchArchive(t)
				stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: "claude", env: []string{"CODEX_HOME=" + home, "CODEX_THREAD_ID=" + codexLauncherID}}, fakeProcess{name: "codex"})
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				cmd := &cobra.Command{}
				cmd.SetIn(strings.NewReader(sessionStartPayload("startup")))
				done := make(chan error, 1)
				go func() { done <- captureLaunchParent(ctx, cmd) }()
				if tc.locked {
					select {
					case err := <-done:
						require.FailNow(t, "capture returned while index was locked", "%v", err)
					case <-time.After(2 * time.Second):
					}
					_, err := locked.ExecContext(t.Context(), "COMMIT")
					require.NoError(t, err)
				}
				if !tc.locked {
					select {
					case err := <-done:
						if tc.unreadable {
							require.Error(t, err)
						} else {
							require.NoError(t, err)
						}
					case <-time.After(time.Second):
						require.FailNow(t, "missing or unreadable index was retried")
					}
				} else {
					require.NoError(t, <-done)
				}
				link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
				if !tc.unreadable {
					require.NoError(t, err)
					assert.Equal(t, "codex:"+codexLauncherID, link.ParentSessionID)
				} else {
					require.ErrorIs(t, err, sql.ErrNoRows)
				}
			})
		})
	}
}
