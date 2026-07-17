package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareSessionForSharedStorage(t *testing.T) {
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	literal := "literal gAAAAA-not-an-encrypted-token"
	mixedPreview := "note " + fernet + " tail"
	pureTokenPreview := fernet
	controlSplitToken := fernet[:3] + "\x00" + fernet[3:]
	invalidSplitToken := fernet[:6] + "\xff" + fernet[6:]
	controlAdjacentToken := fernet + "\x01"
	tests := []struct {
		name     string
		session  Session
		messages []Message
		wantErr  bool
	}{
		{
			name: "verified subagent literal",
			session: Session{
				ID: "literal", Agent: "codex", DataVersion: 64,
				RelationshipType: "subagent", FirstMessage: &literal,
			},
			messages: []Message{{Ordinal: 0, Role: "user", Content: literal}},
		},
		{
			name: "encrypted subagent message",
			session: Session{
				ID: "encrypted-message", Agent: "codex", DataVersion: 64,
				RelationshipType: "subagent",
			},
			messages: []Message{{Ordinal: 0, Role: "user", Content: fernet}},
			wantErr:  true,
		},
		{
			name: "encrypted collaboration input",
			session: Session{
				ID: "encrypted-tool", Agent: "codex", DataVersion: 64,
			},
			messages: []Message{{
				Ordinal: 0, Role: "assistant", Content: "[Task: spawn_agent]\n" + fernet,
				ToolCalls: []ToolCall{{ToolName: "spawn_agent", InputJSON: fernet}},
			}},
			wantErr: true,
		},
		{
			name: "unlinked child encrypted delivery",
			session: Session{
				ID: "unlinked-child", Agent: "codex", DataVersion: 64,
			},
			messages: []Message{{Ordinal: 0, Role: "user", Content: fernet}},
			wantErr:  true,
		},
		{
			name: "tool message with lost tool_calls row",
			session: Session{
				ID: "lost-tool-call", Agent: "codex", DataVersion: 64,
			},
			messages: []Message{{
				Ordinal: 1, Role: "assistant", HasToolUse: true,
				Content: "[Task: spawn_agent]\n" + fernet,
			}},
			wantErr: true,
		},
		{
			name: "formatted collab block with stale flag and lost row",
			session: Session{
				ID: "stale-missing-tool-call", Agent: "codex", DataVersion: 67,
			},
			messages: []Message{{
				Ordinal: 1, Role: "assistant", HasToolUse: false,
				Content: "[Task: spawn_agent]\n" + fernet,
			}},
			wantErr: true,
		},
		{
			name: "legacy encrypted collaboration header",
			session: Session{
				ID: "encrypted-header", Agent: "codex", DataVersion: 67,
			},
			messages: []Message{{
				Ordinal: 1, Role: "assistant", HasToolUse: false,
				Content: "[Task: " + fernet + "]\nRun the task",
			}},
			wantErr: true,
		},
		{
			name: "formatted non-collab block with stale flag and no row",
			session: Session{
				ID: "stale-non-collab", Agent: "codex", DataVersion: 67,
			},
			messages: []Message{{
				Ordinal: 1, Role: "assistant", HasToolUse: false,
				Content: "[Bash: decrypt]\n$ decrypt " + fernet,
			}},
		},
		{
			name: "pure token user message on root session",
			session: Session{
				ID: "root-pure-token", Agent: "codex", DataVersion: 64,
				RelationshipType: "root",
			},
			messages: []Message{{Ordinal: 0, Role: "user", Content: fernet}},
			wantErr:  true,
		},
		{
			name: "root user mixed quote",
			session: Session{
				ID: "root-quote", Agent: "codex", DataVersion: 64,
				RelationshipType: "root",
			},
			messages: []Message{{
				Ordinal: 0, Role: "user",
				Content: "the token was " + fernet + " according to the log",
			}},
		},
		{
			name: "pure token preview on unlinked child",
			session: Session{
				ID: "unlinked-preview", Agent: "codex", DataVersion: 64,
				FirstMessage: &pureTokenPreview,
			},
			wantErr: true,
		},
		{
			name: "mixed literal preview",
			session: Session{
				ID: "mixed-preview", Agent: "codex", DataVersion: 64,
				FirstMessage: &mixedPreview,
			},
		},
		{
			name: "known non-collab tool token",
			session: Session{
				ID: "non-collab-tool", Agent: "codex", DataVersion: 64,
			},
			messages: []Message{{
				Ordinal: 0, Role: "assistant", HasToolUse: true,
				Content:   "[Bash: decrypt]\n$ decrypt " + fernet,
				ToolCalls: []ToolCall{{ToolName: "Bash", InputJSON: fernet}},
			}},
		},
		{
			name: "lost collab row with surviving non-collab tool",
			session: Session{
				ID: "mixed-tool-residue", Agent: "codex", DataVersion: 67,
			},
			messages: []Message{{
				Ordinal: 0, Role: "assistant", HasToolUse: true,
				Content:   "[Task: spawn_agent]\n" + fernet,
				ToolCalls: []ToolCall{{ToolName: "Bash", InputJSON: `{}`}},
			}},
			wantErr: true,
		},
		{
			name: "collab row with stale tool-use flag",
			session: Session{
				ID: "stale-tool-flag", Agent: "codex", DataVersion: 67,
			},
			messages: []Message{{
				Ordinal: 0, Role: "assistant", HasToolUse: false,
				Content: "[Task: spawn_agent]\n" + fernet,
				ToolCalls: []ToolCall{{
					ToolName: "spawn_agent", InputJSON: `{}`,
				}},
			}},
			wantErr: true,
		},
		{
			name: "control byte inside encrypted preview",
			session: Session{
				ID: "sanitized-preview", Agent: "codex", DataVersion: 67,
				FirstMessage: &controlSplitToken,
			},
			wantErr: true,
		},
		{
			name: "invalid byte inside encrypted message",
			session: Session{
				ID: "sanitized-message", Agent: "codex", DataVersion: 67,
			},
			messages: []Message{{
				Ordinal: 0, Role: "user", Content: invalidSplitToken,
			}},
			wantErr: true,
		},
		{
			name: "control byte adjacent to encrypted tool input",
			session: Session{
				ID: "sanitized-tool-input", Agent: "codex", DataVersion: 67,
			},
			messages: []Message{{
				Ordinal: 0, Role: "assistant", Content: "safe",
				ToolCalls: []ToolCall{{
					ToolName: "spawn_agent", InputJSON: controlAdjacentToken,
				}},
			}},
			wantErr: true,
		},
		{
			name: "sanitized user role exposes encrypted delivery",
			session: Session{
				ID: "sanitized-role", Agent: "codex", DataVersion: 67,
			},
			messages: []Message{{
				Ordinal: 0, Role: "us\x00er", Content: fernet,
			}},
			wantErr: true,
		},
		{
			name: "sanitized collaboration tool name exposes encrypted input",
			session: Session{
				ID: "sanitized-tool-name", Agent: "codex", DataVersion: 67,
			},
			messages: []Message{{
				Ordinal: 0, Role: "assistant", Content: "safe",
				ToolCalls: []ToolCall{{
					ToolName: "spawn_\x00agent", InputJSON: fernet,
				}},
			}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PrepareSessionForSharedStorage(tt.session, tt.messages)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrCodexSessionUnverified)
				assert.Equal(t, tt.session.DataVersion, got.DataVersion)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, redactedCodexSourceDataVersion, got.DataVersion)
		})
	}
}

