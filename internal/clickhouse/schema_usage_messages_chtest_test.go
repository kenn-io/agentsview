//go:build chtest

package clickhouse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// A backfill racing with a retry must not undo the retry, and removing usage
// from a message must remove its contribution even if the ordinal is retained.
func TestClickHouseUsageMessagesReplacement(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	query, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	for _, agent := range []string{"claude", "codex", "grok", "gemini", "opencode", "copilot", "cursor", "amp"} {
		t.Run(agent, func(t *testing.T) {
			id := "usage-replacement-" + agent
			_, err := store.DB().ExecContext(ctx, `INSERT INTO sessions
			 (id,agent,project,started_at,message_count,push_version) VALUES (?,?,'usage-replacement',parseDateTime64BestEffort('2026-01-10T12:00:00Z'),1,1)`, id, agent)
			require.NoError(t, err)
			for _, tokens := range []string{`{"output_tokens":17}`, `{"output_tokens":31}`, ""} {
				timestamp := "2026-01-10T12:01:00Z"
				if tokens == `{"output_tokens":31}` {
					timestamp = "2026-02-10T12:01:00Z"
				}
				_, err = store.DB().ExecContext(ctx, `INSERT INTO messages
				 (session_id,ordinal,timestamp,model,token_usage,push_version)
				 VALUES (?,0,parseDateTime64BestEffort(?),'gpt-4o',?,1)`, id, timestamp, tokens)
				require.NoError(t, err)
				// This older backfill finishes after the live insert of the same version.
				_, err = store.DB().ExecContext(ctx, `INSERT INTO usage_messages
				 (session_id,ordinal,timestamp,model,push_version,usage_present,usage_output,revision)
				 VALUES (?,0,parseDateTime64BestEffort('2026-01-10T12:01:00Z'),'gpt-4o',1,1,99,2)`, id)
				require.NoError(t, err)
				got, err := store.GetSessionUsageRows(ctx, []string{id})
				require.NoError(t, err)
				want := 0
				if tokens == `{"output_tokens":17}` {
					want = 17
				} else if tokens != "" {
					want = 31
				}
				require.Equal(t, want, got.RawOutputTokensBySession[id])
				var source string
				require.NoError(t, store.DB().QueryRowContext(ctx,
					"SELECT token_usage FROM messages WHERE session_id=?", id).Scan(&source))
				require.Equal(t, tokens, source)
				report, err := store.BuildActivityReportArtifacts(ctx,
					db.AnalyticsFilter{Project: "usage-replacement", Agent: agent, Timezone: "UTC"}, query, nil)
				require.NoError(t, err)
				dayOutput := 0
				if tokens == `{"output_tokens":17}` {
					dayOutput = 17
				}
				require.Equal(t, dayOutput, report.Report.Totals.OutputTokens)
			}
			// A shorter published session leaves no matching message version.
			_, err = store.DB().ExecContext(ctx, `INSERT INTO messages
			 (session_id,ordinal,model,token_usage,push_version) VALUES (?,0,'gpt-4o',?,1)`, id, `{"output_tokens":67}`)
			require.NoError(t, err)
			_, err = store.DB().ExecContext(ctx, `INSERT INTO sessions
			 (id,agent,started_at,push_version) VALUES (?,?,parseDateTime64BestEffort('2026-01-10T12:00:00Z'),2)`, id, agent)
			require.NoError(t, err)
			got, err := store.GetSessionUsageRows(ctx, []string{id})
			require.NoError(t, err)
			require.Zero(t, got.RawOutputTokensBySession[id])
		})
	}
	// A reader must refuse the partially filled table after an interrupted startup.
	_, err = store.DB().ExecContext(ctx, "DELETE FROM sync_metadata WHERE key='usage_messages_backfill' SETTINGS mutations_sync=1")
	require.NoError(t, err)
	require.ErrorContains(t, CheckSchemaCompat(ctx, store.DB()), "backfill is incomplete")
	require.NoError(t, ensureUsageMessages(ctx, store.DB()))
	require.NoError(t, CheckSchemaCompat(ctx, store.DB()))
}

// A push writes messages before it publishes the session row, and an
// interrupted push never publishes it. Usage must not read as zero meanwhile.
func TestClickHouseUsageSurvivesUnpublishedPush(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	const id = "usage-unpublished-push"
	query, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	outputTokens := func() (int, int) {
		t.Helper()
		got, err := store.GetSessionUsageRows(ctx, []string{id})
		require.NoError(t, err)
		report, err := store.BuildActivityReportArtifacts(ctx,
			db.AnalyticsFilter{Project: id, Timezone: "UTC"}, query, nil)
		require.NoError(t, err)
		return got.RawOutputTokensBySession[id], report.Report.Totals.OutputTokens
	}
	exec := func(statement string, args ...any) {
		t.Helper()
		_, err := store.DB().ExecContext(ctx, statement, args...)
		require.NoError(t, err)
	}
	const insertMessage = `INSERT INTO messages (session_id,ordinal,timestamp,model,token_usage,push_version)
		VALUES (?,?,parseDateTime64BestEffort('2026-01-10T12:01:00Z'),'gpt-4o',?,?)`
	const publish = `INSERT INTO sessions (id,agent,project,started_at,message_count,push_version)
		VALUES (?,'codex',?,parseDateTime64BestEffort('2026-01-10T12:00:00Z'),?,?)`

	exec(insertMessage, id, 0, `{"output_tokens":17}`, 1)
	exec(insertMessage, id, 1, `{"output_tokens":5}`, 1)
	exec(publish, id, id, 2, 1)
	session, day := outputTokens()
	require.Equal(t, 22, session)
	require.Equal(t, 22, day)

	// Version 2 rewrites the session as a single message; its row is not published yet.
	exec(insertMessage, id, 0, `{"output_tokens":31}`, 2)
	session, day = outputTokens()
	require.Equal(t, 36, session, "the rewritten message shows beside the not yet replaced tail")
	require.Equal(t, 36, day)

	exec(publish, id, id, 1, 2)
	session, day = outputTokens()
	require.Equal(t, 31, session, "the older tail falls below the published version")
	require.Equal(t, 31, day)
}
