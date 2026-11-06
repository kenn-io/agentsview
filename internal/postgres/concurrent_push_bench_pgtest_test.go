//go:build pgtest

package postgres

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// This file also runs unchanged on the base revision. One operation publishes
// four independent archives concurrently, including their messages and usage.
func BenchmarkPGConcurrentPush(b *testing.B) {
	const writers, sessions, messages = 4, 400, 20
	pgURL := testPGURL(b)
	schema := pgUsageBenchmarkSchema(b)
	b.Cleanup(func() { cleanNamedPGSchema(b, pgURL, schema) })
	syncers := make([]*Sync, writers)
	for writer := range writers {
		local, err := db.Open(b.Context(), filepath.Join(b.TempDir(), "archive.db"))
		require.NoError(b, err)
		b.Cleanup(func() { require.NoError(b, local.Close()) })
		machine := fmt.Sprintf("writer-%d", writer)
		for session := range sessions {
			id := fmt.Sprintf("%s-session-%d", machine, session)
			started := "2026-10-01T09:00:00Z"
			require.NoError(b, local.UpsertSession(b.Context(), db.Session{ID: id, Machine: machine, Project: fmt.Sprintf("project-%d", session%10), Agent: "codex", StartedAt: &started, CreatedAt: started, MessageCount: messages, UserMessageCount: messages / 2}))
			batch := make([]db.Message, messages)
			for ordinal := range messages {
				role := "user"
				if ordinal%2 == 1 {
					role = "assistant"
				}
				batch[ordinal] = db.Message{SessionID: id, Ordinal: ordinal, Role: role, Timestamp: started, Model: "model-a", Content: strings.Repeat("Inspect the query and explain its execution plan. ", 20), TokenUsage: []byte(`{"input_tokens":1200,"output_tokens":300,"cache_read_input_tokens":2400}`)}
			}
			require.NoError(b, local.InsertMessages(b.Context(), batch))
			require.NoError(b, local.ReplaceSessionUsageEvents(b.Context(), id, []db.UsageEvent{
				{SessionID: id, Source: "provider", Model: "model-a", InputTokens: 1200, OutputTokens: 300, OccurredAt: started, DedupKey: "event-a"},
				{SessionID: id, Source: "provider", Model: "model-a", InputTokens: 1400, OutputTokens: 400, OccurredAt: started, DedupKey: "event-b"},
			}))
		}
		syncer, err := New(b.Context(), pgURL, schema, local, machine, true, storage.PusherOptions{})
		require.NoError(b, err)
		b.Cleanup(func() { require.NoError(b, syncer.Close()) })
		require.NoError(b, syncer.EnsureSchema(b.Context()))
		syncers[writer] = syncer
	}
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		cleanNamedPGSchema(b, pgURL, schema)
		require.NoError(b, EnsureSchema(b.Context(), syncers[0].pg, schema))
		b.StartTimer()
		var group sync.WaitGroup
		errors := make(chan error, writers)
		start := make(chan struct{})
		for _, syncer := range syncers {
			group.Go(func() {
				<-start
				result, err := syncer.Push(b.Context(), true, nil)
				if err == nil && (result.Errors > 0 || result.SessionsPushed != sessions || result.MessagesPushed != sessions*messages) {
					err = fmt.Errorf("incomplete push: %+v", result)
				}
				errors <- err
			})
		}
		close(start)
		group.Wait()
		close(errors)
		for err := range errors {
			require.NoError(b, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(writers*sessions, "sessions/op")
	b.ReportMetric(writers*sessions*messages, "messages/op")
	b.ReportMetric(writers*sessions*2, "usage_events/op")
}

// This also runs unchanged on the base revision. One operation inserts 300,000
// usage events across 1,000 sessions in a single statement.
func BenchmarkPGUsageEventsBulkInsert(b *testing.B) {
	const sessions, events = 1000, 300000
	pgURL := testPGURL(b)
	schema := pgUsageBenchmarkSchema(b)
	cleanNamedPGSchema(b, pgURL, schema)
	b.Cleanup(func() { cleanNamedPGSchema(b, pgURL, schema) })
	pg, err := Open(b.Context(), pgURL, schema, true)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, pg.Close()) })
	require.NoError(b, EnsureSchema(b.Context(), pg, schema))
	_, err = pg.ExecContext(b.Context(), `INSERT INTO sessions(id,machine,project,agent)
 SELECT 'bulk-' || g, 'machine', 'project', 'codex' FROM generate_series(1, $1) g`, sessions)
	require.NoError(b, err)
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		_, err := pg.ExecContext(b.Context(), `DELETE FROM usage_events`)
		require.NoError(b, err)
		b.StartTimer()
		_, err = pg.ExecContext(b.Context(), `INSERT INTO usage_events(session_id,source,model,input_tokens,output_tokens,dedup_key)
 SELECT 'bulk-' || (g % $1 + 1), 'provider', 'model-a', g, g, 'event-' || g FROM generate_series(1, $2) g`, sessions, events)
		require.NoError(b, err)
	}
	b.StopTimer()
	b.ReportMetric(events, "usage_events/op")
}