func TestUnverifiedCodexSessionIDs(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	sessions := []Session{
		{ID: "codex-legacy", Agent: "codex", DataVersion: 64},
		{ID: "codex-current", Agent: "codex",
			DataVersion: redactedCodexSourceDataVersion},
		{ID: "claude-legacy", Agent: "claude", DataVersion: 64},
	}
	for _, sess := range sessions {
		sess.CreatedAt = "2026-07-14T12:00:00Z"
		require.NoError(t, database.UpsertSession(sess))
		// UpsertSession seeds data_version 0; stamp the intended version.
		require.NoError(t, database.SetSessionDataVersion(sess.ID, sess.DataVersion))
	}

	ids, err := database.UnverifiedCodexSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"codex-legacy"}, ids,
		"only codex sessions below the watermark are unverified")

	legacy, err := database.GetSession(ctx, "codex-legacy")
	require.NoError(t, err)
	require.NotNil(t, legacy)
	require.NotNil(t, legacy.TranscriptRevision)
	require.NoError(t, database.SetCodexSharedStorageCertification(
		legacy.ID, *legacy.TranscriptRevision, legacy.FirstMessage,
	))
	assert.Equal(t, 64, database.GetSessionDataVersion(legacy.ID),
		"payload certification must not claim unrelated parser migrations ran")
	ids, err = database.UnverifiedCodexSessionIDs(ctx)
	require.NoError(t, err)
	assert.Empty(t, ids, "the matching transcript revision is certified")

	preview := "changed preview"
	legacy.FirstMessage = &preview
	require.NoError(t, database.UpsertSession(*legacy))
	ids, err = database.UnverifiedCodexSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{legacy.ID}, ids,
		"a preview change must invalidate payload certification")
	require.Error(t, database.SetCodexSharedStorageCertification(
		legacy.ID, *legacy.TranscriptRevision, nil,
	), "the previously verified preview must fail closed after a concurrent change")
	legacy, err = database.GetSession(ctx, legacy.ID)
	require.NoError(t, err)
	require.NotNil(t, legacy)
	require.NotNil(t, legacy.TranscriptRevision)
	require.NoError(t, database.SetCodexSharedStorageCertification(
		legacy.ID, *legacy.TranscriptRevision, legacy.FirstMessage,
	))

	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: legacy.ID,
		Ordinal:   0,
		Role:      "assistant",
		Content:   "new content",
	}}))
	ids, err = database.UnverifiedCodexSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{legacy.ID}, ids,
		"a transcript change must invalidate the content-bound certification")
	require.Error(t, database.SetCodexSharedStorageCertification(
		legacy.ID, *legacy.TranscriptRevision, legacy.FirstMessage,
	), "a stale revision must fail closed")
}
