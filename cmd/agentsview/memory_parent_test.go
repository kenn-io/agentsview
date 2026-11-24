package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
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
		return launchProcess{Name: p.name, PPID: next, Cmdline: func(context.Context) ([]string, error) {
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
	originalOpen := openLaunchParentArchive
	t.Cleanup(func() {
		resolveLaunchAnnotator = original
		openLaunchParentArchive = originalOpen
	})
	resolveLaunchAnnotator = func(context.Context, *cobra.Command) (service.SessionAnnotator, func(), error) {
		return service.NewDirectBackend(database, nil).(service.SessionAnnotator), func() {}, nil
	}
	openLaunchParentArchive = func(ctx context.Context, _ *cobra.Command) (*db.DB, error) {
		return db.OpenReadOnly(ctx, database.Path())
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
	for _, tc := range []struct {
		child, launcher, wantChild, wantParent string
		env, args                              []string
		chain                                  []fakeProcess
	}{
		{"claude.exe", "claude", childSessionID, claudeLauncherID, []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}, nil, nil},
		{"claude", "codex", childSessionID, "codex:" + codexLauncherID, []string{"CODEX_THREAD_ID=" + codexLauncherID}, nil, nil},
		{"codex", "claude.exe", "codex:" + childSessionID, claudeLauncherID, []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}, nil, nil},
		{"codex.exe", "codex", "codex:" + childSessionID, "codex:" + codexLauncherID, []string{"CODEX_SESSION_ID=" + codexLauncherID}, nil, nil},
		{"codex", "claude", "codex:" + childSessionID, claudeLauncherID, []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}, []string{"e", "x"}, nil},
		{"claude", "codex", childSessionID, "codex:" + codexLauncherID, nil, nil, []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID, "CODEX_THREAD_ID=" + codexLauncherID}},
			{name: "bash"}, {name: "codex"}, {name: "bash"}, {name: "claude"},
		}},
	} {
		t.Run(tc.child+" from "+tc.launcher+fmt.Sprint(tc.args)+fmt.Sprint(len(tc.chain)), func(t *testing.T) {
			database := stubLaunchArchive(t)
			chain := tc.chain
			if chain == nil {
				chain = []fakeProcess{{name: "sh"}, {name: tc.child, env: tc.env, args: tc.args}, {name: "bash"}, {name: tc.launcher}}
			}
			home := t.TempDir()
			writeCodexState(t, home, codexLauncherID, codexLauncherID)
			chain[1].env = append(chain[1].env, "CODEX_HOME="+home)
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

func TestCaptureLaunchParentRecordsNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		chain   []fakeProcess
		wantErr bool
	}{
		{"no launcher", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{}}, {name: "bash"},
		}, false},
		{"no agent ancestor", sessionStartPayload("startup"), []fakeProcess{{name: "sh"}, {name: "bash"}}, false},
		{"missing launcher variable", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{"PATH=/bin"}}, {name: "codex"},
		}, false},
		{"invalid launcher ID", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{"CODEX_THREAD_ID=not-a-uuid"}}, {name: "codex"},
		}, true},
		{"same agent without readable environment", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude"}, {name: "claude"},
		}, false},
		{"invalid child ID", strings.Replace(sessionStartPayload("startup"), childSessionID, "bad", 1), []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}}, {name: "codex"},
		}, true},
		{"unreadable process", sessionStartPayload("startup"), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			stubLaunchProcesses(t, tc.chain...)
			_, err := runLaunchHook(t, tc.payload)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			_, err = database.GetSessionExternalParent(t.Context(), childSessionID)
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestCaptureLaunchParentKeepsClearedLinkAfterLaterEvents(t *testing.T) {
	database := stubLaunchArchive(t)
	home := t.TempDir()
	writeCodexState(t, home, codexLauncherID, codexLauncherID)
	stubLaunchProcesses(t,
		fakeProcess{name: "sh"},
		fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID, "CODEX_HOME=" + home}},
		fakeProcess{name: "codex"},
	)
	_, err := runLaunchHook(t, sessionStartPayload("startup"))
	require.NoError(t, err)
	_, err = database.ClearSessionExternalParent(t.Context(), childSessionID)
	require.NoError(t, err)

	for _, source := range []string{"resume", "clear", "compact"} {
		_, err := runLaunchHook(t, sessionStartPayload(source))
		require.NoError(t, err, source)
	}
	_, err = database.GetSessionExternalParent(t.Context(), childSessionID)
	require.ErrorIs(t, err, sql.ErrNoRows)
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
	if err != nil {
		t.Skipf("process environment is unavailable here: %v", err)
	}
	assert.NotEmpty(t, env)
}

func TestMemorySessionStartDisableSwitchSkipsLauncherLink(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })
	runMemorySessionStart = func(context.Context, memorySessionStartRequest) error { return nil }
	t.Setenv("AGENTSVIEW_DISABLE_AUTO_SYNC", "1")
	database := stubLaunchArchive(t)
	stubLaunchProcesses(t,
		fakeProcess{name: "sh"},
		fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}},
		fakeProcess{name: "codex"},
	)

	cmd := newRootCommand()
	cmd.SetIn(strings.NewReader(sessionStartPayload("startup")))
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"memory", "session-start", "--hook"})
	require.NoError(t, cmd.Execute())
	_, err := database.GetSessionExternalParent(t.Context(), childSessionID)
	require.ErrorIs(t, err, sql.ErrNoRows)
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

