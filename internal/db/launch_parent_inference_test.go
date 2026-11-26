package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
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
	_, err = d.SetSessionExternalParent(t.Context(), "child", "parent")
	require.NoError(t, err)
	var inferred bool
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT inferred FROM session_external_parents WHERE session_id='child'`).Scan(&inferred))
	assert.False(t, inferred)
	_, err = d.ClearSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
	require.NoError(t, err)
	_, err = d.GetSessionExternalParent(t.Context(), "child")
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = d.SetSessionExternalParent(t.Context(), "child", "parent")
	require.NoError(t, err)
	launch("other", `{"command":"codex exec < prompt.md"}`)
	parent, err = d.GetSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	assert.Equal(t, "parent", parent.ParentSessionID)
}

func TestLaunchParentEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, input, tool, cwd, machine, childCwd, childMachine string
		other                                                   bool
		want                                                    string
	}{
		{"possible alone", `{"command":"test -d /repo && codex exec hi"}`, "Bash", "/repo", "local", "/repo", "local", false, "candidate"},
		{"possible competitor", `{"command":"test -d /repo && codex exec hi"}`, "Bash", "/repo", "local", "/repo", "local", true, ""},
		{"dead competitor", `{"command":"false && codex exec hi"}`, "Bash", "/repo", "local", "/repo", "local", true, "actual"},
		{"missing folder", `{"command":"codex exec hi"}`, "Bash", "", "local", "/repo", "local", false, ""},
		{"two folders", `{"command":"cd /missing; codex exec hi"}`, "Bash", "/repo", "local", "/repo", "local", true, ""},
		{"unknown directory", `{"command":"codex -C \"$DIR\" exec hi"}`, "Bash", "/repo", "local", "/repo", "local", true, ""},
		{"unknown code workdir", `text(await tools.exec_command({cmd: "codex exec hi", workdir: dir}))`, "exec", "/repo", "local", "/repo", "local", true, ""},
		{"guarded code call", `flag && tools.exec_command({cmd: "codex exec hi"})`, "exec", "/repo", "local", "/repo", "local", true, ""},
		{"unreadable", `{`, "Bash", "/repo", "local", "/repo", "local", true, ""},
		{"empty", ``, "Bash", "/repo", "local", "/repo", "local", true, ""},
		{"empty object", `{}`, "Bash", "/repo", "local", "/repo", "local", true, "actual"},
		{"other agent in batch", `Promise.all([tools.exec_command({cmd:"claude -p hi"}),tools.exec_command({cmd:"pwd"})])`, "exec", "/repo", "local", "/repo", "local", true, "actual"},
		{"different folder", `{"command":"cd /repo && codex exec hi"}`, "Bash", "/start", "local", "/other", "local", true, "actual"},
		{"blank machine", `{"command":"codex exec hi"}`, "Bash", "/repo", "", "/repo", "local", true, ""},
		{"two launchers", `{"command":"codex exec hi"}`, "Bash", "/repo", "local", "/repo", "local", true, ""},
		{"guarded directory", `{"command":"cd /repo && codex exec hi"}`, "Bash", "/start", "local", "/repo", "local", false, "candidate"},
		{"blank child machine", `{"command":"codex exec hi"}`, "Bash", "/repo", "", "/repo", "", false, ""},
		{"argv cmd.exe Windows folder", `{"command":["cmd.exe","/c","codex","exec","hi"]}`, "Bash", `C:\Users\example\project`, "local", `C:\Users\example\project`, "local", false, "candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			add := func(id, cwd, machine, tool, input string) {
				insertSession(t, d, id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = cwd; s.Machine = machine })
				insertMessages(t, d, Message{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: id, ToolName: tool, Category: "Bash", InputJSON: input}}})
			}
			add("candidate", tc.cwd, tc.machine, tc.tool, tc.input)
			if tc.other {
				add("actual", tc.childCwd, "local", "Bash", `{"command":"codex exec hi"}`)
			}
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = tc.childCwd
				s.Machine = tc.childMachine
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(t.Context(), "child")
			if tc.want == "" {
				require.ErrorIs(t, err, sql.ErrNoRows)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, link.ParentSessionID)
			}
		})
	}
}

func TestLaunchParentBatchWorkdirs(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "codex"; s.Cwd = "/repo" })
	input := `await Promise.all([tools.exec_command({cmd:"codex exec one",workdir:"/repo"}),tools.exec_command({cmd:"codex exec two",workdir:"/other"})])`
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
}

func TestLaunchParentCopyEvidence(t *testing.T) {
	for _, state := range []string{"inferred", "cleared"} {
		for _, method := range []string{"orphan", "metadata", "replacement"} {
			t.Run(state+"/"+method, func(t *testing.T) {
				source := testDB(t)
				id := "child"
				if method == "replacement" {
					seedReplaceSession(t, source)
					id = "replace"
				} else {
					insertSession(t, source, id, "project")
				}
				_, err := source.getWriter().ExecContext(t.Context(), `UPDATE sessions SET agent='codex', session_kind='non-interactive', cwd='/repo', started_at='2026-01-02T03:04:03Z' WHERE id=?`, id)
				require.NoError(t, err)
				insertSession(t, source, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
				insertMessages(t, source, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
				_, err = source.GetSessionExternalParent(t.Context(), id)
				require.NoError(t, err)
				if state == "cleared" {
					_, err = source.ClearSessionExternalParent(t.Context(), id)
				}
				require.NoError(t, err)
				dest := source
				copyID := id
				switch method {
				case "orphan":
					dest = testDB(t)
					_, err = dest.CopyOrphanedDataFrom(t.Context(), source.Path())
				case "metadata":
					dest = testDB(t)
					insertSession(t, dest, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
					started := "2026-01-02T03:04:03Z"
					insertSession(t, dest, id, "project", func(s *Session) {
						s.Agent = "codex"
						s.Cwd = "/repo"
						s.SessionKind = "non-interactive"
						s.StartedAt = &started
					})
					if state == "cleared" {
						insertMessages(t, dest, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
						_, err = dest.GetSessionExternalParent(t.Context(), id)
						require.NoError(t, err)
					}
					err = dest.CopySessionMetadataFrom(t.Context(), source.Path())
				case "replacement":
					copyID, err = dest.ReplaceSessionKeepingTrashedCopy(t.Context(), replaceWrite("new"))
				}
				require.NoError(t, err)
				_, err = dest.GetSessionExternalParent(t.Context(), copyID)
				require.ErrorIs(t, err, sql.ErrNoRows)
				var blocks int
				require.NoError(t, dest.getReader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_parent_inference_blocks WHERE session_id=?`, copyID).Scan(&blocks))
				assert.Equal(t, state == "cleared", blocks == 1)
				child, err := dest.GetSessionFull(t.Context(), copyID)
				require.NoError(t, err)
				assert.Nil(t, child.ParentSessionID)
			})
		}
	}
}

