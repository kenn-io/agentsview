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
// updates from four independent archives concurrently, including messages and usage.
func BenchmarkPGConcurrentPush(b *testing.B) {
	const writers, sessions, messages = 4, 400, 20
	pgURL := testPGURL(b)
	schema := pgUsageBenchmarkSchema(b)
	b.Cleanup(func() { cleanNamedPGSchema(b, pgURL, schema) })
	syncers := make([]*Sync, writers)
	locals := make([]*db.DB, writers)
	writerSessions := make([][]db.Session, writers)
	for writer := range writers {
		local, err := db.Open(b.Context(), filepath.Join(b.TempDir(), "archive.db"))
		require.NoError(b, err)
		b.Cleanup(func() { require.NoError(b, local.Close()) })
		locals[writer] = local
		machine := fmt.Sprintf("writer-%d", writer)
		for session := range sessions {
			id := fmt.Sprintf("%s-session-%d", machine, session)
			started := "2026-10-01T09:00:00Z"
			row := db.Session{ID: id, Machine: machine, Project: fmt.Sprintf("project-%d", session%10), Agent: "codex", StartedAt: &started, CreatedAt: started, MessageCount: messages, UserMessageCount: messages / 2}
			require.NoError(b, local.UpsertSession(b.Context(), row))
			writerSessions[writer] = append(writerSessions[writer], row)
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
		result, err := syncer.Push(b.Context(), true, nil)
		require.NoError(b, err)
		require.Zero(b, result.Errors)
		require.Equal(b, sessions, result.SessionsPushed)
		require.Equal(b, sessions*messages, result.MessagesPushed)
	}
	b.ResetTimer()
	for iteration := range b.N {
		b.StopTimer()
		for writer, local := range locals {
			for i := range writerSessions[writer] {
				row := &writerSessions[writer][i]
				row.FirstMessage = new(fmt.Sprintf("Updated preview %d", iteration))
				require.NoError(b, local.UpsertSession(b.Context(), *row))
			}
		}
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
