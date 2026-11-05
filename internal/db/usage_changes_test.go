//go:build fts5

package db

import (
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
				{"start time", func(t *testing.T, s Session) {
					t.Helper()
					s.StartedAt = new("2026-07-02T10:00:00Z")
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
				t.Run(tc.name, func(t *testing.T) {
					s := Session{ID: tc.name, Project: "project-a", Machine: "machine-a", Agent: "claude", StartedAt: new("2026-07-01T10:00:00Z")}
					before, err := d.UsageChangeRevision(ctx)
					require.NoError(t, err)
					if tc.name == "atomic sync insert" {
						tc.write(t, s)
					} else {
						require.NoError(t, d.UpsertSession(ctx, s))
					}
					inserted, err := d.UsageChangeRevision(ctx)
					require.NoError(t, err)
					assert.Greater(t, inserted, before)
					ids, err := d.LoadUsageChanges(ctx, before, inserted)
					require.NoError(t, err)
					assert.Equal(t, []string{s.ID}, ids)
					if tc.name == "atomic sync insert" {
						return
					}
					require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: s.ID, Ordinal: 0, Role: "assistant", Content: "Hello", TokenUsage: []byte(`{"output_tokens":1}`)}}))
					switch tc.name {
					case "clear usage events":
						require.NoError(t, d.ReplaceSessionUsageEvents(ctx, s.ID, []UsageEvent{{Source: "session", Model: "model-x", OutputTokens: 5}}))
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
					ids, err = d.LoadUsageChanges(ctx, before, after)
					require.NoError(t, err)
					assert.Equal(t, []string{s.ID}, ids)
				})
			}

			t.Run("unchanged usage events", func(t *testing.T) {
				const id = "same-events"
				require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Project: "project-a", Machine: "machine-a", Agent: "claude"}))
				events := []UsageEvent{
					{Source: "session", Model: "model-x", OutputTokens: 5, OccurredAt: "2026-07-02T10:00:00Z"},
					{Source: "session", Model: "model-y", InputTokens: 7, MessageOrdinal: new(0)},
				}
				require.NoError(t, d.ReplaceSessionUsageEvents(ctx, id, events))
				stored, err := d.GetUsageEvents(ctx, id)
				require.NoError(t, err)
				before, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				require.NoError(t, d.ReplaceSessionUsageEvents(ctx, id, events))
				after, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				ids, err := d.LoadUsageChanges(ctx, before, after)
				require.NoError(t, err)
				assert.Empty(t, ids)
				unchanged, err := d.GetUsageEvents(ctx, id)
				require.NoError(t, err)
				assert.Equal(t, stored, unchanged)
			})

			t.Run("consecutive session transactions advance revision", func(t *testing.T) {
				const id = "consecutive"
				require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Project: "project-a", Machine: "machine-a", Agent: "claude"}))
				before, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				events := []UsageEvent{{Source: "session", Model: "model-x", OutputTokens: 5}, {Source: "session", Model: "model-y", OutputTokens: 7}}
				require.NoError(t, d.ReplaceSessionUsageEvents(ctx, id, events))
				after, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				assert.Greater(t, after, before)
				ids, err := d.LoadUsageChanges(ctx, before, after)
				require.NoError(t, err)
				assert.Equal(t, []string{id}, ids)
				require.NoError(t, d.ReplaceSessionUsageEvents(ctx, id, nil))
				cleared, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				assert.Greater(t, cleared, after)
				ids, err = d.LoadUsageChanges(ctx, after, cleared)
				require.NoError(t, err)
				assert.Equal(t, []string{id}, ids)
			})

			t.Run("no usage change", func(t *testing.T) {
				s := Session{ID: "unchanged", Project: "project-a", Machine: "machine-a", Agent: "claude"}
				require.NoError(t, d.UpsertSession(ctx, s))
				require.NoError(t, d.UpsertSession(ctx, Session{ID: "before-unchanged", Project: "project-a", Machine: "machine-a", Agent: "claude"}))
				var marker string
				require.NoError(t, d.getReader().QueryRowContext(ctx, "SELECT COALESCE(sync_marker, '') FROM sessions WHERE id = ?", s.ID).Scan(&marker))
				before, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				require.NoError(t, d.UpsertSession(ctx, s))
				_, err = d.SetSessionLabels(ctx, s.ID, []string{"review"})
				require.NoError(t, err)
				_, err = d.getWriter().ExecContext(ctx, `UPDATE sessions SET local_modified_at = '2100-07-03T10:00:00.000Z', file_mtime = '2100-07-03T10:00:00.000Z', usage_generation = 'generation-only' WHERE id = ?`, s.ID)
				require.NoError(t, err)
				var changedMarker string
				require.NoError(t, d.getReader().QueryRowContext(ctx, "SELECT COALESCE(sync_marker, '') FROM sessions WHERE id = ?", s.ID).Scan(&changedMarker))
				// Fresh scratch archives omit the sync-marker triggers.
				if mode.name == "normal" {
					assert.Greater(t, changedMarker, marker)
				}
				after, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				ids, err := d.LoadUsageChanges(ctx, before, after)
				require.NoError(t, err)
				assert.Empty(t, ids)
			})

			t.Run("pricing", func(t *testing.T) {
				before, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				_, err = d.getWriter().ExecContext(ctx, "INSERT INTO model_pricing (model_pattern) VALUES ('model-x')")
				require.NoError(t, err)
				after, err := d.UsageChangeRevision(ctx)
				require.NoError(t, err)
				assert.Greater(t, after, before)
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
	var triggerCount int
	require.NoError(t, upgraded.getReader().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE '%_usage_change_%'`).Scan(&triggerCount))
	assert.Equal(t, 14, triggerCount)
	require.NoError(t, upgraded.InsertMessages(ctx, []Message{{SessionID: "existing", Role: "assistant", Content: "Hello", TokenUsage: []byte(`{"output_tokens":3}`)}}))
	after, err := upgraded.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, after, before)
	ids, err := upgraded.LoadUsageChanges(ctx, before, after)
	require.NoError(t, err)
	assert.Equal(t, []string{"existing"}, ids)
}
