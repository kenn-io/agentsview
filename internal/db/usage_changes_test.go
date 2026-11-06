//go:build fts5

package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageChangeJournal(t *testing.T) {
	for _, mode := range []struct {
		name string
		open func(testing.TB) *DB
	}{
		{"normal", testDB},
		{"fresh", testFreshDB},
	} {
		t.Run(mode.name, func(t *testing.T) {
			d := mode.open(t)
			ctx := t.Context()
			for _, tc := range []struct {
				name  string
				write func(*testing.T, Session)
			}{
				{"append", func(t *testing.T, s Session) {
					t.Helper()
					require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", Content: "More", TokenUsage: []byte(`{"output_tokens":2}`)}}))
				}},
				{"message usage", func(t *testing.T, s Session) {
					t.Helper()
					require.NoError(t, d.ReplaceSessionMessages(ctx, s.ID, []Message{{SessionID: s.ID, Ordinal: 0, Role: "assistant", Content: "Hello", TokenUsage: []byte(`{"output_tokens":3}`)}}))
				}},
				{"usage events", func(t *testing.T, s Session) {
					t.Helper()
					require.NoError(t, d.ReplaceSessionUsageEvents(ctx, s.ID, []UsageEvent{{Source: "session", Model: "model-x", OutputTokens: 5}}))
				}},
				{"clear usage events", func(t *testing.T, s Session) {
					t.Helper()
					require.NoError(t, d.ReplaceSessionUsageEvents(ctx, s.ID, nil))
				}},
				{"rename", func(t *testing.T, s Session) {
					t.Helper()
					require.NoError(t, d.RenameSession(ctx, s.ID, new("New title")))
				}},
				{"session name", func(t *testing.T, s Session) {
					t.Helper()
					s.SessionName = new("Agent title")
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"relationship", func(t *testing.T, s Session) {
					t.Helper()
					s.RelationshipType = "subagent"
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"created at", func(t *testing.T, s Session) {
					t.Helper()
					_, err := d.getWriter().ExecContext(ctx, "UPDATE sessions SET created_at = ? WHERE id = ?", "2026-07-01T10:00:00Z", s.ID)
					require.NoError(t, err)
				}},
				{"start time", func(t *testing.T, s Session) {
					t.Helper()
					s.StartedAt = new("2026-07-02T10:00:00Z")
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"end time", func(t *testing.T, s Session) {
					t.Helper()
					s.EndedAt = new("2026-07-02T11:00:00Z")
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"termination status", func(t *testing.T, s Session) {
					t.Helper()
					s.TerminationStatus = new("completed")
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"agent", func(t *testing.T, s Session) {
					t.Helper()
					s.Agent = "codex"
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"machine", func(t *testing.T, s Session) {
					t.Helper()
					s.Machine = "machine-b"
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"branch", func(t *testing.T, s Session) {
					t.Helper()
					s.GitBranch = "feature"
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"group", func(t *testing.T, s Session) {
					t.Helper()
					s.GroupKey = "group-a"
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"user messages", func(t *testing.T, s Session) {
					t.Helper()
					s.UserMessageCount = 2
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"automation", func(t *testing.T, s Session) {
					t.Helper()
					s.IsAutomated = true
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"soft delete", func(t *testing.T, s Session) {
					t.Helper()
					require.NoError(t, d.SoftDeleteSession(ctx, s.ID))
				}},
				{"restore", func(t *testing.T, s Session) {
					t.Helper()
					_, err := d.RestoreSession(ctx, s.ID)
					require.NoError(t, err)
				}},
				{"hard delete", func(t *testing.T, s Session) {
					t.Helper()
					require.NoError(t, d.DeleteSession(ctx, s.ID))
				}},
				{"reinsert", func(t *testing.T, s Session) {
					t.Helper()
					_, err := d.getWriter().ExecContext(ctx, "DELETE FROM excluded_sessions WHERE id = ?", s.ID)
					require.NoError(t, err)
					require.NoError(t, d.UpsertSession(ctx, s))
				}},
				{"project", func(t *testing.T, s Session) {
					t.Helper()
					_, err := d.AssignSessionProject(ctx, s.ID, "project-b")
					require.NoError(t, err)
				}},
				{"atomic sync insert", func(t *testing.T, s Session) {
					t.Helper()
					result, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{Session: s}})
					require.NoError(t, err)
					assert.Equal(t, 1, result.WrittenSessions)
				}},
				{"atomic sync events", func(t *testing.T, s Session) {
					t.Helper()
					result, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{
						Session: s, UsageEvents: []UsageEvent{{Source: "session", OutputTokens: 5}},
					}})
					require.NoError(t, err)
					assert.Equal(t, 1, result.WrittenSessions)
				}},
				{"atomic sync append", func(t *testing.T, s Session) {
					t.Helper()
					result, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{
						Session:  s,
						Messages: []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", Content: "More", TokenUsage: []byte(`{"output_tokens":2}`)}},
					}})
					require.NoError(t, err)
					assert.Equal(t, 1, result.WrittenSessions)
				}},
			} {
				if mode.name == "fresh" && tc.name != "machine" {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					s := Session{ID: tc.name, Project: "project-a", Machine: "machine-a", Agent: "claude", StartedAt: new("2026-07-01T10:00:00Z")}
					before, err := d.UsageChangeRevision(ctx)
					require.NoError(t, err)
					if tc.name == "atomic sync insert" {
						tc.write(t, s)
					} else {
						require.NoError(t, d.UpsertSession(ctx, s))
					}
					if tc.name == "machine" || tc.name == "atomic sync insert" {
						inserted, err := d.UsageChangeRevision(ctx)
						require.NoError(t, err)
						assert.Greater(t, inserted, before)
						ids, err := d.LoadUsageChanges(ctx, before, inserted)
						require.NoError(t, err)
						assert.Equal(t, []string{s.ID}, ids)
					}
					if tc.name == "atomic sync insert" {
						return
					}
					require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: s.ID, Ordinal: 0, Role: "assistant", Content: "Hello", TokenUsage: []byte(`{"output_tokens":1}`)}}))
					switch tc.name {
					case "clear usage events":
						before, err = d.UsageChangeRevision(ctx)
						require.NoError(t, err)
						require.NoError(t, d.ReplaceSessionUsageEvents(ctx, s.ID, []UsageEvent{
							{Source: "session", Model: "model-x", OutputTokens: 5},
							{Source: "session", Model: "model-y", OutputTokens: 7},
						}))
						written, err := d.UsageChangeRevision(ctx)
						require.NoError(t, err)
						assert.Greater(t, written, before)
						ids, err := d.LoadUsageChanges(ctx, before, written)
						require.NoError(t, err)
						assert.Equal(t, []string{s.ID}, ids)
					case "atomic sync events":
						require.NoError(t, d.ReplaceSessionUsageEvents(ctx, s.ID, []UsageEvent{{Source: "session", OutputTokens: 1}}))
					case "restore":
						require.NoError(t, d.SoftDeleteSession(ctx, s.ID))
					case "reinsert":
						require.NoError(t, d.DeleteSession(ctx, s.ID))
					}
					before, err = d.UsageChangeRevision(ctx)
					require.NoError(t, err)
					tc.write(t, s)
					after, err := d.UsageChangeRevision(ctx)
					require.NoError(t, err)
					assert.Greater(t, after, before)
					ids, err := d.LoadUsageChanges(ctx, before, after)
					require.NoError(t, err)
					assert.Equal(t, []string{s.ID}, ids)
				})
			}

			if mode.name == "fresh" {
				return
			}

			t.Run("no usage change", func(t *testing.T) {
				s := Session{ID: "unchanged", Project: "project-a", Machine: "machine-a", Agent: "claude"}
				require.NoError(t, d.UpsertSession(ctx, s))
				before, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				require.NoError(t, d.UpsertSession(ctx, s))
				_, err = d.SetSessionLabels(ctx, s.ID, []string{"review"})
				require.NoError(t, err)
				_, err = d.getWriter().ExecContext(ctx, `UPDATE sessions SET local_modified_at = '2100-07-03T10:00:00.000Z', file_mtime = '2100-07-03T10:00:00.000Z', usage_generation = 'generation-only' WHERE id = ?`, s.ID)
				require.NoError(t, err)
				after, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				ids, err := d.LoadUsageChanges(ctx, before, after)
				require.NoError(t, err)
				assert.Empty(t, ids)
			})

			t.Run("revision window", func(t *testing.T) {
				require.NoError(t, d.UpsertSession(ctx, Session{ID: "first", Project: "project-a", Machine: "machine-a", Agent: "claude"}))
				first, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				require.NoError(t, d.UpsertSession(ctx, Session{ID: "second", Project: "project-a", Machine: "machine-a", Agent: "claude"}))
				second, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				ids, err := d.LoadUsageChanges(ctx, first, second)
				require.NoError(t, err)
				assert.Equal(t, []string{"second"}, ids)
				ids, err = d.LoadUsageChanges(ctx, second, second)
				require.NoError(t, err)
				assert.Empty(t, ids)
				_, err = d.LoadUsageChanges(ctx, second, first)
				assert.Error(t, err)
			})
		})
	}
}

