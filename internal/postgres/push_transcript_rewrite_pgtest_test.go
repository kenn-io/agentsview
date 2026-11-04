//go:build pgtest

package postgres

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPGFullPushRewritesTranscriptsOnlyWhenRequested checks that a full push
// forced by a missing backfill marker leaves unchanged messages in place,
// while it still copies changed messages and a requested full push, or the
// resumption of one, rewrites every transcript.
func TestPGFullPushRewritesTranscriptsOnlyWhenRequested(t *testing.T) {
	tests := []struct {
		name         string
		prepare      func(t *testing.T, syncer *Sync)
		requested    bool
		wantReason   string
		wantMessages int
		wantRewrite  bool
	}{
		{
			name: "backfill keeps unchanged transcript",
			prepare: func(t *testing.T, syncer *Sync) {
				clearAliasBackfillMarker(t, syncer)
			},
			wantReason: "backfilling session aliases",
		},
		{
			name: "backfill copies changed message",
			prepare: func(t *testing.T, syncer *Sync) {
				_, err := syncer.pg.ExecContext(t.Context(),
					`UPDATE messages SET content = 'stale' WHERE session_id = 'rewrite-scope'`)
				require.NoError(t, err)
				clearAliasBackfillMarker(t, syncer)
			},
			wantReason:   "backfilling session aliases",
			wantMessages: 1,
			wantRewrite:  true,
		},
		{
			name:         "requested full push rewrites transcript",
			requested:    true,
			wantMessages: 1,
			wantRewrite:  true,
		},
		{
			name: "resumed requested full push rewrites transcript",
			prepare: func(t *testing.T, syncer *Sync) {
				markerID, err := syncer.pushMarkerID(t.Context())
				require.NoError(t, err)
				record, err := json.Marshal(syncer.newFullPushRun("requested", markerID, nil))
				require.NoError(t, err)
				require.NoError(t, syncer.effectiveSyncState().SetSyncState(
					t.Context(), fullPushRunStateKey, string(record)))
			},
			wantReason:   "resuming an unfinished full push, started because: requested",
			wantMessages: 1,
			wantRewrite:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncer, localDB, pg, ctx := newSessionProvenancePushSync(t, "agentsview_transcript_rewrite_test")
			seedProvenanceSession(t, localDB, "rewrite-scope", "workstation", "project", "")
			_, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			before := pgMessageRowVersion(t, syncer)

			if tt.prepare != nil {
				tt.prepare(t, syncer)
			}
			result, err := syncer.Push(ctx, tt.requested, nil)
			require.NoError(t, err)

			assert.True(t, result.Full)
			assert.Equal(t, tt.wantReason, result.FullReason)
			assert.Equal(t, 1, result.SessionsPushed)
			assert.Equal(t, tt.wantMessages, result.MessagesPushed)
			if tt.wantRewrite {
				assert.NotEqual(t, before, pgMessageRowVersion(t, syncer))
			} else {
				assert.Equal(t, before, pgMessageRowVersion(t, syncer))
			}
			var content string
			require.NoError(t, pg.QueryRowContext(ctx,
				`SELECT content FROM messages WHERE session_id = 'rewrite-scope' AND ordinal = 0`,
			).Scan(&content))
			assert.Equal(t, "hello", content)

			again, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			assert.False(t, again.Full)
			assert.Zero(t, again.MessagesPushed)
		})
	}
}

func clearAliasBackfillMarker(t *testing.T, syncer *Sync) {
	t.Helper()
	require.NoError(t, syncer.aliasBackfillSyncStateOrDefault().SetSyncState(
		t.Context(), sessionAliasBackfillStateKey, ""))
}

// pgMessageRowVersion returns the transaction that last wrote the session's
// message row, so a test can tell an untouched row from a rewritten one.
func pgMessageRowVersion(t *testing.T, syncer *Sync) string {
	t.Helper()
	var xmin string
	require.NoError(t, syncer.pg.QueryRowContext(t.Context(),
		`SELECT xmin::text FROM messages WHERE session_id = 'rewrite-scope' AND ordinal = 0`,
	).Scan(&xmin))
	return xmin
}
