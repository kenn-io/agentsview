//go:build chtest

package clickhouse

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
)

func TestMain(m *testing.M) {
	code := m.Run()
	chtest.Terminate()
	os.Exit(code)
}

const (
	fixtureAlphaID = "ch-sync-alpha"
	fixtureBetaID  = "ch-sync-beta"
	fixtureChildID = "ch-sync-alpha-child"
	fixtureSecret  = "secret token sk-clickhouse"
	fixtureMachine = "test-machine"
)

// seedFixture opens a SQLite archive with two root sessions (alpha in
// project "alpha" with a tool call, result event, usage event, secret
// finding, star and pin; beta in project "beta") plus a subagent child of
// alpha, and returns a Target pointing at a fresh ClickHouse database.
func seedFixture(t *testing.T) (*db.DB, Target) {
	t.Helper()
	local := dbtest.OpenTestDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "claude-test",
		InputPerMTok:         money.MustParseDollars("3"),
		OutputPerMTok:        money.MustParseDollars("15"),
		CacheCreationPerMTok: money.MustParseDollars("1"),
		CacheReadPerMTok:     money.MustParseDollars("0.5"),
	}}))
	alphaPath := filepath.Join(t.TempDir(), "alpha.jsonl")
	callIndex := 0
	alpha := fixtureSession(fixtureAlphaID, "alpha", "alpha first", "2026-01-10T00:00:00.000Z", 2)
	alpha.FilePath = &alphaPath
	alpha.GitBranch = "main"
	child := fixtureSession(fixtureChildID, "alpha", "child first", "2026-01-10T00:05:00.000Z", 1)
	parent := fixtureAlphaID
	child.ParentSessionID = &parent
	child.RelationshipType = "subagent"
	writes := []db.SessionBatchWrite{
		{
			Session: alpha,
			Messages: []db.Message{
				fixtureMessage(fixtureAlphaID, 0, "user", "alpha first", "2026-01-10T00:00:00.000Z"),
				fixtureMessage(fixtureAlphaID, 1, "assistant", fixtureSecret, "2026-01-10T00:01:00.000Z",
					db.ToolCall{
						ToolName:  "search",
						Category:  "search",
						SkillName: "ch-search",
						ToolUseID: "tool-alpha",
						InputJSON: `{"query":"clickhouse"}`,
						ResultEvents: []db.ToolResultEvent{{
							Source:        "tool",
							Status:        "complete",
							Content:       "clickhouse result",
							Timestamp:     "2026-01-10T00:01:30.000Z",
							EventIndex:    0,
							ContentLength: len("clickhouse result"),
						}},
					}),
			},
			UsageEvents: []db.UsageEvent{{
				Source:       "hermes",
				Model:        "claude-test",
				InputTokens:  10,
				OutputTokens: 5,
				OccurredAt:   "2026-01-10T00:02:00.000Z",
				DedupKey:     "alpha-usage",
			}},
			Findings: []db.SecretFinding{{
				SessionID:      fixtureAlphaID,
				RuleName:       "test_secret",
				Confidence:     "definite",
				LocationKind:   "message",
				MessageOrdinal: 1,
				CallIndex:      &callIndex,
				MatchStart:     len("secret token "),
				MatchEnd:       len(fixtureSecret),
				RedactedMatch:  "sk-clickhouse...",
				RulesVersion:   "test-rules",
			}},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: fixtureSession(fixtureBetaID, "beta", "beta first", "2026-01-11T00:00:00.000Z", 1),
			Messages: []db.Message{
				fixtureMessage(fixtureBetaID, 0, "user", "beta first", "2026-01-11T00:00:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: child,
			Messages: []db.Message{
				fixtureMessage(fixtureChildID, 0, "user", "child first", "2026-01-10T00:05:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	ok, err := local.StarSession(fixtureAlphaID)
	require.NoError(t, err)
	require.True(t, ok)
	msgs, err := local.GetAllMessages(context.Background(), fixtureAlphaID)
	require.NoError(t, err)
	note := "pin alpha"
	_, err = local.PinMessage(fixtureAlphaID, msgs[0].ID, &note)
	require.NoError(t, err)

	dsn, database := chtest.FreshDatabase(t)
	return local, Target{URL: dsn, Database: database}
}

func fixtureSession(id, project, first, ts string, messageCount int) db.Session {
	firstValue := first
	startedAt := ts
	endedAt := ts
	localModifiedAt := ts
	transcriptRevision := "1"
	return db.Session{
		ID:                 id,
		Project:            project,
		Machine:            "local",
		Agent:              "claude",
		FirstMessage:       &firstValue,
		StartedAt:          &startedAt,
		EndedAt:            &endedAt,
		CreatedAt:          ts,
		LocalModifiedAt:    &localModifiedAt,
		TranscriptRevision: &transcriptRevision,
		MessageCount:       messageCount,
		UserMessageCount:   1,
		RelationshipType:   "root",
		Outcome:            "success",
		OutcomeConfidence:  "high",
		EndedWithRole:      "assistant",
		DataVersion:        1,
	}
}

func fixtureMessage(
	sessionID string, ordinal int, role, content, ts string, calls ...db.ToolCall,
) db.Message {
	return db.Message{
		SessionID:        sessionID,
		Ordinal:          ordinal,
		Role:             role,
		Content:          content,
		Timestamp:        ts,
		ContentLength:    len(content),
		HasToolUse:       len(calls) > 0,
		ToolCalls:        calls,
		Model:            "claude-test",
		TokenUsage:       []byte(`{"input_tokens":1,"output_tokens":2}`),
		ContextTokens:    1,
		OutputTokens:     2,
		HasContextTokens: true,
		HasOutputTokens:  true,
	}
}

// newTestSync runs EnsureSchema so tests can push immediately.
func newTestSync(t *testing.T, local *db.DB, target Target, opts SyncOptions) *Sync {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, target, local, fixtureMachine, opts)
	require.NoError(t, err)
	require.NoError(t, s.EnsureSchema(ctx))
	t.Cleanup(func() { s.Close() })
	return s
}

// appendMessage adds one assistant message to a session locally and bumps
// its message count so the session becomes an incremental candidate.
func appendMessage(t *testing.T, local *db.DB, sessionID, content, ts string) {
	t.Helper()
	ctx := context.Background()
	sess, err := local.GetSessionFull(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	msgs, err := local.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	msgs = append(msgs, fixtureMessage(sessionID, len(msgs), "assistant", content, ts))
	sess.MessageCount = len(msgs)
	sess.EndedAt = &ts
	sess.LocalModifiedAt = &ts
	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         *sess,
		Messages:        msgs,
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
}
