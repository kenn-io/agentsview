package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestHermesRejectedReplacementDoesNotBlockHealthyBatch(t *testing.T) {
	for _, mode := range []syncWriteMode{syncWriteDefault, syncWriteBulk} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			root := t.TempDir()
			stateDB := writeHermesArchiveStateDB(t, root)
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local"})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			before, err := database.GetAllMessages(t.Context(), "hermes:child")
			require.NoError(t, err)
			writes := []pendingWrite{
				{sess: parser.ParsedSession{ID: "hermes:child", Agent: parser.AgentHermes, SourceVersion: "hermes-json-v1", File: parser.FileInfo{Path: parser.VirtualSourcePath(stateDB, "child")}}, msgs: []parser.ParsedMessage{{Role: "user", Content: "conflicting snapshot"}}, replacement: parser.ArchiveReplacementPolicy{Content: parser.ContentReplaceSameSource}, forceReplace: true},
				{sess: parser.ParsedSession{ID: "healthy", Agent: parser.AgentClaude, File: parser.FileInfo{Path: filepath.Join(root, "healthy.jsonl")}}, msgs: []parser.ParsedMessage{{Role: "user", Content: "healthy update"}}, forceReplace: true},
			}
			outcome := engine.writeBatchWithOutcomeContext(t.Context(), writes, mode, false)
			require.ErrorContains(t, outcome.err, "cannot replace archived content")
			assert.Equal(t, 1, outcome.failedSessions)
			assert.Equal(t, 1, outcome.writtenSessions)
			after, err := database.GetAllMessages(t.Context(), "hermes:child")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			healthy, err := database.GetAllMessages(t.Context(), "healthy")
			require.NoError(t, err)
			contents := make([]string, len(healthy))
			for i, message := range healthy {
				contents[i] = message.Content
			}
			assert.Equal(t, []string{"healthy update"}, contents)
		})
	}
}

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
		t.Context(), candidate,
	)
	require.NoError(t, err)
	assert.Equal(t, ingest.HistoryReplace, result.Action)
	assert.Zero(t, archive.getSessionCalls)
	assert.Zero(t, archive.getMessageCalls)
}
