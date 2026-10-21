package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeSessionDropsSelfParent(t *testing.T) {
	for _, tc := range []struct {
		name             string
		parent           *string
		parserParent     *string
		wantParent       *string
		wantParserParent *string
	}{
		{
			name:   "self parent cleared",
			parent: Ptr("s1"),
		},
		{
			name:             "self parent falls back to parser parent",
			parent:           Ptr("s1"),
			parserParent:     Ptr("main"),
			wantParent:       Ptr("main"),
			wantParserParent: Ptr("main"),
		},
		{
			name:         "self parser parent cleared",
			parserParent: Ptr("s1"),
		},
		{
			name:         "both self references cleared",
			parent:       Ptr("s1"),
			parserParent: Ptr("s1"),
		},
		{
			name:             "real parent kept",
			parent:           Ptr("main"),
			parserParent:     Ptr("main"),
			wantParent:       Ptr("main"),
			wantParserParent: Ptr("main"),
		},
		{
			name: "nil parent kept",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Session{
				ID: "s1", ParentSessionID: tc.parent,
				ParserParentSessionID: tc.parserParent,
				RelationshipType:      "subagent",
			}
			SanitizeSession(&s)
			assert.Equal(t, tc.wantParent, s.ParentSessionID)
			assert.Equal(t, tc.wantParserParent, s.ParserParentSessionID)
			assert.Equal(t, "subagent", s.RelationshipType,
				"dropping the parent claim must not retag the row")
		})
	}
}

// TestSelfParentNeverPersists pins the contract at every session write
// boundary: a parser or imported artifact that names the session as its own
// parent stores a NULL parent, so the row stays a hierarchy root instead of
// disappearing under itself.
func TestSelfParentNeverPersists(t *testing.T) {
	const origin = "peer-a1b2c3"
	selfParented := func(id string) Session {
		return Session{
			ID: id, Project: "p", Machine: origin, Agent: "claude",
			ParentSessionID: Ptr(id), RelationshipType: "subagent",
		}
	}
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, d *DB) string
	}{
		{
			name: "UpsertSession",
			write: func(t *testing.T, d *DB) string {
				t.Helper()

				require.NoError(t, d.UpsertSession(t.Context(), selfParented("upsert-self")))
				return "upsert-self"
			},
		},
		{
			name: "WriteSessionBatch",
			write: func(t *testing.T, d *DB) string {
				t.Helper()

				_, err := d.WriteSessionBatch([]SessionBatchWrite{{
					Session:  selfParented("batch-self"),
					Messages: []Message{spawnEdgeTo("batch-self", "batch-self", "self")},
				}})
				require.NoError(t, err)
				return "batch-self"
			},
		},
		{
			name: "ApplyArtifactImportedSession",
			write: func(t *testing.T, d *DB) string {
				t.Helper()

				gid := origin + "~import-self"
				result, err := d.ApplyArtifactImportedSession(
					t.Context(),
					ArtifactImportedSession{
						Origin: origin, GID: gid,
						ManifestHash:      strings.Repeat("b", 64),
						ImportedSessionID: gid,
					},
					SessionBatchWrite{
						Session:         selfParented(gid),
						Messages:        []Message{spawnEdgeTo(gid, gid, "self")},
						ReplaceMessages: true,
					},
				)
				require.NoError(t, err)
				require.True(t, result.Written)
				return gid
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			id := tc.write(t, d)
			s, err := d.GetSession(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, s)
			assert.Nil(t, s.ParentSessionID)
			assert.Equal(t, "subagent", s.RelationshipType)
		})
	}
}

// TestSanitizeMessageDropsDuplicateDialogue pins the dialogue storage
// contract: search falls back to Content when DialogueText is NULL, so a
// dialogue copy whose sanitized bytes equal sanitized Content is not stored
// unless ThinkingText marks the row as reasoning-classified.
func TestSanitizeMessageDropsDuplicateDialogue(t *testing.T) {
	for _, tc := range []struct {
		name, content, thinking string
		dialogue, want          *string
	}{
		{name: "duplicate after content sanitization dropped", content: "ans\x00wer", dialogue: Ptr("answer")},
		{name: "duplicate after dialogue sanitization dropped", content: "answer", dialogue: Ptr("ans\x00wer\xff")},
		{name: "distinct dialogue kept", content: "answer [Read: file.txt]", dialogue: Ptr("answer"), want: Ptr("answer")},
		{name: "empty dialogue for tool-only message kept", content: "[Read: file.txt]", dialogue: Ptr(""), want: Ptr("")},
		{name: "duplicate with thinking kept", content: "answer", thinking: "reasoning", dialogue: Ptr("answer"), want: Ptr("answer")},
		{name: "nil dialogue kept", content: "answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{
				SessionID: "s1", Ordinal: 0, Role: "assistant",
				Content: tc.content, ThinkingText: tc.thinking, DialogueText: tc.dialogue,
			}
			sanitized := m
			if tc.dialogue != nil {
				sanitized.DialogueText = Ptr(*tc.dialogue)
			}
			SanitizeMessage(&sanitized)
			assert.Equal(t, tc.want, sanitized.DialogueText, "sanitized")
		})
	}

	d := testDB(t)
	insertSession(t, d, "s1", "project")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s1", []Message{{
		SessionID: "s1", Role: "assistant", Content: "answer", DialogueText: Ptr("answer"),
	}}))
	stored, err := d.GetMessageByOrdinal(t.Context(), "s1", 0)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Nil(t, stored.DialogueText)
}