func TestLaunchParentBackfillCheckpoint(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "first-parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/first" })
	insertMessages(t, d, Message{SessionID: "first-parent", Ordinal: 0, Role: "assistant", Timestamp: "2025-12-31T03:04:00Z", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	tx, err := d.getWriter().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for i := range 1001 {
		started := "2026-01-01T03:04:03Z"
		cwd := "/repo"
		if i == 0 {
			started = "2025-12-31T03:04:03Z"
			cwd = "/first"
		}
		if i == 1000 {
			started = "2026-01-02T03:04:03Z"
		}
		_, err = tx.ExecContext(t.Context(), `INSERT INTO sessions (id,project,agent,session_kind,cwd,started_at) VALUES (?, 'project', 'codex', 'non-interactive', ?, ?)`, fmt.Sprintf("child-%04d", i), cwd, started)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	_, err = d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET source_missing_at='2026-01-03T00:00:00Z' WHERE id='child-1000'`)
	require.NoError(t, err)
	tx, err = d.getWriter().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = backfillLaunchParentsTx(t.Context(), tx)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	state, err := d.GetSyncState(t.Context(), launchBackfillKey)
	require.NoError(t, err)
	assert.NotEmpty(t, state)
	assert.NotEqual(t, "done", state)
	first, err := d.GetSessionExternalParent(t.Context(), "child-0000")
	require.NoError(t, err)
	assert.Equal(t, "first-parent", first.ParentSessionID)
	_, err = d.LinkSubagentSessionsContext(t.Context())
	require.NoError(t, err)
	state, err = d.GetSyncState(t.Context(), launchBackfillKey)
	require.NoError(t, err)
	assert.Equal(t, "done", state)
	first, err = d.GetSessionExternalParent(t.Context(), "child-0000")
	require.NoError(t, err)
	assert.Equal(t, "first-parent", first.ParentSessionID)
	parent, err := d.GetSessionExternalParent(t.Context(), "child-1000")
	require.NoError(t, err)
	assert.Equal(t, "parent", parent.ParentSessionID)
}

func TestLaunchParentBulkImportBackfillsAfterIndexRebuild(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.DropBulkImportIndexes(t.Context()))
	var exists int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_activity_timestamp'`).Scan(&exists))
	assert.Zero(t, exists)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	started := "2026-01-02T03:04:03Z"
	insertSession(t, d, "child", "project", func(s *Session) {
		s.Agent = "codex"
		s.Cwd = "/repo"
		s.StartedAt = &started
		s.SessionKind = "non-interactive"
	})
	_, err := d.GetSessionExternalParent(t.Context(), "child")
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, d.RebuildBulkImportIndexes(t.Context()))
	_, err = d.LinkSubagentSessionsContext(t.Context())
	require.NoError(t, err)
	parent, err := d.GetSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	assert.Equal(t, "parent", parent.ParentSessionID)
}

