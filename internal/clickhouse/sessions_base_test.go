package clickhouse

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/readbase"
)

func TestSessionReadErrors(t *testing.T) {
	cause := errors.New("read failed")
	backend := sessionsSQL{Store: &Store{}}
	tests := []struct {
		operation readbase.ReadOperation
		text      string
	}{
		{readbase.QueryMessages, "querying clickhouse messages: read failed"},
		{readbase.QueryAroundMessages, "querying clickhouse around-window messages: read failed"},
		{readbase.QueryAllMessages, "querying all clickhouse messages: read failed"},
		{readbase.ScanAllMessages, "querying all clickhouse messages: read failed"},
		{readbase.QueryToolCalls, "querying clickhouse tool calls: read failed"},
		{readbase.ScanToolCall, "scanning clickhouse tool call: read failed"},
		{readbase.QueryToolResultEvents, "querying clickhouse tool result events: read failed"},
		{readbase.ScanToolResultEvent, "scanning clickhouse tool result event: read failed"},
		{readbase.ListStarredSessions, "listing clickhouse starred sessions: read failed"},
		{readbase.ListPinnedMessages, "listing clickhouse pinned messages: read failed"},
		{readbase.ReadMachineMetadata, "reading clickhouse machine metadata: read failed"},
		{readbase.ListFindings, "listing clickhouse findings: read failed"},
	}
	for _, tt := range tests {
		t.Run(string(tt.operation), func(t *testing.T) {
			err := backend.ReadError(tt.operation, cause)
			require.ErrorIs(t, err, cause)
			require.EqualError(t, err, tt.text)
		})
	}
}
