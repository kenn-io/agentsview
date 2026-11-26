package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchParentInference(t *testing.T) {
	d := testDB(t)
	launch := func(id, command string) {
		insertSession(t, d, id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
		insertMessages(t, d, Message{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", HasToolUse: true, ToolCalls: []ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", InputJSON: command}}})
	}
	launch("parent", `{"command":"codex exec -C /repo 'review'"}`)
	start := "2026-01-02T03:04:03Z"
	insertSession(t, d, "child", "project", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo"
		s.SessionKind = "non-interactive"
		s.StartedAt = &start
	})
	_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
	require.NoError(t, err)
	parent, err := d.GetSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	assert.Equal(t, "parent", parent.ParentSessionID)
	assert.True(t, parent.Applied)
	children, err := d.GetChildSessions(t.Context(), "parent")
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, "child", children[0].ID)
	launch("other", `{"command":"codex exec < prompt.md"}`)
	_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"other"})
	require.NoError(t, err)
	_, err = d.GetSessionExternalParent(t.Context(), "child")
	require.Error(t, err)
	_, err = d.SetSessionExternalParent(t.Context(), "child", "parent")
	require.NoError(t, err)
	_, err = d.ClearSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
	require.NoError(t, err)
	_, err = d.GetSessionExternalParent(t.Context(), "child")
	require.Error(t, err)
	_, err = d.SetSessionExternalParent(t.Context(), "child", "other")
	require.NoError(t, err)
	parent, err = d.GetSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	assert.Equal(t, "other", parent.ParentSessionID)
}

func TestLaunchParentConditionalExecution(t *testing.T) {
	for _, input := range []string{
		`{"command":"false && codex exec hi"}`,
		`false && tools.exec_command({cmd: "codex exec hi"})`,
		`{"command":"test -d /repo && codex exec hi"}`,
		`flag && tools.exec_command({cmd: "codex exec hi"})`,
		`{"command":"exit; codex exec hi"}`,
		`exit(); tools.exec_command({cmd: "codex exec hi"})`,
		`{"command":"true && exit; codex exec hi"}`,
		`{"command":"if true; then exit; fi; codex exec hi"}`,
		`{"command":"exit && codex exec hi"}`,
		`exit() && tools.exec_command({cmd: "codex exec hi"})`,
		`{"command":"if exit; then codex exec hi; fi"}`,
		`if (exit()) { tools.exec_command({cmd: "codex exec hi"}) }`,
		`if (flag) { exit() }; tools.exec_command({cmd: "codex exec hi"})`,
	} {
		t.Run(input, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
			insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: input}}})
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			_, err = d.GetSessionExternalParent(t.Context(), "child")
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestLaunchParentConditionalCompetitor(t *testing.T) {
	for _, tc := range []struct {
		command string
		blocked bool
		raw     bool
	}{
		{`false && codex exec hi`, false, false},
		{`exit && codex exec hi`, false, false},
		{`if exit; then codex exec hi; fi`, false, false},
		{`exit() && tools.exec_command({cmd: "codex exec hi"})`, false, true},
		{`if (exit()) { tools.exec_command({cmd: "codex exec hi"}) }`, false, true},
		{`test -d /repo && codex exec hi`, true, false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			d := testDB(t)
			for _, parent := range []struct{ id, command string }{{"actual", "codex exec hi"}, {"conditional", tc.command}} {
				insertSession(t, d, parent.id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
				input, err := json.Marshal(map[string]string{"command": parent.command})
				require.NoError(t, err)
				if parent.id == "conditional" && tc.raw {
					input = []byte(parent.command)
				}
				insertMessages(t, d, Message{SessionID: parent.id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: parent.id, ToolName: "Bash", Category: "Bash", InputJSON: string(input)}}})
			}
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			parent, err := d.GetSessionExternalParent(t.Context(), "child")
			if tc.blocked {
				require.ErrorIs(t, err, sql.ErrNoRows)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "actual", parent.ParentSessionID)
			}
		})
	}
}

