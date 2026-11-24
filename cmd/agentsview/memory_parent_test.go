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
	"time"

	"github.com/shirou/gopsutil/v4/process"
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
	name  string
	env   []string
	files []process.OpenFilesStat
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
		return launchProcess{Name: p.name, PPID: next, OpenFiles: func(context.Context) ([]process.OpenFilesStat, error) { return p.files, nil }, Environ: func(context.Context) ([]string, error) {
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

func TestCaptureLaunchParentRetriesTransientWriteFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		annotator := &flakyAnnotator{failures: 2, err: errors.New("database is locked")}
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
		require.NoError(t, captureLaunchParent(ctx, cmd))
		assert.Equal(t, 3, annotator.calls)
		assert.Equal(t, childSessionID, annotator.child)
		assert.Equal(t, "codex:"+codexLauncherID, annotator.parent)
	})
}

func TestCaptureLaunchParentGivesUpAtHookDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		retries bool
	}{
		{"unavailable writer", errors.New("connection refused"), true},
		{"rejected link", fmt.Errorf("%w: cycle", db.ErrSessionExternalParentInvalid), false},
		{"read-only backend", db.ErrReadOnly, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				annotator := &flakyAnnotator{failures: 1 << 30, err: tc.err}
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
				start := time.Now()
				require.ErrorIs(t, captureLaunchParent(ctx, cmd), tc.err)
				assert.LessOrEqual(t, time.Since(start), memorySessionStartTimeout)
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
		{"ambiguous", []string{"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID, "CODEX_THREAD_ID=" + codexLauncherID}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: "claude", env: tc.env})
			_, err := runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
			if tc.want == "" {
				require.ErrorIs(t, err, sql.ErrNoRows)
				return
			}
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
				fakeProcess{name: tc.child, env: []string{tc.env, "CODEX_HOME=" + t.TempDir()}},
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

func TestCaptureLaunchParentDetachedMixedProviders(t *testing.T) {
	for _, tc := range []struct {
		name, child, intermediate, outer, wantChild string
	}{
		{"codex intermediate", "claude", "codex:" + codexLauncherID, claudeLauncherID, childSessionID},
		{"claude intermediate", "codex", claudeLauncherID, "codex:" + codexLauncherID, "codex:" + childSessionID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			dbtest.SeedSession(t, database, tc.outer, "proj")
			dbtest.SeedSession(t, database, tc.intermediate, "proj")
			_, err := database.SetSessionExternalParent(t.Context(), tc.intermediate, tc.outer)
			require.NoError(t, err)
			stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: tc.child, env: []string{
				"CODEX_HOME=" + t.TempDir(),
				"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID,
				"CODEX_THREAD_ID=" + codexLauncherID,
			}})
			_, err = runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), tc.wantChild)
			require.NoError(t, err)
			assert.Equal(t, tc.intermediate, link.ParentSessionID)
		})
	}
}

func TestCaptureLaunchParentDetachedPendingMixedProviders(t *testing.T) {
	for _, tc := range []struct {
		name, child, intermediate, outer, wantChild string
	}{
		{"codex intermediate", "claude", "codex:" + codexLauncherID, claudeLauncherID, childSessionID},
		{"claude intermediate", "codex", claudeLauncherID, "codex:" + codexLauncherID, "codex:" + childSessionID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := stubLaunchArchive(t)
			_, err := database.SetSessionExternalParent(t.Context(), tc.intermediate, tc.outer)
			require.NoError(t, err)
			stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: tc.child, env: []string{
				"CODEX_HOME=" + t.TempDir(),
				"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID,
				"CODEX_THREAD_ID=" + codexLauncherID,
			}})
			_, err = runLaunchHook(t, sessionStartPayload("startup"))
			require.NoError(t, err)
			link, err := database.GetSessionExternalParent(t.Context(), tc.wantChild)
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

func TestCaptureLaunchParentDetachedMixedProvidersRevertPage(t *testing.T) {
	home := t.TempDir()
	state, err := sql.Open("sqlite3", filepath.Join(home, "state_5.sqlite"))
	require.NoError(t, err)
	_, err = state.ExecContext(t.Context(), `CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL)`)
	require.NoError(t, err)
	page := codexLauncherID + "_44444444-4444-4444-8444-444444444444"
	_, err = state.ExecContext(t.Context(), `INSERT INTO threads VALUES (?, ?)`, codexLauncherID, "rollout-2026-01-01-"+page+".jsonl")
	require.NoError(t, err)
	require.NoError(t, state.Close())
	database := stubLaunchArchive(t)
	dbtest.SeedSession(t, database, claudeLauncherID, "proj")
	dbtest.SeedSession(t, database, "codex:"+page, "proj")
	_, err = database.SetSessionExternalParent(t.Context(), "codex:"+page, claudeLauncherID)
	require.NoError(t, err)
	stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: "claude", env: []string{
		"CODEX_HOME=" + home,
		"CLAUDE_CODE_SESSION_ID=" + claudeLauncherID,
		"CODEX_THREAD_ID=" + codexLauncherID,
	}})
	_, err = runLaunchHook(t, sessionStartPayload("startup"))
	require.NoError(t, err)
	link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
	require.NoError(t, err)
	assert.Equal(t, "codex:"+page, link.ParentSessionID)
}

