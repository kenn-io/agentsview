package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	session         *db.Session
	err             error
}

func (s *providerHistoryReadSpy) GetSessionFull(
	context.Context, string,
) (*db.Session, error) {
	s.getSessionCalls++
	return s.session, nil
}

func (s *providerHistoryReadSpy) GetAllMessages(
	context.Context, string,
) ([]db.Message, error) {
	s.getMessageCalls++
	return nil, s.err
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

// TestReconcileProviderHistoryPropagatesTransientArchiveReadError covers a
// transient failure reading the archived session: it must stop the write
// rather than silently proceeding as if no archived output existed, which
// would let the current parse's placeholder overwrite it.
func TestReconcileProviderHistoryPropagatesTransientArchiveReadError(t *testing.T) {
	readErr := errors.New("transient read failure")
	archive := &providerHistoryReadSpy{
		session: &db.Session{ID: "claude:session-1"}, err: readErr,
	}
	engine := &Engine{db: openTestDB(t), archiveStore: archive}
	candidate := ingest.Candidate{
		Session: db.Session{ID: "claude:session-1", Agent: string(parser.AgentClaude)},
		Parsed: parser.ParseResult{
			Session: parser.ParsedSession{
				ID: "session-1", Agent: parser.AgentClaude,
				File: parser.FileInfo{Path: "/synthetic/project/session-1.jsonl"},
			},
			Messages: []parser.ParsedMessage{{
				Ordinal: 0, Timestamp: time.Now(),
				ToolResults: []parser.ParsedToolResult{{
					ToolUseID: "toolu_1", Source: "tool_result",
					PersistedOutputUnresolved: true,
				}},
			}},
		},
	}

	_, err := engine.reconcileProviderHistoryContext(t.Context(), candidate)
	require.Error(t, err)
	assert.ErrorIs(t, err, readErr)
}

// TestClaudeForcedReparseKeepsArchivedToolResultAfterSidecarDeleted covers a
// local Claude Code session whose large tool output moved to a tool-results
// side file: the first sync archives the real output, and a later forced
// full reparse, run after Claude Code deletes the side file, must keep the
// archived output rather than store the placeholder over it.
func TestClaudeForcedReparseKeepsArchivedToolResultAfterSidecarDeleted(
	t *testing.T,
) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	sessionPath := filepath.Join(projectDir, "parent-session.jsonl")
	sidecarPath := filepath.Join(
		projectDir, "parent-session", "tool-results", "output.txt",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(sidecarPath), 0o755))
	require.NoError(t, os.WriteFile(
		sidecarPath, []byte("full persisted output\n"), 0o644,
	))

	placeholder := "<persisted-output>\nOutput too large. Full output saved to: " +
		sidecarPath + "\n</persisted-output>"
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` +
			mustSyncJSONString(t, placeholder) +
			`}]},"toolUseResult":{"persistedOutputPath":` +
			mustSyncJSONString(t, sidecarPath) + `}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(sessionPath, []byte(content), 0o644))

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	var sessionID string
	require.NoError(t, database.Reader().QueryRow(
		t.Context(), "SELECT id FROM sessions WHERE agent='claude'",
	).Scan(&sessionID))
	initialMessages, err := database.GetAllMessages(
		t.Context(), sessionID,
	)
	require.NoError(t, err)
	require.Len(t, initialMessages, 2)
	require.Len(t, initialMessages[1].ToolCalls, 1)
	require.Equal(t, "full persisted output\n",
		initialMessages[1].ToolCalls[0].ResultContent)

	require.NoError(t, os.RemoveAll(filepath.Dir(filepath.Dir(sidecarPath))))

	stats := engine.SyncAllForceParse(t.Context(), nil)
	require.Zero(t, stats.Failed)
	messages, err := database.GetAllMessages(
		t.Context(), sessionID,
	)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "full persisted output\n",
		messages[1].ToolCalls[0].ResultContent)
}