func TestUsageChangeJournalUsageEventUpdates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutation string
		wantIDs  []string
	}{
		{"token correction", `UPDATE usage_events SET input_tokens = 20 WHERE session_id = 'source'`, []string{"source"}},
		{"identical update", `UPDATE usage_events SET input_tokens = 10 WHERE session_id = 'source'`, nil},
		{"move session", `UPDATE usage_events SET session_id = 'target' WHERE session_id = 'source'`, []string{"source", "target"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "source", "project-a")
			insertSession(t, d, "target", "project-a")
			require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "source", []UsageEvent{{Source: "session", Model: "model-x", InputTokens: 10}}))
			before, err := d.UsageChangeRevision(ctx)
			require.NoError(t, err)

			result, err := d.getWriter().ExecContext(ctx, tc.mutation)
			require.NoError(t, err)
			rows, err := result.RowsAffected()
			require.NoError(t, err)
			require.EqualValues(t, 1, rows)
			after, err := d.UsageChangeRevision(ctx)
			require.NoError(t, err)
			if len(tc.wantIDs) == 0 {
				assert.Equal(t, before, after)
			} else {
				assert.Greater(t, after, before)
			}
			ids, err := d.LoadUsageChanges(ctx, before, after)
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.wantIDs, ids)
		})
	}
}

