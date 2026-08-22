package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cancelOnErrCheckContext struct {
	context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	remaining int
}

func newCancelOnErrCheckContext(
	t *testing.T, checks int,
) context.Context {
	t.Helper()
	base, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &cancelOnErrCheckContext{
		Context: base, cancel: cancel, remaining: checks,
	}
}

func (ctx *cancelOnErrCheckContext) Err() error {
	ctx.mu.Lock()
	if ctx.remaining > 0 {
		ctx.remaining--
		if ctx.remaining == 0 {
			ctx.cancel()
		}
	}
	ctx.mu.Unlock()
	return ctx.Context.Err()
}

func TestJSONLProviderFingerprintStopsAfterContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(
		path, []byte(strings.Repeat("x", 256*1024)), 0o600,
	))
	for _, agent := range []AgentType{AgentClaude, AgentCodex} {
		t.Run(string(agent), func(t *testing.T) {
			provider, ok := NewProvider(agent, ProviderConfig{})
			require.True(t, ok)
			ctx := newCancelOnErrCheckContext(t, 4)

			_, err := provider.Fingerprint(ctx, SourceRef{
				Provider: agent,
				Opaque:   MaterializedFileSource{Path: path},
			})

			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestClaudeCanceledHeadSniffIsNotCached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fork.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(
		`{"type":"user","uuid":"root","parentUuid":null,"sessionKind":"bg"}`+"\n",
	), 0o600))

	_, err := claudeSniffHead(newCancelOnErrCheckContext(t, 3), path)
	require.ErrorIs(t, err, context.Canceled)

	sniff, err := claudeSniffHead(t.Context(), path)
	require.NoError(t, err)
	assert.True(t, sniff.ok)
	assert.True(t, sniff.rootIsBG)
	assert.Equal(t, "root", sniff.rootUUID)
}

func TestClaudeDAGPostProcessingStopsAfterContextCancellation(t *testing.T) {
	entries := make([]dagEntry, 1024)
	for i := range entries {
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("entry-%d", i-1)
		}
		entries[i] = dagEntry{
			uuid: fmt.Sprintf("entry-%d", i), parentUuid: parent,
			entryType: "user", line: `{"type":"user","message":{"content":"x"}}`,
		}
	}
	ctx := newCancelOnErrCheckContext(t, 2)

	_, err := parseDAG(
		ctx, entries, "session", "project", "machine", "",
		FileInfo{}, nil, time.Time{}, time.Time{}, claudeSessionMeta{},
	)

	require.ErrorIs(t, err, context.Canceled)
}

func TestCodexPostReadNormalizationStopsAfterContextCancellation(t *testing.T) {
	builder := newCodexSessionBuilder(context.Background(), false, nil)
	for i := 1024; i > 0; i-- {
		builder.messages = append(builder.messages, ParsedMessage{Ordinal: i})
	}
	ctx := newCancelOnErrCheckContext(t, 3)

	err := builder.normalizeOrdinalsContext(ctx)

	require.ErrorIs(t, err, context.Canceled)
}

func TestClaudeWebSearchCollectionStopsAfterContextCancellation(t *testing.T) {
	entries := make([]dagEntry, 1024)
	for i := range entries {
		entries[i] = dagEntry{
			entryType: "user",
			line:      `{"message":{"content":[]},"toolUseResult":{"searchCount":1}}`,
		}
	}

	_, err := collectClaudeWebSearchCountsContext(
		newCancelOnErrCheckContext(t, 2), entries,
	)

	require.ErrorIs(t, err, context.Canceled)
}

func TestClaudeWebSearchAnnotationStopsAfterContextCancellation(t *testing.T) {
	messages := make([]ParsedMessage, 1024)
	for i := range messages {
		messages[i] = ParsedMessage{
			TokenUsage: json.RawMessage(`{"input_tokens":1,"output_tokens":1}`),
			ToolCalls: []ParsedToolCall{{
				ToolName:  claudeWebSearchToolName,
				ToolUseID: "search",
			}},
		}
	}

	err := annotateClaudeWebSearchRequestsContext(
		newCancelOnErrCheckContext(t, 2), messages,
		map[string]int{"search": 1},
	)

	require.ErrorIs(t, err, context.Canceled)
}

func TestUsageEventTokenAggregateStopsAfterContextCancellation(t *testing.T) {
	events := make([]ParsedUsageEvent, 1024)

	_, _, _, _, err := UsageEventTokenAggregateContext(
		newCancelOnErrCheckContext(t, 2), events,
	)

	require.ErrorIs(t, err, context.Canceled)
}

func TestTokenCoverageStopsAfterContextCancellation(t *testing.T) {
	messages := make([]ParsedMessage, 1024)

	_, _, err := (ParsedSession{}).TokenCoverageContext(
		newCancelOnErrCheckContext(t, 2), messages,
	)

	require.ErrorIs(t, err, context.Canceled)
}
