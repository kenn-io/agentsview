//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestSignalVersionMicroseconds(t *testing.T) {
	const schema = "agentsview_signals_version_test"
	const id = "signals-version"
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, schema)
	url := testPGURL(t)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: id, Agent: "claude", Project: "sample", Machine: "workstation", MessageCount: 1,
	}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{
		SessionID: id, Ordinal: 0, Role: "user", Content: "this is broken",
	}}))
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, url, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	for _, test := range []struct {
		name      string
		timestamp string
		want      int64
	}{
		{"before-epoch", "1969-12-31T23:59:59.999999Z", -1},
		{"epoch", "1970-01-01T00:00:00Z", 0},
		{"after-epoch", "1970-01-01T00:00:00.000001Z", 1},
		{"fractional", "2023-11-14T22:13:20.123456Z", 1700000000123456},
		{"offset", "2023-11-14T17:13:20.654321-05:00", 1700000000654321},
	} {
		t.Run(test.name, func(t *testing.T) {
			var updatedAt time.Time
			require.NoError(t, pg.QueryRowContext(ctx, `UPDATE sessions SET updated_at = $1::timestamptz WHERE id = $2 RETURNING updated_at`, test.timestamp, id).Scan(&updatedAt))
			rows, versions, err := store.signalRows(ctx, db.AnalyticsFilter{})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, test.want, versions[id])
			assert.Equal(t, updatedAt.UnixMicro(), versions[id])

			require.NoError(t, store.populateFrustrationMarkers(ctx, rows, versions))
			assert.Equal(t, 1, rows[0].FrustrationMarkerCount)
			count, hit := store.runtimeState().frustrationMarkers.Lookup(id, test.want)
			assert.True(t, hit, "both queries must use the same microsecond version")
			assert.Equal(t, 1, count)
		})
	}
}

func TestFrustrationMemoPublicationBetweenReads(t *testing.T) {
	for _, test := range []struct {
		name string
		want int
	}{
		{"edit", 1},
		{"remove-prompts", 0},
		{"remove-session", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			const schema = "agentsview_signals_memo_race_test"
			const id = "signals-memo-race"
			syncer, local, pg, ctx := newSessionProvenancePushSync(t, schema)
			url := testPGURL(t)
			t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
			require.NoError(t, local.UpsertSession(ctx, db.Session{
				ID: id, Agent: "claude", Project: "sample", Machine: "workstation", MessageCount: 1,
			}))
			require.NoError(t, local.InsertMessages(ctx, []db.Message{{
				SessionID: id, Ordinal: 0, Role: "user", Content: "Please explain this function.",
			}}))
			_, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			store, err := NewStore(ctx, url, schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			rows, versions, err := store.signalRows(ctx, db.AnalyticsFilter{})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Contains(t, versions, id)

			// Publish after selection, before the separate prompt scan.
			tx, err := pg.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			switch test.name {
			case "edit":
				_, err = tx.ExecContext(ctx, `UPDATE messages SET content = 'this is broken' WHERE session_id = $1`, id)
			case "remove-prompts":
				_, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = $1`, id)
			case "remove-session":
				_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
			}
			require.NoError(t, err)
			var updatedAt time.Time
			if test.name != "remove-session" {
				require.NoError(t, tx.QueryRowContext(ctx, `UPDATE sessions SET updated_at = updated_at + interval '1 second' WHERE id = $1 RETURNING updated_at`, id).Scan(&updatedAt))
			}
			require.NoError(t, tx.Commit())

			require.NoError(t, store.populateFrustrationMarkers(ctx, rows, versions))
			assert.Equal(t, test.want, rows[0].FrustrationMarkerCount)
			memo := &store.runtimeState().frustrationMarkers
			_, hit := memo.Lookup(id, versions[id])
			assert.False(t, hit, "the prompt scan must not cache published content under the selected old version")
			if test.name != "remove-session" {
				count, hit := memo.Lookup(id, updatedAt.UnixMicro())
				assert.True(t, hit, "the prompt scan must cache the version it read, including sessions without prompts")
				assert.Equal(t, test.want, count)
			}
		})
	}
}

func TestFrustrationMemo(t *testing.T) {
	for _, test := range []struct {
		name string
		want int
	}{
		{"reuse", 7},
		{"append", 2},
		{"replace", 0},
		{"rewrite", 1},
		{"recreate", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			const schema = "agentsview_signals_memo_test"
			const id = "signals-memo"
			syncer, local, _, ctx := newSessionProvenancePushSync(t, schema)
			url := testPGURL(t)
			t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
			session := db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "workstation", MessageCount: 1}
			require.NoError(t, local.UpsertSession(ctx, session))
			messages := []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "this is broken"}}
			require.NoError(t, local.InsertMessages(ctx, messages))
			require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
				QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
			}))
			_, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			if test.name == "rewrite" {
				_, err = syncer.pg.ExecContext(ctx, `UPDATE messages SET content = 'Please explain this function.' WHERE session_id = $1`, id)
				require.NoError(t, err)
			}
			store, err := NewStore(ctx, url, schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			initial, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
			require.NoError(t, err)
			if test.name == "rewrite" {
				require.Zero(t, initial.QualityHealth.Totals.FrustrationMarkerCount)
			} else {
				require.Equal(t, 1, initial.QualityHealth.Totals.FrustrationMarkerCount)
			}
			_, versions, err := store.signalRows(ctx, db.AnalyticsFilter{})
			require.NoError(t, err)
			require.Contains(t, versions, id)
			switch test.name {
			case "reuse":
				store.runtimeState().frustrationMarkers.Store(id, versions[id], 7)
			case "append":
				messages = append(messages, db.Message{SessionID: id, Ordinal: 1, Role: "user", Content: "this is broken"})
			case "replace", "recreate":
				messages[0].Content = "Please explain this function."
			}
			if test.name != "reuse" && test.name != "rewrite" {
				session.MessageCount = len(messages)
				_, err = local.WriteSessionBatchContext(ctx, []db.SessionBatchWrite{{Session: session, Messages: messages, ReplaceMessages: true}})
				require.NoError(t, err)
				require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
					QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
				}))
			}
			if test.name == "recreate" {
				_, err = syncer.pg.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
				require.NoError(t, err)
			}
			if test.name != "reuse" {
				var before, after string
				if test.name == "rewrite" {
					require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT transcript_revision::text || ':' || created_at::text FROM sessions WHERE id = $1`, id).Scan(&before))
				}
				_, err = syncer.Push(ctx, test.name == "rewrite", nil)
				require.NoError(t, err)
				if test.name == "rewrite" {
					require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT transcript_revision::text || ':' || created_at::text FROM sessions WHERE id = $1`, id).Scan(&after))
					assert.Equal(t, before, after)
				}
			}
			warm, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
			require.NoError(t, err)
			assert.Equal(t, test.want, warm.QualityHealth.Totals.FrustrationMarkerCount)
			if test.name != "reuse" {
				fresh, err := NewStore(ctx, url, schema, true)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, fresh.Close()) })
				scanned, err := fresh.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
				require.NoError(t, err)
				assert.Equal(t, scanned.QualityHealth.Totals.FrustrationMarkerCount, warm.QualityHealth.Totals.FrustrationMarkerCount)
			}
		})
	}
}