func TestCaptureLaunchParentGivesUpAtHookDeadline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		failures int
		retries  bool
	}{
		{"recovers", errors.New("database is locked"), 2, true},
		{"unavailable writer", errors.New("connection refused"), 1 << 30, true},
		{"rejected link", fmt.Errorf("%w: cycle", db.ErrSessionExternalParentInvalid), 1 << 30, false},
		{"read-only backend", db.ErrReadOnly, 1 << 30, false},
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
				if tc.failures == 2 {
					require.NoError(t, err)
					assert.Equal(t, 3, annotator.calls)
					assert.Equal(t, childSessionID, annotator.child)
					assert.Equal(t, "codex:"+codexLauncherID, annotator.parent)
					return
				}
				require.ErrorIs(t, err, tc.err)
				if tc.retries {
					assert.Greater(t, annotator.calls, 1)
				} else {
					assert.Equal(t, 1, annotator.calls)
				}
			})
		})
	}
}

func TestCaptureLaunchParentDetached(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"claude", []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}, claudeLauncherID},
		{"codex", []string{"CODEX_THREAD_ID=" + codexLauncherID}, "codex:" + codexLauncherID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			home := t.TempDir()
			writeCodexState(t, home, codexLauncherID, codexLauncherID)
			stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: "claude", env: append(tc.env, "CODEX_HOME="+home)})
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, link.ParentSessionID)
		})
	}
}

func TestCaptureLaunchParentDetachedInteractiveWithStaleID(t *testing.T) {
	for _, tc := range []struct {
		name, child, childID, env string
	}{
		{"claude", "claude", childSessionID, "CODEX_THREAD_ID=" + codexLauncherID},
		{"codex", "codex", "codex:" + childSessionID, "CLAUDE_CODE_SESSION_ID=" + claudeLauncherID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			// The terminal and its shell inherited an ID from an earlier run;
			// neither was launched by the agent now started interactively.
			stubLaunchProcesses(t,
				fakeProcess{name: "sh"},
				fakeProcess{name: tc.child, args: []string{}, env: []string{tc.env, "CODEX_HOME=" + t.TempDir()}},
				fakeProcess{name: "bash", env: []string{tc.env}},
				fakeProcess{name: "terminal", env: []string{tc.env}},
			)
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			_, err = database.GetSessionExternalParent(t.Context(), tc.childID)
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestCaptureLaunchParentDetachedPendingMixedProviders(t *testing.T) {
	for _, tc := range []struct {
		name, child, intermediate, outer, wantChild string
	}{
		{"codex intermediate", "claude", "codex:" + codexLauncherID, claudeLauncherID, childSessionID},
		{"claude intermediate", "codex", claudeLauncherID, "codex:" + codexLauncherID, "codex:" + childSessionID},
		{"ambiguous", "claude", "", "", childSessionID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			home := t.TempDir()
			writeCodexState(t, home, codexLauncherID, codexLauncherID)
			if tc.intermediate != "" {
				_, err := database.SetSessionExternalParent(t.Context(), tc.intermediate, tc.outer)
				require.NoError(t, err)
			}
			stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: tc.child, env: []string{
				"CODEX_HOME=" + home,
				"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID,
				"CODEX_THREAD_ID=" + codexLauncherID,
			}})
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), tc.wantChild)
			if tc.intermediate == "" {
				require.ErrorIs(t, err, sql.ErrNoRows)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.intermediate, link.ParentSessionID)
		})
	}
}

func TestOpenLocalLaunchParentArchive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	_, err := database.SetSessionExternalParent(t.Context(), "codex:"+codexLauncherID, claudeLauncherID)
	require.NoError(t, err)
	archive, err := openLocalLaunchParentArchive(t.Context(), &cobra.Command{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	link, err := archive.GetSessionExternalParent(t.Context(), "codex:"+codexLauncherID)
	require.NoError(t, err)
	assert.Equal(t, claudeLauncherID, link.ParentSessionID)
}

func writeCodexState(t *testing.T, home, thread, page string) {
	t.Helper()
	state, err := sql.Open("sqlite3", filepath.Join(home, "state_5.sqlite"))
	require.NoError(t, err)
	_, err = state.ExecContext(t.Context(), `CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = state.ExecContext(t.Context(), `INSERT INTO threads (id, rollout_path) VALUES (?, ?)`, thread, "rollout-2026-01-01-"+page+".jsonl")
	require.NoError(t, err)
	require.NoError(t, state.Close())
}

func TestCaptureDetachedCodexRevertPage(t *testing.T) {
	home := t.TempDir()
	page := codexLauncherID + "_44444444-4444-4444-8444-444444444444"
	writeCodexState(t, home, codexLauncherID, page)
	database := stubLaunchArchive(t)
	stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: "claude", env: []string{"CODEX_HOME=" + home, "CODEX_THREAD_ID=" + codexLauncherID}})
	_, err := runLaunchHook(t, sessionStartPayload("startup"))
	require.NoError(t, err)
	link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
	require.NoError(t, err)
	assert.Equal(t, "codex:"+page, link.ParentSessionID)

}

func TestCaptureLaunchParentDoesNotGuessCodexPage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"missing index", func(*testing.T, string) {}},
		{"invalid index", func(t *testing.T, home string) {
			require.NoError(t, os.WriteFile(filepath.Join(home, "state_5.sqlite"), []byte("invalid"), 0o600))
		}},
		{"missing thread", func(t *testing.T, home string) {
			writeCodexState(t, home, childSessionID, childSessionID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.setup(t, home)
			database := stubLaunchArchive(t)
			stubLaunchProcesses(t, fakeProcess{name: "sh"},
				fakeProcess{name: "claude", env: []string{"CODEX_HOME=" + home, "CODEX_THREAD_ID=" + codexLauncherID}},
				fakeProcess{name: "codex"})
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			_, err = database.GetSessionExternalParent(t.Context(), childSessionID)
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}
