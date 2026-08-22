package db

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type usageCountdownContext struct {
	context.Context
	remaining atomic.Int64
	done      chan struct{}
	cancel    sync.Once
}

func newUsageCountdownContext(checks int64) *usageCountdownContext {
	ctx := &usageCountdownContext{
		Context: context.Background(), done: make(chan struct{}),
	}
	ctx.remaining.Store(checks)
	return ctx
}

func (c *usageCountdownContext) Err() error {
	if c.remaining.Add(-1) < 0 {
		c.cancel.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}

func (c *usageCountdownContext) Done() <-chan struct{} { return c.done }

func TestGetSessionUsageStopsDuringInMemoryAggregation(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "usage-context", "project", func(session *Session) {
		session.Agent = "claude"
	})
	messages := make([]Message, 512)
	for i := range messages {
		messages[i] = Message{
			SessionID: "usage-context", Ordinal: i, Role: "assistant",
			Timestamp: "2026-08-19T12:00:00Z", Model: "test-model",
			TokenUsage: json.RawMessage(`{"input_tokens":1,"output_tokens":1}`),
		}
	}
	insertMessages(t, database, messages...)

	usage, err := database.GetSessionUsage(
		newUsageCountdownContext(32), "usage-context", true,
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, usage)
}

func TestGetSessionUsageRowsStopsDuringInMemoryAggregation(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "rows-context", "project", func(session *Session) {
		session.Agent = "claude"
	})
	messages := make([]Message, 512)
	for i := range messages {
		messages[i] = Message{
			SessionID: "rows-context", Ordinal: i, Role: "assistant",
			Timestamp: "2026-08-19T12:00:00Z", Model: "test-model",
			TokenUsage: json.RawMessage(`{"input_tokens":1,"output_tokens":1}`),
		}
	}
	insertMessages(t, database, messages...)

	rows, err := database.GetSessionUsageRows(
		newUsageCountdownContext(32), []string{"rows-context"},
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, rows)
}
