package ingest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestReconcileProviderHistoryLazyContent(t *testing.T) {
	const fingerprint = `opencode-storage:v1:{"messages":[{"id":"message-1","time":1,"hash":"old"}]}`
	storedPath := "/synthetic/storage/session/session-1.json"
	stored := ingest.PriorSession{
		Session:  db.Session{SessionName: new("retained title"), GroupKey: "retained group", FilePath: &storedPath, FileHash: new(fingerprint)},
		Messages: []db.Message{{Ordinal: 0, Role: "assistant", SourceUUID: "message-1", Content: "complete answer", ContentLength: len("complete answer")}},
	}
	candidate := func(agent parser.AgentType, messages bool) ingest.Candidate {
		c := ingest.Candidate{
			Session: db.Session{Agent: string(agent), PreserveSessionName: true},
			Parsed:  parser.ParseResult{Session: parser.ParsedSession{Agent: agent, KeepStoredGroupKey: true}},
		}
		if messages {
			c.Messages = []db.Message{{Ordinal: 0, Role: "user", Content: "new prompt"}}
		}
		return c
	}
	claude := candidate(parser.AgentClaude, true)
	claude.Parsed.Session.File.Path = "/synthetic/session.jsonl"
	icodemate := claude
	icodemate.Parsed.Session.Agent = parser.AgentIcodemate
	for _, tc := range []struct {
		name      string
		candidate ingest.Candidate
		loads     int
	}{
		{"codex metadata", candidate(parser.AgentCodex, true), 0},
		{"complete claude", claude, 0},
		{"claude format dispatch", icodemate, 0},
		{"nonempty roo", candidate(parser.AgentRooCode, true), 0},
		{"empty roo", candidate(parser.AgentRooCode, false), 1},
		{"empty kilo", candidate(parser.AgentKiloLegacy, false), 1},
		{"empty cline", candidate(parser.AgentCline, false), 1},
		{"copilot archive", candidate(parser.AgentVSCopilot, false), 1},
		{"opencode archive", openCodeHistoryCandidate("", "short"), 1},
		{"opencode complete fingerprint", openCodeHistoryCandidate(fingerprint, "short"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eager, err := ingest.ReconcileProviderHistory(t.Context(), tc.candidate, &stored, ingest.ContentOptions{})
			require.NoError(t, err)
			lazy := stored
			// A supplied loader is authoritative; stale eager rows must not win.
			lazy.Messages = []db.Message{{Content: "stale content"}}
			calls := 0
			lazy.LoadMessages = func(ctx context.Context) ([]db.Message, error) {
				calls++
				require.NoError(t, ctx.Err())
				return stored.Messages, nil
			}
			got, err := ingest.ReconcileProviderHistory(t.Context(), tc.candidate, &lazy, ingest.ContentOptions{})
			require.NoError(t, err)
			assert.Equal(t, eager, got)
			assert.Equal(t, tc.loads, calls)
			assert.Equal(t, "stale content", lazy.Messages[0].Content, "reconciliation must not mutate retained input")
			if tc.name == "codex metadata" {
				assert.Equal(t, stored.Session.SessionName, got.Candidate.Session.SessionName)
				assert.Equal(t, stored.Session.GroupKey, got.Candidate.Session.GroupKey)
			}
			readErr := errors.New("retained content unavailable")
			lazy.LoadMessages = func(context.Context) ([]db.Message, error) { return nil, readErr }
			_, err = ingest.ReconcileProviderHistory(t.Context(), tc.candidate, &lazy, ingest.ContentOptions{})
			if tc.loads == 0 {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, readErr)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = ingest.ReconcileProviderHistory(ctx, tc.candidate, &lazy, ingest.ContentOptions{})
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}