func TestLaunchParentHookVersionGuard(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "inferred", "project")
	insertSession(t, d, "hook", "project")
	insertSession(t, d, "child", "project")
	_, err := d.SetSessionExternalParent(t.Context(), "child", "inferred")
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(t.Context(), `UPDATE session_external_parents SET inferred=1 WHERE session_id='child'`)
	require.NoError(t, err)
	_, err = d.SetSessionExternalParent(t.Context(), "child", "hook")
	require.NoError(t, err)
	var parent string
	var inferred bool
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT parent_session_id,inferred FROM session_external_parents WHERE session_id='child'`).Scan(&parent, &inferred))
	assert.Equal(t, "hook", parent)
	assert.False(t, inferred)
	var version int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version))
	assert.Greater(t, version, 189, "a version-189 binary must reject this archive before it can write a hook link")
}

func TestLaunchParentBackfillPages(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec review"}`}}})
	tx, err := d.getWriter().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for i := range 3001 {
		started := "2026-01-01T03:04:03Z"
		if i == 3000 {
			started = "2026-01-02T03:04:03Z"
		}
		_, err = tx.ExecContext(t.Context(), `INSERT INTO sessions (id,project,agent,session_kind,cwd,started_at) VALUES (?, 'project', 'codex', 'non-interactive', '/repo', ?)`, fmt.Sprintf("child-%04d", i), started)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	_, err = d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET source_missing_at='2026-01-03T00:00:00Z' WHERE id='child-0000'`)
	require.NoError(t, err)
	_, err = d.LinkSubagentSessionsContext(t.Context())
	require.NoError(t, err)
	state, err := d.GetSyncState(t.Context(), launchBackfillKey)
	require.NoError(t, err)
	assert.Equal(t, "done", state)
	var linked int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE parent_session_id = 'parent'`).Scan(&linked))
	assert.Equal(t, 1, linked)
	var parent sql.NullString
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT parent_session_id FROM sessions WHERE id='child-3000'`).Scan(&parent))
	assert.Equal(t, "parent", parent.String)
}

func TestLaunchParentAgentPairs(t *testing.T) {
	for _, parentAgent := range []string{"claude", "codex"} {
		for _, childAgent := range []string{"claude", "codex"} {
			t.Run(parentAgent+"-"+childAgent, func(t *testing.T) {
				d := testDB(t)
				insertSession(t, d, "launcher", "project", func(s *Session) { s.Agent = parentAgent; s.Cwd = "/repo" })
				input := `{"cmd":"codex exec hi"}`
				if childAgent == "claude" {
					input = `{"cmd":"claude -p hi"}`
				}
				insertMessages(t, d, Message{SessionID: "launcher", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00.250Z", ToolCalls: []ToolCall{{SessionID: "launcher", ToolName: "exec_command", Category: "Bash", InputJSON: input}}})
				start := "2026-01-02T03:05:00.250Z"
				insertSession(t, d, "run", "project", func(s *Session) {
					s.Agent = childAgent
					s.Cwd = "/repo"
					s.StartedAt = &start
					if childAgent == "claude" {
						s.Entrypoint = "sdk-cli"
					} else {
						s.SessionKind = "non-interactive"
					}
				})
				_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"launcher"})
				require.NoError(t, err)
				link, err := d.GetSessionExternalParent(t.Context(), "run")
				require.NoError(t, err)
				assert.Equal(t, "launcher", link.ParentSessionID)
			})
		}
	}
}

func TestLaunchParentRebuildKeepsClear(t *testing.T) {
	source := testDB(t)
	insertSession(t, source, "parent", "project")
	insertSession(t, source, "child", "project")
	_, err := source.SetSessionExternalParent(t.Context(), "child", "parent")
	require.NoError(t, err)
	_, err = source.ClearSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	dest := testDB(t)
	insertSession(t, dest, "parent", "project")
	insertSession(t, dest, "child", "project")
	_, err = dest.getWriter().ExecContext(t.Context(), `INSERT INTO session_external_parents (session_id,parent_session_id,inferred) VALUES ('child','parent',1)`)
	require.NoError(t, err)
	_, err = dest.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
	require.NoError(t, err)
	require.NoError(t, dest.CopySessionMetadataFrom(t.Context(), source.Path()))
	_, err = dest.GetSessionExternalParent(t.Context(), "child")
	require.Error(t, err)
	child, err := dest.GetSession(t.Context(), "child")
	require.NoError(t, err)
	assert.Nil(t, child.ParentSessionID)
}

func TestLaunchParentFolderMachineAndWindow(t *testing.T) {
	for _, tc := range []struct {
		name, cwd, machine, started string
		want                        bool
	}{
		{"matching Windows path", `c:\repo`, "local", "2026-01-02T03:05:00.250Z", true},
		{"other folder", `c:\elsewhere`, "local", "2026-01-02T03:04:03Z", false},
		{"other machine", `c:\repo`, "other", "2026-01-02T03:04:03Z", false},
		{"outside window", `c:\repo`, "local", "2026-01-02T03:05:00.251Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "launcher", "project", func(s *Session) { s.Agent = "codex"; s.Cwd = `C:\repo` })
			insertMessages(t, d, Message{SessionID: "launcher", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00.250Z", ToolCalls: []ToolCall{{SessionID: "launcher", ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"codex.exe exec hi", "workdir":"C:\\repo"}`}}})
			start := tc.started
			insertSession(t, d, "run", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = tc.cwd
				s.Machine = tc.machine
				s.StartedAt = &start
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"run"})
			require.NoError(t, err)
			_, err = d.GetSessionExternalParent(t.Context(), "run")
			if tc.want {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestLaunchParentCandidateQueryPlan(t *testing.T) {
	d := testDB(t)
	plan := explainQueryPlan(t, d, launchCandidateSQL, "2026-01-01", "2026-01-02", "local", "child")
	require.NotEmpty(t, plan)
	assert.Contains(t, plan[0], "idx_messages_activity_timestamp")
	assert.Contains(t, plan[1], "idx_tool_calls_message")
}

func TestLaunchParentSchemaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "archive")
	insertSession(t, d, "parent", "project")
	insertSession(t, d, "child", "project")
	_, err := d.SetSessionExternalParent(t.Context(), "child", "parent")
	require.NoError(t, err)
	require.NoError(t, d.Close())
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `ALTER TABLE session_external_parents DROP COLUMN inferred`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `DROP TABLE session_parent_inference_blocks`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA user_version=189`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	migrated, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, migrated.Close()) })
	var version int
	require.NoError(t, migrated.getReader().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version))
	assert.Equal(t, dataVersion, version)
	var inferred int
	require.NoError(t, migrated.getReader().QueryRowContext(t.Context(), `SELECT inferred FROM session_external_parents WHERE session_id='child'`).Scan(&inferred))
	assert.Zero(t, inferred)
	_, err = migrated.ClearSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	var blocked int
	require.NoError(t, migrated.getReader().QueryRowContext(t.Context(), `SELECT count(*) FROM session_parent_inference_blocks WHERE session_id='child'`).Scan(&blocked))
	assert.Equal(t, 1, blocked)
}

func TestLaunchParentRetractsUnusableChild(t *testing.T) {
	for _, tc := range []struct{ name, update string }{
		{"missing cwd", `UPDATE sessions SET cwd='' WHERE id='child'`},
		{"missing machine", `UPDATE sessions SET machine='' WHERE id='child'`},
		{"missing timestamp", `UPDATE sessions SET started_at=NULL WHERE id='child'`},
		{"invalid timestamp", `UPDATE sessions SET started_at='invalid' WHERE id='child'`},
		{"interactive", `UPDATE sessions SET session_kind='' WHERE id='child'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
			insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
			start := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.SessionKind = "non-interactive"
				s.StartedAt = &start
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(t.Context(), "child")
			require.NoError(t, err)
			assert.True(t, link.Applied)
			_, err = d.getWriter().ExecContext(t.Context(), tc.update)
			require.NoError(t, err)
			_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			_, err = d.GetSessionExternalParent(t.Context(), "child")
			require.ErrorIs(t, err, sql.ErrNoRows)
			child, err := d.GetSession(t.Context(), "child")
			require.NoError(t, err)
			assert.Nil(t, child.ParentSessionID)
		})
	}
}

