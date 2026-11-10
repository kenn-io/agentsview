package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func BenchmarkCodexStagingToolResult(b *testing.B) {
	sink, err := newCodexStagingSink(b.Context(), b.TempDir(), nil)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, sink.Close()) })
	ids := make([]string, b.N)
	for i := range ids {
		ids[i] = fmt.Sprintf("call-%d", i)
		sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{
			ToolUseID: ids[i], ToolName: "exec_command", Category: "Bash",
		}}})
	}
	const content = "Synthetic tool result for SQL preparation measurement."
	b.ReportAllocs()
	b.ResetTimer()
	for _, id := range ids {
		sink.AppendToolResultEvent(b.Context(), id, nil, parser.ParsedToolResultEvent{
			ToolUseID: id, Source: "function_call_output", Status: "completed", Content: content,
		})
	}
	b.StopTimer()
	require.NoError(b, sink.Err())
	var count int
	require.NoError(b, sink.scratch.QueryRowContext(b.Context(), `SELECT COUNT(*) FROM stage_events`).Scan(&count))
	require.Equal(b, b.N, count)
	var stored string
	require.NoError(b, sink.scratch.QueryRowContext(b.Context(),
		`SELECT content FROM stage_events WHERE seq = 1`).Scan(&stored))
	require.Equal(b, content, stored)
}

func BenchmarkReconciliationSpoolAdd(b *testing.B) {
	dir := b.TempDir()
	spool, err := newReconciliationSpool(b.Context(), filepath.Join(dir, "archive.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, spool.CloseAndRemove(context.WithoutCancel(b.Context()))) })
	candidates := make([]reconciliationCandidate, b.N)
	for i := range candidates {
		id := fmt.Sprintf("session-%06d", i)
		candidates[i] = reconciliationCandidate{
			Provider: parser.AgentCodex, Identity: id, Path: filepath.Join(dir, id+".jsonl"),
			StoredPath: id, WatchRoot: dir, Machine: "local", Project: "sample", Preference1: 1,
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for _, candidate := range candidates {
		require.NoError(b, spool.Add(b.Context(), candidate))
	}
	b.StopTimer()
	var count int
	require.NoError(b, spool.db.QueryRowContext(b.Context(), `SELECT COUNT(*) FROM candidates`).Scan(&count))
	require.Equal(b, b.N, count)
	candidate, ok, err := spool.Candidate(b.Context(), parser.AgentCodex, candidates[0].Identity)
	require.NoError(b, err)
	require.True(b, ok)
	require.Equal(b, candidates[0].Path, candidate.Path)
}

func BenchmarkReconciliationSpoolContainsSource(b *testing.B) {
	spool, err := newReconciliationSpool(b.Context(), filepath.Join(b.TempDir(), "archive.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, spool.CloseAndRemove(context.WithoutCancel(b.Context()))) })
	for i := range 1000 {
		id := fmt.Sprintf("session-%06d", i)
		require.NoError(b, spool.Add(b.Context(), reconciliationCandidate{
			Provider: parser.AgentCodex, Identity: id, Path: id, StoredPath: id,
		}))
	}
	ids := make([]string, b.N)
	for i := range ids {
		ids[i] = fmt.Sprintf("session-%06d", i%1000)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for _, id := range ids {
		found, err := spool.ContainsSource(b.Context(), parser.AgentCodex, id)
		require.NoError(b, err)
		require.True(b, found)
	}
}
