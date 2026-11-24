package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

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
		return launchProcess{Name: p.name, PPID: next, Environ: func(context.Context) ([]string, error) {
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
	t.Cleanup(func() { resolveLaunchAnnotator = original })
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
	for _, tc := range []struct {
		child, launcher, wantChild, wantParent string
		env                                    []string
	}{
		{"claude.exe", "claude", childSessionID, claudeLauncherID, []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}},
		{"claude", "codex", childSessionID, "codex:" + codexLauncherID, []string{"CODEX_THREAD_ID=" + codexLauncherID}},
		{"codex", "claude.exe", "codex:" + childSessionID, claudeLauncherID, []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}},
		{"codex.exe", "codex", "codex:" + childSessionID, "codex:" + codexLauncherID, []string{"CODEX_SESSION_ID=" + codexLauncherID}},
	} {
		t.Run(tc.child+" from "+tc.launcher, func(t *testing.T) {
			database := stubLaunchArchive(t)
			stubLaunchProcesses(t,
				fakeProcess{name: "sh"},
				fakeProcess{name: tc.child, env: tc.env},
				fakeProcess{name: "bash"},
				fakeProcess{name: tc.launcher},
			)
			stderr, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err, stderr)

			// The link is stored before either transcript syncs.
			link, err := database.GetSessionExternalParent(t.Context(), tc.wantChild)
			require.NoError(t, err)
			assert.Equal(t, tc.wantParent, link.ParentSessionID)

			dbtest.SeedSession(t, database, tc.wantParent, "proj")
			dbtest.SeedSession(t, database, tc.wantChild, "proj")
			children, err := database.GetChildSessions(t.Context(), tc.wantParent)
			require.NoError(t, err)
			require.Len(t, children, 1)
			assert.Equal(t, tc.wantChild, children[0].ID)
		})
	}
}

func TestCaptureLaunchParentPicksNearestLauncherInAlternatingChain(t *testing.T) {
	// Claude A starts Codex B, which starts Claude C. C's environment still
	// carries A's Claude ID beside B's Codex ID.
	database := stubLaunchArchive(t)
	stubLaunchProcesses(t,
		fakeProcess{name: "sh"},
		fakeProcess{name: "claude.exe", env: []string{
			"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID,
			"CODEX_THREAD_ID=" + codexLauncherID,
		}},
		fakeProcess{name: "bash"},
		fakeProcess{name: "codex", env: []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID}},
		fakeProcess{name: "bash"},
		fakeProcess{name: "claude.exe"},
	)
	stderr, err := runLaunchHook(t, sessionStartPayload("startup"))
	require.NoError(t, err, stderr)
	link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
	require.NoError(t, err)
	assert.Equal(t, "codex:"+codexLauncherID, link.ParentSessionID)
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
			t.Setenv("CLAUDE_CODE_SESSION_ID", childSessionID)
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
	stubLaunchProcesses(t,
		fakeProcess{name: "sh"},
		fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}},
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
	dbtest.SeedSession(t, database, childSessionID, "proj")
	_, err = database.GetSessionExternalParent(t.Context(), childSessionID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	child, err := database.GetSession(t.Context(), childSessionID)
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Empty(t, child.ParentSessionID)
}

func TestLookupGopsutilProcessReadsAncestryAndEnvironment(t *testing.T) {
	self, err := lookupGopsutilProcess(t.Context(), int32(os.Getpid()))
	require.NoError(t, err)
	assert.Equal(t, int32(os.Getppid()), self.PPID)
	assert.NotEmpty(t, self.Name)
	env, err := self.Environ(t.Context())
	if err != nil {
		t.Skipf("process environment is unavailable here: %v", err)
	}
	assert.NotEmpty(t, env)
}