func TestLaunchParentUnknownLauncherFolder(t *testing.T) {
	for _, tc := range []struct{ name, cwd, input string }{
		{"missing launcher folder", "", `{"command":"codex exec hi"}`},
		{"relative workdir", "", `{"command":"codex exec hi","workdir":"repo"}`},
		{"dynamic shell cd", "/other", `{"command":"cd \"$DIR\" && codex exec hi"}`},
		{"dynamic codex cd", "/other", `{"command":"codex -C \"$DIR\" exec hi"}`},
		{"dynamic long cd", "/other", `{"command":"codex --cd=\"$DIR\" exec hi"}`},
		{"conditional cd", "/repo", `{"command":"cd /other || codex exec hi"}`},
		{"unchecked cd", "/repo", `{"command":"cd /missing; codex exec hi"}`},
		{"undecodable input", "/repo", `{`},
		{"home cd", "/repo", `{"command":"cd; codex exec hi"}`},
		{"previous cd", "/repo", `{"command":"cd -; codex exec hi"}`},
		{"tilde cd", "/repo", `{"command":"cd ~/repo; codex exec hi"}`},
		{"pushd", "/other", `{"command":"pushd /repo; codex exec hi"}`},
		{"popd", "/other", `{"command":"popd; codex exec hi"}`},
		{"eval cd", "/other", `{"command":"eval 'cd /repo'; codex exec hi"}`},
		{"source", "/other", `{"command":"source setup.sh; codex exec hi"}`},
		{"dot source", "/other", `{"command":". setup.sh; codex exec hi"}`},
		{"loop cd", "/other", `{"command":"while test -d x; do cd /repo; done; codex exec hi"}`},
		{"block cd", "/other", `{"command":"{ cd /repo; }; codex exec hi"}`},
		{"dynamic code mode workdir", "/other", `text(await tools.exec_command({cmd: "codex exec hi", workdir: dir}))`},
		{"overridden code mode workdir", "/other", `text(await tools.exec_command({cmd: "codex exec hi", workdir: "/other", workdir: dir}))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			for _, launcher := range []struct{ id, cwd, input string }{{"known", "/repo", `{"command":"codex exec hi"}`}, {"unknown", tc.cwd, tc.input}} {
				insertSession(t, d, launcher.id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = launcher.cwd })
				insertMessages(t, d, Message{SessionID: launcher.id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: launcher.id, ToolName: "Bash", Category: "Bash", InputJSON: launcher.input}}})
			}
			start := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.SessionKind = "non-interactive"
				s.StartedAt = &start
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			_, err = d.GetSessionExternalParent(t.Context(), "child")
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestLaunchParentTrashEvidence(t *testing.T) {
	t.Run("late trashed competitor", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		add := func(id string) {
			insertSession(t, d, id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
			insertMessages(t, d, Message{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
		}
		add("first")
		start := "2026-01-02T03:04:03Z"
		insertSession(t, d, "child", "project", func(s *Session) {
			s.Agent = "codex"
			s.Cwd = "/repo"
			s.StartedAt = &start
			s.SessionKind = "non-interactive"
		})
		_, err := d.LinkSubagentSessionsForSessions(ctx, []string{"child"})
		require.NoError(t, err)
		_, err = d.GetSessionExternalParent(ctx, "child")
		require.NoError(t, err)
		add("second")
		require.NoError(t, d.SoftDeleteSession(ctx, "second"))
		_, err = d.GetSessionExternalParent(ctx, "child")
		require.ErrorIs(t, err, sql.ErrNoRows)
	})
	for _, action := range []string{"single", "batch"} {
		t.Run(action, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			add := func(id string) {
				insertSession(t, d, id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
				insertMessages(t, d, Message{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex -m $MODEL exec hi"}`}}})
			}
			add("first")
			start := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.StartedAt = &start
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(ctx, []string{"child"})
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(ctx, "child")
			require.NoError(t, err)
			assert.Equal(t, "first", link.ParentSessionID)
			if action == "single" {
				require.NoError(t, d.SoftDeleteSession(ctx, "first"))
			} else {
				_, err = d.SoftDeleteSessions(ctx, []string{"first"})
				require.NoError(t, err)
			}
			_, err = d.GetSessionExternalParent(ctx, "child")
			require.ErrorIs(t, err, sql.ErrNoRows)
			_, err = d.RestoreSession(ctx, "first")
			require.NoError(t, err)
			link, err = d.GetSessionExternalParent(ctx, "child")
			require.NoError(t, err)
			assert.Equal(t, "first", link.ParentSessionID)
			require.NoError(t, d.SoftDeleteSession(ctx, "first"))
			add("second")
			_, err = d.LinkSubagentSessionsForSessions(ctx, []string{"second"})
			require.NoError(t, err)
			_, err = d.GetSessionExternalParent(ctx, "child")
			require.ErrorIs(t, err, sql.ErrNoRows)
			_, err = d.RestoreSession(ctx, "first")
			require.NoError(t, err)
			_, err = d.GetSessionExternalParent(ctx, "child")
			require.ErrorIs(t, err, sql.ErrNoRows)
			require.NoError(t, d.SoftDeleteSession(ctx, "first"))
			_, err = d.DeleteSessionIfTrashed(ctx, "first")
			require.NoError(t, err)
			link, err = d.GetSessionExternalParent(ctx, "child")
			require.NoError(t, err)
			assert.Equal(t, "second", link.ParentSessionID)
		})
	}
}

