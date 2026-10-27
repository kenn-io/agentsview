//go:build !(windows && arm64)

package duckdb

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func BenchmarkMirrorPush(b *testing.B) {
	ctx := b.Context()
	local, err := db.OpenIsolatedContext(ctx, filepath.Join(b.TempDir(), "archive.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, local.Close()) })
	require.NoError(b, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "benchmark-model"}}))
	const sessionCount, messagesPerSession = 100, 40
	const ts = "2026-01-01T00:00:00.000Z"
	writes := make([]db.SessionBatchWrite, 0, sessionCount)
	for i := range sessionCount {
		id := fmt.Sprintf("benchmark-%03d", i)
		msgs := make([]db.Message, 0, messagesPerSession)
		for ordinal := range messagesPerSession {
			role := "user"
			var calls []db.ToolCall
			if ordinal%2 == 1 {
				role = "assistant"
				for call := range 2 {
					useID := fmt.Sprintf("call-%d-%d-%d", i, ordinal, call)
					result := strings.Repeat("generated result payload ", 40)
					calls = append(calls, db.ToolCall{
						ToolName: "Bash", Category: "other", ToolUseID: useID,
						InputJSON: `{"command":"echo fixture"}`, ResultContent: result,
						ResultContentLength: len(result),
						ResultEvents: []db.ToolResultEvent{{
							ToolUseID: useID, Source: "tool_result", Status: "completed",
							Content: result, ContentLength: len(result), Timestamp: ts,
						}},
					})
				}
			}
			msgs = append(msgs, syncMessage(id, ordinal, role,
				strings.Repeat("generated message payload ", 20), ts, calls...))
		}
		writes = append(writes, db.SessionBatchWrite{
			Session:  syncSession(id, "benchmark", "generated fixture", ts, messagesPerSession),
			Messages: msgs, DataVersion: 1, ReplaceMessages: true,
			UsageEvents: []db.UsageEvent{{
				SessionID: id, Source: "fixture", Model: "fixture-model", OccurredAt: ts,
				InputTokens: 10, OutputTokens: 20, DedupKey: "fixture-usage",
			}},
		})
	}
	_, err = local.WriteSessionBatchAtomic(ctx, writes)
	require.NoError(b, err)
	for b.Loop() {
		b.StopTimer()
		path := filepath.Join(b.TempDir(), "mirror.duckdb")
		b.StartTimer()
		result, err := Push(ctx, path, local, "benchmark-machine", storage.MirrorPushOptions{}, false, nil)
		b.StopTimer()
		require.NoError(b, err)
		require.Zero(b, result.Errors)
		require.Equal(b, sessionCount, result.SessionsPushed)
		require.Equal(b, sessionCount*messagesPerSession, result.MessagesPushed)
		func() {
			mirror, err := OpenReadOnly(ctx, path)
			require.NoError(b, err)
			defer func() { require.NoError(b, mirror.Close()) }()
			for _, table := range []struct {
				name string
				want int
			}{
				{"sessions", 100},
				{"messages", 4000},
				{"tool_calls", 4000},
				{"tool_result_events", 4000},
				{"usage_events", 100},
			} {
				var count int
				require.NoError(b, mirror.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(&count))
				assert.Equal(b, table.want, count, table.name)
			}
		}()
		b.StartTimer()
	}
}
