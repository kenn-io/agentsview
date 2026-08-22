package sync

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type cancelAfterErrChecksContext struct {
	context.Context
	remaining atomic.Int64
}

func newCancelAfterErrChecksContext(
	t *testing.T, checks int64,
) context.Context {
	t.Helper()
	ctx := &cancelAfterErrChecksContext{Context: t.Context()}
	ctx.remaining.Store(checks)
	return ctx
}

func (ctx *cancelAfterErrChecksContext) Err() error {
	if ctx.remaining.Add(-1) <= 0 {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func TestPrepareSessionWriteStopsDuringMessageConversion(t *testing.T) {
	msgs := make([]parser.ParsedMessage, 1024)
	for i := range msgs {
		msgs[i] = parser.ParsedMessage{
			Ordinal: i,
			Role:    parser.RoleAssistant,
			ToolCalls: []parser.ParsedToolCall{{
				ToolName: "Read",
			}},
		}
	}
	engine := &Engine{}

	_, _, _, err := engine.prepareSessionWriteContext(
		newCancelAfterErrChecksContext(t, 20),
		pendingWrite{
			sess: parser.ParsedSession{ID: "session", Agent: parser.AgentClaude},
			msgs: msgs,
		},
		nil,
	)

	require.ErrorIs(t, err, context.Canceled)
}

func TestValidateAndSanitizeStopsDuringMessages(t *testing.T) {
	msgs := make([]db.Message, 1024)

	_, err := validateAndSanitizeContext(
		newCancelAfterErrChecksContext(t, 3), nil, msgs, nil,
	)

	require.ErrorIs(t, err, context.Canceled)
}

func TestUsageEventConversionStopsAfterContextCancellation(t *testing.T) {
	events := make([]parser.ParsedUsageEvent, 1024)

	_, _, err := toDBUsageEventsContext(
		newCancelAfterErrChecksContext(t, 20), "session", events,
	)

	require.ErrorIs(t, err, context.Canceled)
}
