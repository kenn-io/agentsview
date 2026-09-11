package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

type providerHistoryReadSpy struct {
	db.Store
	getSessionCalls int
	getMessageCalls int
}

func (s *providerHistoryReadSpy) GetSessionFull(
	context.Context, string,
) (*db.Session, error) {
	s.getSessionCalls++
	return nil, nil
}

func (s *providerHistoryReadSpy) GetAllMessages(
	context.Context, string,
) ([]db.Message, error) {
	s.getMessageCalls++
	return nil, nil
}

func TestReconcileProviderHistorySkipsPriorReadForICodeMateCLI(t *testing.T) {
	archive := &providerHistoryReadSpy{}
	engine := &Engine{db: openTestDB(t), archiveStore: archive}
	candidate := ingest.Candidate{
		Session: db.Session{
			ID: "icodemate:session-1", Agent: string(parser.AgentIcodemate),
		},
		Parsed: parser.ParseResult{Session: parser.ParsedSession{
			ID: "session-1", Agent: parser.AgentIcodemate,
			File: parser.FileInfo{Path: "/synthetic/project/session-1.jsonl"},
		}},
	}

	result, err := engine.reconcileProviderHistoryContext(
		context.Background(), candidate,
	)
	require.NoError(t, err)
	assert.Equal(t, ingest.HistoryReplace, result.Action)
	assert.Zero(t, archive.getSessionCalls)
	assert.Zero(t, archive.getMessageCalls)
}