func TestUsageChangeJournalPersistsForOtherWriters(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "external", "project-a")
	before, err := d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	other, err := sql.Open("sqlite3", makeDSN(d.Path(), false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	_, err = other.ExecContext(ctx, `UPDATE sessions SET machine = 'machine-b' WHERE id = 'external'`)
	require.NoError(t, err)
	metadata, err := d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, metadata, before)
	ids, err := d.LoadUsageChanges(ctx, before, metadata)
	require.NoError(t, err)
	assert.Equal(t, []string{"external"}, ids)
	_, err = other.ExecContext(ctx, `INSERT INTO usage_events (session_id, source, model, output_tokens) VALUES ('external', 'session', 'model-x', 7)`)
	require.NoError(t, err)
	require.NoError(t, other.Close())
	path := d.Path()
	require.NoError(t, d.Close())
	reopened, err := OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	after, err := reopened.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, after, metadata)
	ids, err = reopened.LoadUsageChanges(ctx, metadata, after)
	require.NoError(t, err)
	assert.Equal(t, []string{"external"}, ids)
}

func TestUsageChangeJournalUpgradeStartsWithoutHistory(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "existing", "project-a")
	path := d.Path()
	require.NoError(t, d.Close())
	old, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, old.Close()) })
	_, err = old.ExecContext(ctx, usageChangeJournalTriggerDropsSQL+`
		DROP TABLE usage_session_changes;
		DELETE FROM archive_metadata WHERE key = 'usage_change_revision';`)
	require.NoError(t, err)
	require.NoError(t, old.Close())
	upgraded, err := OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	before, err := upgraded.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Zero(t, before)
	for _, query := range []string{
		`UPDATE sessions SET machine = 'machine-b' WHERE id = 'existing'`,
		`INSERT INTO usage_events (session_id, source, model, output_tokens) VALUES ('existing', 'session', 'model-x', 7)`,
		`INSERT INTO model_pricing (model_pattern) VALUES ('model-x')`,
	} {
		_, err := upgraded.getWriter().ExecContext(ctx, query)
		require.NoError(t, err)
		after, err := upgraded.UsageChangeRevision(ctx)
		require.NoError(t, err)
		assert.Greater(t, after, before, query)
		before = after
	}
	ids, err := upgraded.LoadUsageChanges(ctx, 0, before)
	require.NoError(t, err)
	assert.Equal(t, []string{"existing"}, ids)
}

