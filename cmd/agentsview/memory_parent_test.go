package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"runtime"
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
			stubLaunchProcesses(t, tc.chain...)
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
		name      string
		payload   string
		chain     []fakeProcess
		wantErr   bool
		wantChild string
	}{
		{"no launcher", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{}}, {name: "bash"},
		}, false, childSessionID},
		{"detached mixed ancestry", sessionStartPayload("startup"), []fakeProcess{
			{name: "sh"}, {name: "claude", env: []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID, "CODEX_THREAD_ID=" + codexLauncherID}},
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
		name, launcher, sessionEnv, wantParent string
	}{
		{"codex", "codex", "CODEX_THREAD_ID=" + codexLauncherID, "codex:" + codexLauncherID},
		{"claude", "claude", "CLAUDE_CODE_SESSION_ID=" + claudeLauncherID, claudeLauncherID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			stubLaunchProcesses(t, fakeProcess{name: "sh"},
				fakeProcess{name: "claude", env: []string{tc.sessionEnv}},
				fakeProcess{name: tc.launcher, env: []string{"CODEX_HOME=~"}})
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantParent, link.ParentSessionID)
		})
	}
}

func TestOneShotRunArguments(t *testing.T) {
	p := launchProcess{Cmdline: func(context.Context) ([]string, error) { return []string{"codex", "-m", "model", "exec"}, nil }}
	assert.True(t, oneShotRun(t.Context(), p, "codex"))
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
			synctest.Test(t, func(t *testing.T) {
				annotator := &flakyAnnotator{failures: tc.failures, err: tc.err}
				stubFlakyLaunchArchive(t, annotator)
				stubLaunchProcesses(t,
					fakeProcess{name: "sh"},
					fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}},
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