func TestLaunchParentAgentPairs(t *testing.T) {
	for _, parentAgent := range []string{"claude", "codex"} {
		for _, childAgent := range []string{"claude", "codex"} {
			t.Run(parentAgent+"-"+childAgent, func(t *testing.T) {
				d := testDB(t)
				insertSession(t, d, "launcher", "project", func(s *Session) { s.Agent = parentAgent; s.Cwd = "/repo" })
				input := `{"cmd":"codex exec hi","workdir":"/repo"}`
				if childAgent == "claude" {
					input = `{"cmd":"claude -p hi","workdir":"/repo"}`
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

func TestLaunchParentFolderMachineAndWindow(t *testing.T) {
	for _, tc := range []struct {
		name, launcherCwd, input, callCwd, cwd, machine, started string
		want                                                     bool
	}{
		{"missing call directory", "/later", `{"command":"codex exec hi"}`, "", "/later", "local", "2026-01-02T03:04:03Z", false},
		{"recorded original directory", "/later", `{"command":"codex exec hi"}`, "/original", "/original", "local", "2026-01-02T03:04:03Z", true},
		{"recorded directory differs from session", "/later", `{"command":"codex exec hi"}`, "/original", "/later", "local", "2026-01-02T03:04:03Z", false},
		{"explicit original directory", "/later", `{"command":"codex exec hi","workdir":"/original"}`, "", "/original", "local", "2026-01-02T03:04:03Z", true},
		{"matching Windows path", `C:\repo`, `{"cmd":"codex.exe exec hi", "workdir":"C:\\repo"}`, "", `c:\repo`, "local", "2026-01-02T03:05:00.250Z", true},
		{"Git Bash path", `C:\repo`, `{"cmd":"codex.exe exec hi", "workdir":"C:\\repo"}`, "", `/c/repo`, "local", "2026-01-02T03:04:03Z", true},
		{"other folder", `C:\repo`, `{"cmd":"codex.exe exec hi", "workdir":"C:\\repo"}`, "", `c:\elsewhere`, "local", "2026-01-02T03:04:03Z", false},
		{"other machine", `C:\repo`, `{"cmd":"codex.exe exec hi", "workdir":"C:\\repo"}`, "", `c:\repo`, "other", "2026-01-02T03:04:03Z", false},
		{"outside window", `C:\repo`, `{"cmd":"codex.exe exec hi", "workdir":"C:\\repo"}`, "", `c:\repo`, "local", "2026-01-02T03:05:00.251Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "launcher", "project", func(s *Session) { s.Agent = "codex"; s.Cwd = tc.launcherCwd })
			insertMessages(t, d, Message{SessionID: "launcher", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00.250Z", ToolCalls: []ToolCall{{SessionID: "launcher", ToolName: "exec_command", Category: "Bash", InputJSON: tc.input, WorkingDirectory: tc.callCwd}}})
			insertSession(t, d, "run", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = tc.cwd
				s.Machine = tc.machine
				s.StartedAt = &tc.started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"run"})
			require.NoError(t, err)
			parent, err := d.GetSessionExternalParent(t.Context(), "run")
			if tc.want {
				require.NoError(t, err)
				assert.Equal(t, "launcher", parent.ParentSessionID)
			} else {
				require.ErrorIs(t, err, sql.ErrNoRows)
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

func TestLaunchParentTrashEvidence(t *testing.T) {
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
	require.NoError(t, d.SoftDeleteSession(ctx, "first"))
	link, err = d.GetSessionExternalParent(ctx, "child")
	require.NoError(t, err)
	assert.Equal(t, "first", link.ParentSessionID)
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

func TestLaunchParentReplacementWritePaths(t *testing.T) {
	for _, mode := range []string{"messages", "late", "content", "incremental", "batch", "atomic batch", "staged", "interactive", "competitor"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
			message := Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}}
			if mode != "late" {
				insertMessages(t, d, message)
			}
			started := "2026-01-02T03:04:03Z"
			insertSession(t, d, "child", "project", func(s *Session) {
				s.Agent = "codex"
				s.Cwd = "/repo"
				s.StartedAt = &started
				s.SessionKind = "non-interactive"
			})
			_, err := d.LinkSubagentSessionsContext(t.Context())
			require.NoError(t, err)
			if mode != "late" {
				_, err = d.GetSessionExternalParent(t.Context(), "child")
				require.NoError(t, err)
			}
			_, err = d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET sync_marker='2000-01-01T00:00:00Z' WHERE id='child'`)
			require.NoError(t, err)
			switch mode {
			case "interactive":
				_, err = d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET session_kind='' WHERE id='child'`)
				require.NoError(t, err)
				_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
			case "competitor":
				insertSession(t, d, "competitor", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
				message.SessionID = "competitor"
				insertMessages(t, d, message)
			case "messages":
				err = d.ReplaceSessionMessages(t.Context(), "parent", nil)
			case "late":
				err = d.ReplaceSessionMessages(t.Context(), "parent", []Message{message})
			case "content":
				err = d.ReplaceSessionContent(t.Context(), "parent", nil, SessionSignalUpdate{}, nil)
			case "incremental":
				from := 0
				_, err = d.WriteSessionIncremental(t.Context(), "parent", nil, IncrementalSessionUpdate{ReplaceFromOrdinal: &from})
			case "batch", "atomic batch":
				session, getErr := d.GetSession(t.Context(), "parent")
				require.NoError(t, getErr)
				write := SessionBatchWrite{Session: *session, ReplaceMessages: true}
				if mode == "batch" {
					_, err = d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
				} else {
					_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
				}
			case "staged":
				err = d.ReplaceSessionContentStaged(t.Context(), "parent", nil, newScratchStagedResults(t), nil, nil)
			}
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(t.Context(), "child")
			if mode == "late" {
				require.NoError(t, err)
				assert.Equal(t, "parent", link.ParentSessionID)
			} else {
				require.ErrorIs(t, err, sql.ErrNoRows)
				child, getErr := d.GetSession(t.Context(), "child")
				require.NoError(t, getErr)
				assert.Nil(t, child.ParentSessionID)
			}
			var marker string
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT sync_marker FROM sessions WHERE id='child'`).Scan(&marker))
			assert.Greater(t, marker, "2000-01-01T00:00:00Z")
		})
	}
}

func TestLaunchParentSequentialRuns(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	second := "other"
	insertSession(t, d, second, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: second, Ordinal: 1, Role: "assistant", Timestamp: "2026-01-02T03:04:30Z", ToolCalls: []ToolCall{{SessionID: second, ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	for _, child := range []struct{ id, started string }{{"one", "2026-01-02T03:04:03Z"}, {"two", "2026-01-02T03:04:33Z"}} {
		insertSession(t, d, child.id, "project", func(s *Session) {
			s.Agent = "codex"
			s.Cwd = "/repo"
			s.StartedAt = &child.started
			s.SessionKind = "non-interactive"
		})
	}
	_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"one", "two"})
	require.NoError(t, err)
	parent, err := d.GetSessionExternalParent(t.Context(), "one")
	require.NoError(t, err)
	assert.Equal(t, "parent", parent.ParentSessionID)
	parent, err = d.GetSessionExternalParent(t.Context(), "two")
	require.NoError(t, err)
	assert.Equal(t, second, parent.ParentSessionID)
}

func TestLaunchParentEarlierRunWithTwoCallsDoesNotReserveEither(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"a", "b"} {
		insertSession(t, d, id, "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	}
	for _, call := range []struct {
		id, stamp string
		ordinal   int
	}{
		{"a", "2026-01-02T03:03:10Z", 0},
		{"a", "2026-01-02T03:04:00Z", 1},
		{"b", "2026-01-02T03:04:30Z", 0},
	} {
		insertMessages(t, d, Message{SessionID: call.id, Ordinal: call.ordinal, Role: "assistant", Timestamp: call.stamp, ToolCalls: []ToolCall{{SessionID: call.id, ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}})
	}
	for _, child := range []struct{ id, started string }{{"one", "2026-01-02T03:04:03Z"}, {"two", "2026-01-02T03:04:33Z"}} {
		insertSession(t, d, child.id, "project", func(s *Session) {
			s.Agent = "codex"
			s.Cwd = "/repo"
			s.StartedAt = &child.started
			s.SessionKind = "non-interactive"
		})
	}
	_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"one", "two"})
	require.NoError(t, err)
	_, err = d.GetSessionExternalParent(t.Context(), "two")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestLaunchParentIncrementalAppend(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	first := Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}}
	insertMessages(t, d, first)
	for _, child := range []struct{ id, started string }{{"one", "2026-01-02T03:04:03Z"}, {"two", "2026-01-02T03:04:33Z"}} {
		insertSession(t, d, child.id, "project", func(s *Session) {
			s.Agent = "codex"
			s.Cwd = "/repo"
			s.StartedAt = &child.started
			s.SessionKind = "non-interactive"
		})
	}
	_, err := d.LinkSubagentSessionsForSessions(t.Context(), []string{"one", "two"})
	require.NoError(t, err)
	for _, id := range []string{"one", "two"} {
		_, err := d.GetSessionExternalParent(t.Context(), id)
		require.ErrorIs(t, err, sql.ErrNoRows)
	}
	second := Message{SessionID: "parent", Ordinal: 1, Role: "assistant", Timestamp: "2026-01-02T03:04:30Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}}
	_, err = d.WriteSessionIncremental(t.Context(), "parent", []Message{second}, IncrementalSessionUpdate{})
	require.NoError(t, err)
	for _, id := range []string{"one", "two"} {
		parent, err := d.GetSessionExternalParent(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, "parent", parent.ParentSessionID)
	}
}

func TestLaunchParentMixedBatchAppend(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "history", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "history", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-01T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "history", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec old"}`}}})
	for _, child := range []struct{ id, cwd string }{{"first", "/repo"}, {"second", "/other"}} {
		started := "2026-01-02T03:04:03Z"
		insertSession(t, d, child.id, "project", func(s *Session) {
			s.Agent = "codex"
			s.Cwd = child.cwd
			s.StartedAt = &started
			s.SessionKind = "non-interactive"
		})
	}
	history, err := d.GetSession(t.Context(), "history")
	require.NoError(t, err)
	launch := func(id string, ordinal int) Message {
		return Message{SessionID: id, Ordinal: ordinal, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`}}}
	}
	newSession := Session{ID: "new", Project: "project", Machine: history.Machine, Agent: "claude", Cwd: "/other"}
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{
		{Session: *history, Messages: []Message{launch("history", 1)}},
		{Session: newSession, Messages: []Message{launch("new", 0)}},
	})
	require.NoError(t, err)
	for _, pair := range []struct{ child, parent string }{{"first", "history"}, {"second", "new"}} {
		link, err := d.GetSessionExternalParent(t.Context(), pair.child)
		require.NoError(t, err)
		assert.Equal(t, pair.parent, link.ParentSessionID)
	}
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
