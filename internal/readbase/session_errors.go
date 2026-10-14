package readbase

import "fmt"

type ReadOperation string

const (
	QueryMessages         ReadOperation = "querying %s messages"
	QueryAroundMessages   ReadOperation = "querying %s around-window messages"
	ScanAllMessages       ReadOperation = "scanning all %s messages"
	QueryAllMessages      ReadOperation = "querying all %s messages"
	QueryToolCalls        ReadOperation = "querying %s tool calls"
	ScanToolCall          ReadOperation = "scanning %s tool call"
	QueryToolResultEvents ReadOperation = "querying %s tool result events"
	ScanToolResultEvent   ReadOperation = "scanning %s tool result event"
	ListStarredSessions   ReadOperation = "listing %s starred sessions"
	ListPinnedMessages    ReadOperation = "listing %s pinned messages"
	ReadMachineMetadata   ReadOperation = "reading %s machine metadata"
	ListFindings          ReadOperation = "listing %s findings"
)

func (operation ReadOperation) Wrap(backend string, err error) error {
	if operation == ScanAllMessages {
		operation = QueryAllMessages
	}
	return fmt.Errorf(string(operation)+": %w", backend, err)
}