func TestCaptureLaunchParentUsesCodexRevertPage(t *testing.T) {
	database := stubLaunchArchive(t)
	rollout := "44444444-4444-4444-8444-444444444444"
	page := codexLauncherID + "_" + rollout
	stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: "claude", env: []string{"CODEX_THREAD_ID=" + codexLauncherID}}, fakeProcess{name: "codex", files: []process.OpenFilesStat{{Path: "rollout-2026-01-01-" + page + ".jsonl"}}})
	_, err := runLaunchHook(t, sessionStartPayload("startup"))
	require.NoError(t, err)
	link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
	require.NoError(t, err)
	assert.Equal(t, "codex:"+page, link.ParentSessionID)
	dbtest.SeedSession(t, database, "codex:"+page, "proj")
	dbtest.SeedSession(t, database, childSessionID, "proj")
	children, err := database.GetChildSessions(t.Context(), "codex:"+page)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, childSessionID, children[0].ID)
}

func TestCaptureDetachedCodexRevertPage(t *testing.T) {
	home := t.TempDir()
	state, err := sql.Open("sqlite3", filepath.Join(home, "state_5.sqlite"))
	require.NoError(t, err)
	_, err = state.ExecContext(t.Context(), `CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL)`)
	require.NoError(t, err)
	page := codexLauncherID + "_44444444-4444-4444-8444-444444444444"
	_, err = state.ExecContext(t.Context(), `INSERT INTO threads (id, rollout_path) VALUES (?, ?)`, codexLauncherID, "rollout-2026-01-01-"+page+".jsonl")
	require.NoError(t, err)
	require.NoError(t, state.Close())
	database := stubLaunchArchive(t)
	stubLaunchProcesses(t, fakeProcess{name: "sh"}, fakeProcess{name: "claude", env: []string{"CODEX_HOME=" + home, "CODEX_THREAD_ID=" + codexLauncherID}})
	_, err = runLaunchHook(t, sessionStartPayload("startup"))
	require.NoError(t, err)
	link, err := database.GetSessionExternalParent(t.Context(), childSessionID)
	require.NoError(t, err)
	assert.Equal(t, "codex:"+page, link.ParentSessionID)
	dbtest.SeedSession(t, database, "codex:"+page, "proj")
	dbtest.SeedSession(t, database, childSessionID, "proj")
	children, err := database.GetChildSessions(t.Context(), "codex:"+page)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, childSessionID, children[0].ID)
}

func TestCodexLauncherPagePrefersCurrentIndex(t *testing.T) {
	home := t.TempDir()
	state, err := sql.Open("sqlite3", filepath.Join(home, "state_5.sqlite"))
	require.NoError(t, err)
	_, err = state.ExecContext(t.Context(), `CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL)`)
	require.NoError(t, err)
	page := codexLauncherID + "_44444444-4444-4444-8444-444444444444"
	_, err = state.ExecContext(t.Context(), `INSERT INTO threads VALUES (?, ?)`, codexLauncherID, "rollout-2026-01-01-"+page+".jsonl")
	require.NoError(t, err)
	require.NoError(t, state.Close())
	child := launchProcess{Environ: func(context.Context) ([]string, error) { return []string{"CODEX_HOME=" + home}, nil }}
	parent := launchProcess{OpenFiles: func(context.Context) ([]process.OpenFilesStat, error) {
		return []process.OpenFilesStat{{Path: "rollout-2026-01-01-" + codexLauncherID + ".jsonl"}}, nil
	}}
	assert.Equal(t, page, codexLauncherPage(t.Context(), child, parent, codexLauncherID))
}