func TestUsageChangeJournalSurvivesCanceledMigration(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "external", "project-a")
	other, err := sql.Open("sqlite3", makeDSN(d.Path(), false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	// Recreate an older schema so cancellation occurs during a column migration.
	_, err = other.ExecContext(ctx, `ALTER TABLE excluded_sessions DROP COLUMN include_codex_pages`)
	require.NoError(t, err)
	before, err := d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	migrationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	err = d.migrateColumns(migrationCtx, func(progress OpenProgress) {
		if progress.Detail == "Adding column excluded_sessions.include_codex_pages" {
			cancel()
		}
	})
	require.Error(t, err)
	require.ErrorIs(t, migrationCtx.Err(), context.Canceled)
	_, err = other.ExecContext(ctx, `UPDATE sessions SET machine = 'machine-b' WHERE id = 'external'`)
	require.NoError(t, err)
	after, err := d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, after, before)
	ids, err := d.LoadUsageChanges(ctx, before, after)
	require.NoError(t, err)
	assert.Equal(t, []string{"external"}, ids)
}

func TestUsageChangeJournalPricingIgnoresMetadataAndUnchangedRows(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	before, err := d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	require.NoError(t, d.SetPricingMetaContext(ctx, "_last_fetch", "2026-07-01T10:00:00Z"))
	require.NoError(t, d.SetPricingMetaContext(ctx, "_last_fetch", "2026-07-02T10:00:00Z"))
	after, err := d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM model_pricing WHERE model_pattern = '_last_fetch'`)
	require.NoError(t, err)
	after, err = d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)

	for _, tc := range []struct {
		table  string
		insert string
		column string
		value  any
	}{
		{"model_pricing", `INSERT INTO model_pricing (model_pattern) VALUES ('model-x')`, "input_microdollars_per_mtok", 100},
		{"model_pricing_bands", `INSERT INTO model_pricing_bands (model_pattern, above_input_tokens, input_microdollars_per_mtok, output_microdollars_per_mtok, cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok) VALUES ('model-x', 1000, 0, 0, 0, 0)`, "input_microdollars_per_mtok", 200},
		{"genai_pricing", `INSERT INTO genai_pricing (singleton, version, source, data_json) VALUES (1, 'v1', 'test', '{}')`, "data_json", `{"price":3}`},
	} {
		t.Run(tc.table, func(t *testing.T) {
			before, err := d.UsageChangeRevision(ctx)
			require.NoError(t, err)
			_, err = d.getWriter().ExecContext(ctx, tc.insert)
			require.NoError(t, err)
			inserted, err := d.UsageChangeRevision(ctx)
			require.NoError(t, err)
			assert.Greater(t, inserted, before)
			ids, err := d.LoadUsageChanges(ctx, before, inserted)
			require.NoError(t, err)
			assert.Empty(t, ids)
			_, err = d.getWriter().ExecContext(ctx, "UPDATE "+tc.table+" SET "+tc.column+" = "+tc.column)
			require.NoError(t, err)
			unchanged, err := d.UsageChangeRevision(ctx)
			require.NoError(t, err)
			assert.Equal(t, inserted, unchanged)
			_, err = d.getWriter().ExecContext(ctx, "UPDATE "+tc.table+" SET "+tc.column+" = ?", tc.value)
			require.NoError(t, err)
			changed, err := d.UsageChangeRevision(ctx)
			require.NoError(t, err)
			assert.Greater(t, changed, unchanged)
			ids, err = d.LoadUsageChanges(ctx, before, changed)
			require.NoError(t, err)
			assert.Empty(t, ids)
		})
	}
}
