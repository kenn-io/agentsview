//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

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