func TestLaunchParentUnknownEvidenceDoesNotCreateLink(t *testing.T) {
	for _, tc := range []struct{ name, parentMachine, childMachine, parentCwd, childCwd string }{
		{"unknown folder", "local", "local", "", "/repo"},
		{"unknown machine", "", "", "/repo", "/repo"},
		{"relative child folder", "local", "local", "/repo", "repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = tc.parentCwd; s.Machine = tc.parentMachine })
			insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = tc.childCwd
				s.Machine = tc.childMachine
				s.SessionKind = "non-interactive"
				s.StartedAt = &started
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			_, err = d.GetSessionExternalParent(t.Context(), "child")
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestLaunchParentUnusableEvidenceKeepsRecordedAndParserParents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parser bool
	}{{"recorded", false}, {"parser", true}} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "parent", "project")
			insertSession(t, d, "child", "project", func(s *Session) { s.Agent = "codex"; s.SessionKind = "non-interactive" })
			if tc.parser {
				_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET parser_parent_session_id='parent', parent_session_id='parent', relationship_type='subagent' WHERE id='child'`)
				require.NoError(t, err)
				_, err = d.getWriter().ExecContext(t.Context(), `INSERT INTO session_external_parents (session_id,parent_session_id,inferred) VALUES ('child','parent',1)`)
				require.NoError(t, err)
			} else {
				_, err := d.SetSessionExternalParent(t.Context(), "child", "parent")
				require.NoError(t, err)
			}
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			child, err := d.GetSession(t.Context(), "child")
			require.NoError(t, err)
			require.NotNil(t, child.ParentSessionID)
			assert.Equal(t, "parent", *child.ParentSessionID)
			_, err = d.GetSessionExternalParent(t.Context(), "child")
			if tc.parser {
				require.ErrorIs(t, err, sql.ErrNoRows)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLaunchParentUnknownMachineRetractsMatch(t *testing.T) {
	d := testDB(t)
	addLauncher := func(id, machine string) {
		insertSession(t, d, id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo"; s.Machine = machine })
		insertMessages(t, d, Message{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	}
	addLauncher("known", "local")
	started := "2026-01-02T03:04:03Z"
	insertSession(t, d, "child", "project", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo"
		s.Machine = "local"
		s.SessionKind = "non-interactive"
		s.StartedAt = &started
	})
	_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
	require.NoError(t, err)
	_, err = d.GetSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	addLauncher("unknown", "")
	_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"unknown"})
	require.NoError(t, err)
	_, err = d.GetSessionExternalParent(t.Context(), "child")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestLaunchParentWrapperAmbiguity(t *testing.T) {
	for _, tc := range []struct{ agent, literal, wrapper string }{
		{"codex", `codex exec hi`, `sudo "$BIN" exec`},
		{"claude", `claude -p hi`, `env "$BIN" -p`},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			d := testDB(t)
			for _, launch := range []struct{ id, command string }{{"literal", tc.literal}, {"wrapper", tc.wrapper}} {
				insertSession(t, d, launch.id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
				input, err := json.Marshal(map[string]string{"command": launch.command})
				require.NoError(t, err)
				insertMessages(t, d, Message{SessionID: launch.id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: launch.id, ToolName: "Bash", Category: "Bash", InputJSON: string(input)}}})
			}
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = tc.agent
				s.Cwd = "/repo"
				s.StartedAt = &started
				if tc.agent == "codex" {
					s.SessionKind = "non-interactive"
				} else {
					s.Entrypoint = "sdk-cli"
				}
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			_, err = d.GetSessionExternalParent(t.Context(), "child")
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestLaunchParentDynamicPrompt(t *testing.T) {
	for _, withCompetitor := range []bool{false, true} {
		t.Run(strconv.FormatBool(withCompetitor), func(t *testing.T) {
			d := testDB(t)
			add := func(id, cmd string) {
				insertSession(t, d, id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
				input, err := json.Marshal(map[string]string{"command": cmd})
				require.NoError(t, err)
				insertMessages(t, d, Message{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", InputJSON: string(input)}}})
			}
			add("dynamic", `codex exec "$PROMPT"`)
			if withCompetitor {
				add("other", `codex exec literal`)
			}
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(t.Context(), "child")
			if withCompetitor {
				require.ErrorIs(t, err, sql.ErrNoRows)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "dynamic", link.ParentSessionID)
			}
		})
	}
}

func TestLaunchParentInferredLinksDoNotCrossCopies(t *testing.T) {
	source := testDB(t)
	insertSession(t, source, "parent", "project")
	insertSession(t, source, "child", "project")
	_, err := source.SetSessionExternalParent(t.Context(), "child", "parent")
	require.NoError(t, err)
	_, err = source.getWriter().ExecContext(t.Context(), `UPDATE session_external_parents SET inferred=1 WHERE session_id='child'`)
	require.NoError(t, err)
	t.Run("orphan", func(t *testing.T) {
		dest := testDB(t)
		_, err := dest.CopyOrphanedDataFrom(t.Context(), source.Path())
		require.NoError(t, err)
		_, err = dest.GetSessionExternalParent(t.Context(), "child")
		require.ErrorIs(t, err, sql.ErrNoRows)
		child, err := dest.GetSession(t.Context(), "child")
		require.NoError(t, err)
		assert.Nil(t, child.ParentSessionID)
	})
	t.Run("metadata", func(t *testing.T) {
		dest := testDB(t)
		insertSession(t, dest, "parent", "project")
		insertSession(t, dest, "child", "project")
		require.NoError(t, dest.CopySessionMetadataFrom(t.Context(), source.Path()))
		_, err := dest.GetSessionExternalParent(t.Context(), "child")
		require.ErrorIs(t, err, sql.ErrNoRows)
	})
	t.Run("replacement", func(t *testing.T) {
		d := testDB(t)
		seedReplaceSession(t, d)
		insertSession(t, d, "manager", "project")
		_, err := d.SetSessionExternalParent(t.Context(), "replace", "manager")
		require.NoError(t, err)
		_, err = d.getWriter().ExecContext(t.Context(), `UPDATE session_external_parents SET inferred=1 WHERE session_id='replace'`)
		require.NoError(t, err)
		copyID, err := d.ReplaceSessionKeepingTrashedCopy(t.Context(), replaceWrite("new"))
		require.NoError(t, err)
		_, err = d.GetSessionExternalParent(t.Context(), copyID)
		require.ErrorIs(t, err, sql.ErrNoRows)
		copied, err := d.GetSessionFull(t.Context(), copyID)
		require.NoError(t, err)
		assert.Nil(t, copied.ParentSessionID)
	})
}

func TestLaunchParentBackfillCancellationRetries(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	started := "2026-01-02T03:04:03Z"
	insertSession(t, d, "child", "project", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo"
		s.StartedAt = &started
		s.SessionKind = "non-interactive"
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn, err := d.getWriter().Conn(t.Context())
	require.NoError(t, err)
	var rawConn *sqlite3.SQLiteConn
	require.NoError(t, conn.Raw(func(raw any) error {
		rawConn = raw.(*sqlite3.SQLiteConn)
		rawConn.RegisterAuthorizer(func(op int, table, _, _ string) int {
			if op == sqlite3.SQLITE_INSERT && table == "session_external_parents" {
				cancel()
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
	_, err = d.LinkSubagentSessionsContext(ctx)
	require.Error(t, err)
	rawConn.RegisterAuthorizer(nil)
	state, err := d.GetSyncState(t.Context(), launchBackfillKey)
	require.NoError(t, err)
	assert.Empty(t, state)
	_, err = d.LinkSubagentSessionsContext(t.Context())
	require.NoError(t, err)
	link, err := d.GetSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	assert.Equal(t, "parent", link.ParentSessionID)
}

func TestLaunchParentClearSurvivesCopies(t *testing.T) {
	t.Run("retained", func(t *testing.T) {
		source := testDB(t)
		insertSession(t, source, "parent", "project")
		insertSession(t, source, "child", "project")
		_, err := source.SetSessionExternalParent(t.Context(), "child", "parent")
		require.NoError(t, err)
		_, err = source.ClearSessionExternalParent(t.Context(), "child")
		require.NoError(t, err)
		dest := testDB(t)
		_, err = dest.CopyOrphanedDataFrom(t.Context(), source.Path())
		require.NoError(t, err)
		var n int
		require.NoError(t, dest.getReader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_parent_inference_blocks WHERE session_id='child'`).Scan(&n))
		assert.Equal(t, 1, n)
	})
	t.Run("replacement", func(t *testing.T) {
		d := testDB(t)
		seedReplaceSession(t, d)
		insertSession(t, d, "parent", "project")
		_, err := d.SetSessionExternalParent(t.Context(), "replace", "parent")
		require.NoError(t, err)
		_, err = d.ClearSessionExternalParent(t.Context(), "replace")
		require.NoError(t, err)
		copyID, err := d.ReplaceSessionKeepingTrashedCopy(t.Context(), replaceWrite("new"))
		require.NoError(t, err)
		var n int
		require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_parent_inference_blocks WHERE session_id=?`, copyID).Scan(&n))
		assert.Equal(t, 1, n)
	})
}

func TestLaunchParentShellOrAndCodeMode(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		linked      bool
	}{
		{"short circuit JavaScript", `false && await tools.exec_command({cmd:"codex exec hi",workdir:"/repo"})`, false},
		{"uncalled function", `function f() { return tools.exec_command({cmd:"codex exec hi",workdir:"/repo"}) }`, false},
		{"uncalled method", `const x = { f() { return tools.exec_command({cmd:"codex exec hi",workdir:"/repo"}) } }`, false},
		{"multiple JavaScript calls", `await Promise.all([tools.exec_command({cmd:"echo hello"}), tools.exec_command({cmd:"codex exec hi",workdir:"/repo"})])`, true},
		{"computed shell call", `await tools["exec_command"]({cmd:"codex exec hi",workdir:"/repo"})`, true},
		{"separate workdirs", `await Promise.all([tools.exec_command({cmd:"echo hello",workdir:"/other"}), tools.exec_command({cmd:"codex exec hi"})])`, true},
		{"literal templates", "await Promise.all([tools.exec_command({cmd:`echo hello`}), tools.exec_command({cmd:`codex exec hi`,workdir:`/repo`})])", true},
		{"dynamic template", "await tools.exec_command({cmd:`codex exec ${prompt}`,workdir:`/repo`})", false},
		{"conditional alongside certain", `if (false) { await tools.exec_command({cmd:"echo hi"}) }; await tools.exec_command({cmd:"codex exec hi",workdir:"/repo"})`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "codex"; s.Cwd = "/repo" })
			insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "exec", Category: "Bash", InputJSON: tc.input}}})
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(t.Context(), "child")
			if tc.linked {
				require.NoError(t, err)
				assert.Equal(t, "parent", link.ParentSessionID)
			} else {
				require.ErrorIs(t, err, sql.ErrNoRows)
			}
		})
	}
}

func TestLaunchParentCodeModeEmptyAndUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		linked      bool
	}{
		{"valid cell without shell call", `text(await tools.clock__curr_time({}))`, true},
		{"unreadable cell", `if (`, false},
		{"dynamic tool reference", `await tools[name]({cmd:"codex exec hi"})`, false},
		{"aliased shell reference", `const run = tools.exec_command; await run({cmd:"codex exec hi"})`, false},
		{"other computed tool", `await tools["clock__curr_time"]({})`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			for _, launcher := range []struct{ id, input string }{{"launcher", `{"command":"codex exec hi"}`}, {"other", tc.input}} {
				insertSession(t, d, launcher.id, "project", func(s *Session) { s.Agent = "codex"; s.Cwd = "/repo" })
				insertMessages(t, d, Message{SessionID: launcher.id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: launcher.id, ToolName: "exec", Category: "Bash", InputJSON: launcher.input}}})
			}
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(t.Context(), "child")
			if tc.linked {
				require.NoError(t, err)
				assert.Equal(t, "launcher", link.ParentSessionID)
			} else {
				require.ErrorIs(t, err, sql.ErrNoRows)
			}
		})
	}
}

func TestLaunchParentPollsAreNotCompetitors(t *testing.T) {
	for _, tc := range []struct {
		name, tool, input string
		linked            bool
	}{
		{"write stdin", "write_stdin", `{"session_id":1}`, true},
		{"wait", "wait", `{"cell_id":"cell"}`, true},
		{"unreadable non-poll", "Bash", `{`, false},
		{"empty non-poll", "Bash", ``, false},
		{"empty structured non-poll", "Bash", `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			for _, launcher := range []struct{ id, tool, input string }{{"launcher", "Bash", `{"command":"codex exec hi"}`}, {"other", tc.tool, tc.input}} {
				insertSession(t, d, launcher.id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
				insertMessages(t, d, Message{SessionID: launcher.id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: launcher.id, ToolName: launcher.tool, Category: "Bash", InputJSON: launcher.input}}})
			}
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(t.Context(), "child")
			if tc.linked {
				require.NoError(t, err)
				assert.Equal(t, "launcher", link.ParentSessionID)
			} else {
				require.ErrorIs(t, err, sql.ErrNoRows)
			}
		})
	}
}

