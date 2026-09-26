//go:build chtest

package clickhouse

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// A failed publication must retain the previous complete metadata and usage;
// a later replacement or hard deletion must not leave its old facts visible.
func TestUsageSessionSnapshotPublication(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	result, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	type snapshot struct {
		version          uint64
		project          string
		count            int64
		messages, events uint64
		usage            []string
	}
	read := func() snapshot {
		t.Helper()
		var row snapshot
		require.NoError(t, s.conn.QueryRowContext(ctx, `SELECT push_version, project, message_count,
		 length(usage_messages),length(usage_events),arrayMap(m -> m.token_usage,usage_messages)
		 FROM usage_session_snapshots WHERE id = ?`, fixtureAlphaID).
			Scan(&row.version, &row.project, &row.count, &row.messages, &row.events, &row.usage))
		return row
	}
	before := read()
	require.EqualValues(t, 2, before.messages)
	require.EqualValues(t, 1, before.events)
	session, err := local.GetSessionFull(ctx, fixtureAlphaID)
	require.NoError(t, err)
	messages, err := local.GetAllMessages(ctx, fixtureAlphaID)
	require.NoError(t, err)
	session.Project = "corrected"
	session.MessageCount = 1
	messages[0].TokenUsage = []byte(`{"input_tokens":123,"output_tokens":45}`)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: *session, Messages: messages[:1], DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	s.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error {
		return errors.New("injected failure before snapshot publication")
	}}
	result, err = s.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.Errors)
	require.Equal(t, before, read(), "partially written source tables must not alter the published row")
	s.hooks = nil
	result, err = s.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	after := read()
	require.Greater(t, after.version, before.version)
	require.Equal(t, "corrected", after.project)
	require.EqualValues(t, 1, after.count)
	require.EqualValues(t, 1, after.messages)
	require.Zero(t, after.events)
	require.Equal(t, []string{string(messages[0].TokenUsage)}, after.usage)

	require.NoError(t, local.SoftDeleteSession(ctx, fixtureAlphaID))
	deleted, err := local.DeleteSessionIfTrashed(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	result, err = s.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	require.Zero(t, chtest.Count(t, s.conn, usageSessionSnapshotTable, "id = ?", fixtureAlphaID))
}

func TestUsageSessionSnapshotReadParity(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	result, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	messageFields := []string{"id AS session_id", "push_version"}
	for _, field := range []string{"ordinal", "timestamp", "model", "provider_id", "claude_message_id", "claude_request_id", "source_uuid", "token_usage"} {
		messageFields = append(messageFields, "record."+field+" AS "+field)
	}
	for _, column := range usageColumns {
		messageFields = append(messageFields, column.expression+" AS "+column.name)
	}
	messageSource := "(SELECT " + strings.Join(messageFields, ",") + " FROM usage_session_snapshots ARRAY JOIN usage_messages AS record)"
	events, ok := tableByName("usage_events")
	require.True(t, ok)
	var eventFields []string
	for _, column := range events.columns {
		eventFields = append(eventFields, "record."+column.name+" AS "+column.name)
	}
	eventFields = append(eventFields, "push_version")
	eventSource := "(SELECT " + strings.Join(eventFields, ",") + " FROM usage_session_snapshots ARRAY JOIN usage_events AS record)"
	for _, candidateSQL := range []string{"SELECT id FROM sessions", "SELECT id FROM sessions WHERE id = 'ch-price-snap-a'"} {
		baseline, args := clickActivityReportUsageQuery(chSessionSet{body: candidateSQL}, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z")
		snapshotQuery := strings.ReplaceAll(baseline, "FROM usage_messages m", "FROM "+messageSource+" m")
		snapshotQuery = strings.ReplaceAll(snapshotQuery, "FROM usage_events ue", "FROM "+eventSource+" ue")
		snapshotQuery = strings.ReplaceAll(snapshotQuery, "FROM sessions", "FROM usage_session_snapshots")
		snapshotQuery = strings.ReplaceAll(snapshotQuery, "JOIN sessions s", "JOIN usage_session_snapshots s")
		var want []string
		for _, query := range []string{baseline, snapshotQuery} {
			rows, err := store.scanActivityUsageRows(ctx, query, args)
			require.NoError(t, err)
			require.NotEmpty(t, rows)
			got := make([]string, len(rows))
			for i, row := range rows {
				got[i] = fmt.Sprintf("%#v", row)
			}
			slices.Sort(got)
			if want == nil {
				want = got
			} else {
				require.Equal(t, want, got)
			}
		}
	}
}

func TestUsageSnapshotWithoutSessionFingerprintIsRemoved(t *testing.T) {
	for _, mode := range []string{"full", "out-of-scope"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			local, target := seedFixture(t)
			s := newTestSync(t, local, target, storage.PusherOptions{})
			session, err := local.GetSessionFull(ctx, fixtureAlphaID)
			require.NoError(t, err)
			payload, err := s.loadPayload(ctx, *session)
			require.NoError(t, err)
			// Model a crash after the complete row lands but before its
			// sessions fingerprint is published.
			require.NoError(t, s.insertUsageSessionSnapshots(ctx, []sessionPayload{payload}, nil, newPushVersion()))
			var result storage.PushResult
			if mode == "full" {
				err = s.deleteSessionsMissingLocally(ctx, nil, &result)
			} else {
				err = s.deleteResidentSessions(ctx, []string{fixtureAlphaID}, &result)
			}
			require.NoError(t, err)
			require.Equal(t, 1, result.DeletedStale)
			require.Zero(t, chtest.Count(t, s.conn, usageSessionSnapshotTable, "id = ?", fixtureAlphaID))
		})
	}
}
