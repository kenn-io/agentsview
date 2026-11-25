package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
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
	launch("other", `{"command":"codex exec < prompt.md"}`)
	_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"other"})
	require.NoError(t, err)
	_, err = d.GetSessionExternalParent(t.Context(), "child")
	assert.Error(t, err)
	_, err = d.SetSessionExternalParent(t.Context(), "child", "parent")
	require.NoError(t, err)
	_, err = d.ClearSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	_, err = d.LinkSubagentSessionsForSessions(t.Context(), []string{"child"})
	require.NoError(t, err)
	_, err = d.GetSessionExternalParent(t.Context(), "child")
	assert.Error(t, err)
	_, err = d.SetSessionExternalParent(t.Context(), "child", "other")
	require.NoError(t, err)
	parent, err = d.GetSessionExternalParent(t.Context(), "child")
	require.NoError(t, err)
	assert.Equal(t, "other", parent.ParentSessionID)
}

func TestLaunchParentBackfillPages(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec review"}`}}})
	tx, err := d.getWriter().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for i := 0; i < 3001; i++ {
		_, err = tx.ExecContext(t.Context(), `INSERT INTO sessions (id,project,agent,session_kind,cwd,started_at) VALUES (?, 'project', 'codex', 'non-interactive', '/repo', '2026-01-02T03:04:03Z')`, fmt.Sprintf("child-%04d", i))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	_, err = d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET source_missing_at='2026-01-03T00:00:00Z' WHERE id='child-0000'`)
	require.NoError(t, err)
	for i := 0; i < 1; i++ {
		_, err = d.LinkSubagentSessionsContext(t.Context())
		require.NoError(t, err)
	}
	state, err := d.GetSyncState(t.Context(), launchBackfillKey)
	require.NoError(t, err)
	assert.Equal(t, "done", state)
	var linked int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE parent_session_id = 'parent'`).Scan(&linked))
	assert.Equal(t, 3001, linked)
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
	assert.Error(t, err)
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
		{"other folder", `c:\other`, "local", "2026-01-02T03:04:03Z", false},
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
				assert.Error(t, err)
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
	assert.Greater(t, version, 189)
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
			assert.ErrorIs(t, err, sql.ErrNoRows)
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
		{"conditional launch", "/repo", `{"command":"false && codex exec hi"}`},
		{"undecodable input", "/repo", `{"redacted":true}`},
		{"home cd", "/repo", `{"command":"cd; codex exec hi"}`},
		{"previous cd", "/repo", `{"command":"cd -; codex exec hi"}`},
		{"tilde cd", "/repo", `{"command":"cd ~/repo; codex exec hi"}`},
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
			assert.ErrorIs(t, err, sql.ErrNoRows)
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
			assert.ErrorIs(t, err, sql.ErrNoRows)
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
				assert.ErrorIs(t, err, sql.ErrNoRows)
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
	assert.ErrorIs(t, err, sql.ErrNoRows)
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
			assert.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestLaunchParentDynamicPrompt(t *testing.T) {
	for _, withCompetitor := range []bool{false, true} {
		t.Run(fmt.Sprint(withCompetitor), func(t *testing.T) {
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
				assert.ErrorIs(t, err, sql.ErrNoRows)
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
		assert.ErrorIs(t, err, sql.ErrNoRows)
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
		assert.ErrorIs(t, err, sql.ErrNoRows)
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
		assert.ErrorIs(t, err, sql.ErrNoRows)
		copy, err := d.GetSessionFull(t.Context(), copyID)
		require.NoError(t, err)
		assert.Nil(t, copy.ParentSessionID)
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

func TestLaunchParentConditionalAlone(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "parent", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "parent", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"false && codex exec hi"}`}}})
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
	assert.ErrorIs(t, err, sql.ErrNoRows)
}