func TestLaunchParentEagerBatches(t *testing.T) {
	for _, method := range []string{"all", "allSettled"} {
		t.Run(method, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "codex"; s.Cwd = "/repo" })
			input := `await Promise.` + method + `([tools.exec_command({cmd:"codex exec one",workdir:"/repo"}),tools.exec_command({cmd:"codex exec two",workdir:"/other"})])`
			insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "exec", Category: "Bash", InputJSON: input}}})
			started := "2026-01-02T03:04:03Z"
			for _, child := range []struct{ id, cwd string }{{"first", "/repo"}, {"second", "/other"}} {
				insertSession(t, d, child.id, "project", func(s *Session) {
					s.Agent = "codex"
					s.Cwd = child.cwd
					s.StartedAt = &started
					s.SessionKind = "non-interactive"
				})
			}
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"first", "second"})
			require.NoError(t, err)
			for _, id := range []string{"first", "second"} {
				link, err := d.GetSessionExternalParent(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, "parent", link.ParentSessionID)
			}
		})
	}
}

func TestLaunchParentCDGuard(t *testing.T) {
	for _, tc := range []struct {
		name, command, cwd string
		other, linked      bool
	}{
		{"matching guarded directory", `cd /repo && codex exec hi`, "/repo", false, true},
		{"other directory", `cd /repo && codex exec hi`, "/other", false, false},
		{"other directory is not blocked", `cd /repo && codex exec hi`, "/other", true, true},
		{"outer unknown guard", `test -d /repo && cd /repo && codex exec hi`, "/repo", false, false},
		{"changed codex directory", `cd /repo && codex -C /other exec hi`, "/other", false, false},
		{"unchecked directory", `cd /repo; codex exec hi`, "/repo", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			add := func(id, cwd, command string) {
				insertSession(t, d, id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = cwd })
				input, err := json.Marshal(map[string]string{"command": command})
				require.NoError(t, err)
				insertMessages(t, d, Message{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", InputJSON: string(input)}}})
			}
			add("guarded", "/start", tc.command)
			if tc.other {
				add("other", tc.cwd, "codex exec hi")
			}
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = tc.cwd
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(t.Context(), "child")
			if tc.linked {
				require.NoError(t, err)
				if tc.other {
					assert.Equal(t, "other", link.ParentSessionID)
				} else {
					assert.Equal(t, "guarded", link.ParentSessionID)
				}
			} else {
				require.ErrorIs(t, err, sql.ErrNoRows)
			}
		})
	}
}

