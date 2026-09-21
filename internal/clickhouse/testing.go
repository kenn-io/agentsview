//go:build chtest

package clickhouse

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

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
	alphaTitle := "Alpha Saved Title"
	alpha.SessionName = &alphaTitle
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
						ToolName:            "search",
						Category:            "search",
						SkillName:           "ch-search",
						ToolUseID:           "tool-alpha",
						InputJSON:           `{"query":"clickhouse"}`,
						ResultContent:       "clickhouse result",
						ResultContentLength: len("clickhouse result"),
						ResultEvents: []db.ToolResultEvent{{
							Source:        "tool",
							Status:        "complete",
							Content:       "clickhouse result",
							Timestamp:     "2026-01-10T00:01:30.000Z",
							EventIndex:    0,
							ContentLength: len("clickhouse result"),
						}},
					},
					db.ToolCall{
						ToolName:  "Write",
						Category:  "Write",
						ToolUseID: "tool-alpha-write",
						FilePath:  "src/main.go",
						InputJSON: `{"path":"src/main.go"}`,
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
	_, err := local.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err)
	for _, id := range []string{fixtureAlphaID, fixtureBetaID, fixtureChildID} {
		require.NoError(t, local.UpdateSessionSignals(t.Context(), id, db.SessionSignalUpdate{
			Outcome:           "success",
			OutcomeConfidence: "high",
		}))
	}
	ok, err := local.StarSession(t.Context(), fixtureAlphaID)
	require.NoError(t, err)
	require.True(t, ok)
	msgs, err := local.GetAllMessages(context.Background(), fixtureAlphaID)
	require.NoError(t, err)
	note := "pin alpha"
	_, err = local.PinMessage(t.Context(), fixtureAlphaID, msgs[0].ID, &note)
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
func newTestSync(t *testing.T, local *db.DB, target Target, opts storage.PusherOptions) *Sync {
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
	_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:         *sess,
		Messages:        msgs,
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
}

func newPushedStore(t *testing.T) (*Store, *Sync, *db.DB) {
	t.Helper()
	ctx := context.Background()
	local, target := seedFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, syncer, local
}

// TestingNewPushedStore is the chtest fixture for external test packages
// that cannot import package clickhouse's _test.go helpers.
func TestingNewPushedStore(t *testing.T) (*Store, *Sync, *db.DB) {
	return newPushedStore(t)
}

const (
	TestingAlphaID = fixtureAlphaID
	TestingBetaID  = fixtureBetaID
)