func TestLaunchParentBatchOtherAgentDoesNotBlock(t *testing.T) {
	d := testDB(t)
	for _, launch := range []struct{ id, input string }{{"actual", `{"command":"codex exec hi"}`}, {"other", `Promise.all([tools.exec_command({cmd:"claude -p hi"}),tools.exec_command({cmd:"pwd"})])`}} {
		insertSession(t, d, launch.id, "project", func(s *Session) { s.Agent = "codex"; s.Cwd = "/repo" })
		insertMessages(t, d, Message{SessionID: launch.id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: launch.id, ToolName: "exec", Category: "Bash", InputJSON: launch.input}}})
	}
	started := "2026-01-02T03:04:03Z"
	insertSession(t, d, "child", "project", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo"
		s.StartedAt = &started
		s.SessionKind = "non-interactive"
	})
	_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
	require.NoError(t, err)
	link, err := d.GetSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	assert.Equal(t, "actual", link.ParentSessionID)
}

func TestLaunchParentSharedLiteralDecoder(t *testing.T) {
	for _, input := range []string{
		`text(await tools.exec_command({cmd:"git commit -m \"fix\""}))`,
		`text(await tools.exec_command({cmd:'git commit -m fix'}))`,
		"text(await tools.exec_command({cmd:`git commit -m fix`}))",
	} {
		t.Run(input, func(t *testing.T) {
			calls, undecodable := shellCalls(input)
			require.False(t, undecodable)
			require.Len(t, calls, 1)
			assert.Equal(t, commitCommandText(input), calls[0].command)
		})
	}
	input := "text(await tools.exec_command({cmd:`git commit -m ${message}`}))"
	assert.Empty(t, commitCommandText(input))
	calls, undecodable := shellCalls(input)
	require.False(t, undecodable)
	require.Len(t, calls, 1)
	assert.True(t, calls[0].unknown)
	batch := `Promise.all([tools.exec_command({cmd:"git commit -m fix"}),tools.exec_command({cmd:"pwd"})])`
	assert.Empty(t, commitCommandText(batch))
}

func TestLaunchParentCodeModeInitializer(t *testing.T) {
	for _, input := range []string{
		`const p = tools.exec_command({cmd:"codex exec hi",workdir:"/repo"})`,
		`let p = await tools.exec_command({cmd:"codex exec hi",workdir:"/repo"})`,
		`var p = Promise.all([tools.exec_command({cmd:"codex exec hi",workdir:"/repo"}),tools.exec_command({cmd:"pwd"})])`,
	} {
		t.Run(input, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "codex"; s.Cwd = "/repo" })
			insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "exec", Category: "Bash", InputJSON: input}}})
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(t.Context(), "child")
			require.NoError(t, err)
			assert.Equal(t, "parent", link.ParentSessionID)
		})
	}
}

func TestLaunchParentCycleAndMirrorMarker(t *testing.T) {
	seed := func(t *testing.T, d *DB) {
		t.Helper()
		insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
		insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
		started := "2026-01-02T03:04:03Z"
		insertSession(t, d, "child", "project", func(s *Session) {
			s.Agent = "codex"
			s.Cwd = "/repo"
			s.StartedAt = &started
			s.SessionKind = "non-interactive"
		})
	}
	t.Run("cycle", func(t *testing.T) {
		d := testDB(t)
		seed(t, d)
		_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET parent_session_id='child',parser_parent_session_id='child',relationship_type='subagent' WHERE id='parent'`)
		require.NoError(t, err)
		_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
		require.NoError(t, err)
		link, err := d.GetSessionExternalParent(t.Context(), "child")
		require.NoError(t, err)
		assert.False(t, link.Applied)
		child, err := d.GetSession(t.Context(), "child")
		require.NoError(t, err)
		assert.Nil(t, child.ParentSessionID)
	})
	t.Run("mirror marker", func(t *testing.T) {
		d := testDB(t)
		seed(t, d)
		reset := func() {
			_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET sync_marker='2000-01-01T00:00:00Z' WHERE id='child'`)
			require.NoError(t, err)
		}
		check := func() {
			var marker string
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT sync_marker FROM sessions WHERE id='child'`).Scan(&marker))
			assert.Greater(t, marker, "2000-01-01T00:00:00Z")
		}
		reset()
		_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
		require.NoError(t, err)
		check()
		insertSession(t, d, "competitor", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
		reset()
		insertMessages(t, d, Message{SessionID: "competitor", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "competitor", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
		check()
		_, err = d.GetSessionExternalParent(t.Context(), "child")
		require.ErrorIs(t, err, sql.ErrNoRows)
	})
}

func TestLaunchParentLateTranscriptWrite(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	started := "2026-01-02T03:04:03Z"
	insertSession(t, d, "child", "project", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo"
		s.StartedAt = &started
		s.SessionKind = "non-interactive"
	})
	_, err := d.LinkSubagentSessionsContext(t.Context())
	require.NoError(t, err)
	message := Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "parent", []Message{message}))
	parent, err := d.GetSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	assert.Equal(t, "parent", parent.ParentSessionID)
}

func TestLaunchParentAmbiguousRuns(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	started := "2026-01-02T03:04:03Z"
	addRun := func(id string) {
		insertSession(t, d, id, "project", func(s *Session) {
			s.Agent = "codex"
			s.Cwd = "/repo"
			s.StartedAt = &started
			s.SessionKind = "non-interactive"
		})
	}
	addRun("one")
	_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"one"})
	require.NoError(t, err)
	_, err = d.GetSessionExternalParent(t.Context(), "one")
	require.NoError(t, err)
	addRun("two")
	_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"two"})
	require.NoError(t, err)
	for _, id := range []string{"one", "two"} {
		_, err := d.GetSessionExternalParent(t.Context(), id)
		require.ErrorIs(t, err, sql.ErrNoRows)
	}
	insertMessages(t, d, Message{SessionID: "parent", Ordinal: 1, Role: "assistant", Timestamp: "2026-01-02T03:04:01Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"parent"})
	require.NoError(t, err)
	for _, id := range []string{"one", "two"} {
		parent, err := d.GetSessionExternalParent(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, "parent", parent.ParentSessionID)
	}
	require.NoError(t, d.SoftDeleteSession(t.Context(), "two"))
	_, err = d.GetSessionExternalParent(t.Context(), "one")
	require.NoError(t, err)
	require.NoError(t, d.DeleteSession(t.Context(), "two"))
	_, err = d.GetSessionExternalParent(t.Context(), "one")
	require.NoError(t, err)
}

func TestLaunchParentSeedCandidateFiltering(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{`{"command":"pwd"}`, false},
		{`{"command":"cd /repo && codex exec hi"}`, true},
		{`{"command":"bash -c \"$COMMAND\""}`, true},
		{`{`, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, launchInputMayMatter(sql.NullString{String: tc.input, Valid: true}))
		})
	}
}
