package db

import (
	"database/sql"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestConversationVisibleProjectionAndRevisions(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "assistant", Content: "Display with reasoning", DialogueText: new("Visible α reply"), SourceUUID: "reply"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Old flattened reply", SourceUUID: "legacy"},
		{SessionID: "chat", Ordinal: 2, Role: "assistant", Content: "Tool rendering", DialogueText: new(""), SourceUUID: "tool"},
	}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
	stored, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	visible, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible"})
	require.NoError(t, err)
	require.Len(t, visible.Changes, 3)
	assert.Equal(t, "visible", visible.TextMode)
	assert.Equal(t, stored.Changes[0].MessageID, visible.Changes[0].MessageID)
	assert.Equal(t, "visible_text_unavailable", visible.Changes[1].Gap)
	assert.Equal(t, "no_visible_text", visible.Changes[2].Gap)
	for _, change := range visible.Changes[1:] {
		body, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{TextMode: "visible", DatabaseID: visible.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision})
		require.NoError(t, err)
		assert.Nil(t, body.Text)
		assert.Empty(t, body.Digest)
		assert.Zero(t, body.TextBytes)
	}
	change := visible.Changes[0]
	opts := ConversationMessageOptions{TextMode: "visible", DatabaseID: visible.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision}
	body, err := d.GetConversationMessage(t.Context(), opts)
	require.NoError(t, err)
	require.NotNil(t, body.Text)
	assert.Equal(t, "Visible α reply", *body.Text)
	opts.TextMode = "stored"
	body, err = d.GetConversationMessage(t.Context(), opts)
	require.NoError(t, err)
	require.NotNil(t, body.Text)
	assert.Equal(t, "Display with reasoning", *body.Text)
	for _, mode := range []string{"stored", "visible"} {
		checkpoint := visible.Checkpoint
		if mode == "visible" {
			checkpoint = stored.Checkpoint
		}
		_, err = d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: mode, Checkpoint: checkpoint})
		require.ErrorIs(t, err, ErrInvalidCursor)
	}
	partial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible", Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, partial.NextCursor)
	_, err = d.ExportConversationChanges(t.Context(), ConversationExportOptions{Cursor: partial.NextCursor})
	require.ErrorIs(t, err, ErrInvalidCursor)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
	unchanged, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible", Checkpoint: visible.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, unchanged.Changes, "an unchanged reparse does not republish visible text")
	msgs[0].DialogueText = new("Corrected prose")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
	delta, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible", Checkpoint: visible.Checkpoint})
	require.NoError(t, err)
	require.Len(t, delta.Changes, 1)
	assert.Equal(t, change.MessageID, delta.Changes[0].MessageID)
	assert.NotEqual(t, change.Revision, delta.Changes[0].Revision)
	opts.TextMode = "visible"
	_, err = d.GetConversationMessage(t.Context(), opts)
	require.ErrorIs(t, err, ErrConversationRevisionChanged)
	opts.Revision = delta.Changes[0].Revision
	body, err = d.GetConversationMessage(t.Context(), opts)
	require.NoError(t, err)
	require.NotNil(t, body.Text)
	assert.Equal(t, "Corrected prose", *body.Text)
	msgs[0].DialogueText = nil
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
	missing, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible", Checkpoint: delta.Checkpoint})
	require.NoError(t, err)
	require.Len(t, missing.Changes, 1)
	assert.Equal(t, change.MessageID, missing.Changes[0].MessageID)
	assert.Equal(t, "visible_text_unavailable", missing.Changes[0].Gap)
	opts.Revision = missing.Changes[0].Revision
	body, err = d.GetConversationMessage(t.Context(), opts)
	require.NoError(t, err)
	assert.Nil(t, body.Text)
	assert.Empty(t, body.Digest)
	assert.Zero(t, body.TextBytes)
}

func TestConversationVisibleUpgradeRetainsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "sample-agent"}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "assistant", Content: "Display and tools", DialogueText: new("Archived dialogue"), SourceUUID: "reply"}}))
	before, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.NoError(t, d.Close())
	// The shipped index has stored-text metadata only. Dialogue already exists
	// in the message archive; upgrading must not need its original source file.
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `DROP TRIGGER conversation_visible_revise;
ALTER TABLE conversation_messages DROP COLUMN visible_digest;
ALTER TABLE conversation_messages DROP COLUMN visible_text_bytes;
ALTER TABLE conversation_messages DROP COLUMN visible_gap;`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	reader, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	_, err = reader.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible"})
	var upgrade *SchemaUpgradeRequiredError
	require.ErrorAs(t, err, &upgrade)
	_, err = reader.GetConversationMessage(t.Context(), ConversationMessageOptions{TextMode: "visible", DatabaseID: before.DatabaseID, SessionID: "chat", MessageID: before.Changes[0].MessageID, Revision: before.Changes[0].Revision})
	require.ErrorAs(t, err, &upgrade)
	require.NoError(t, reader.Close())

	// A preservation rebuild can copy an older archive before that archive
	// has received a writable open with the newer binary.
	destination := testDB(t)
	require.NoError(t, destination.CopyArchiveIdentityFrom(path))
	_, err = destination.CopyOrphanedDataFrom(path)
	require.NoError(t, err)
	copied, err := destination.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible"})
	require.NoError(t, err)
	require.Len(t, copied.Changes, 1)
	assert.Equal(t, before.Changes[0].MessageID, copied.Changes[0].MessageID)
	copiedBody, err := destination.GetConversationMessage(t.Context(), ConversationMessageOptions{TextMode: "visible", DatabaseID: copied.DatabaseID, SessionID: "chat", MessageID: copied.Changes[0].MessageID, Revision: copied.Changes[0].Revision})
	require.NoError(t, err)
	require.NotNil(t, copiedBody.Text)
	assert.Equal(t, "Archived dialogue", *copiedBody.Text)

	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	visible, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible"})
	require.NoError(t, err)
	require.Len(t, visible.Changes, 1)
	change := visible.Changes[0]
	assert.Equal(t, before.Changes[0].MessageID, change.MessageID)
	body, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{TextMode: "visible", DatabaseID: visible.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision})
	require.NoError(t, err)
	require.NotNil(t, body.Text)
	assert.Equal(t, "Archived dialogue", *body.Text)
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	unchanged, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible", Checkpoint: visible.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, unchanged.Changes)
}

func TestConversationVisibleCopyAndPolicy(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			source := testDB(t)
			require.NoError(t, source.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "sample-agent"}))
			require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "assistant", Content: "Rendered transcript", DialogueText: new("Retained dialogue"), SourceUUID: "reply"}}))
			before, err := source.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible"})
			require.NoError(t, err)
			path := source.Path()
			require.NoError(t, source.Close())
			destination := testDB(t)
			destination.SetArchiveContent(policy)
			require.NoError(t, destination.CopyArchiveIdentityFrom(path))
			_, err = destination.CopyOrphanedDataFrom(path)
			require.NoError(t, err)
			after, err := destination.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible"})
			require.NoError(t, err)
			messages := 0
			for _, change := range after.Changes {
				if change.Type != "message" {
					continue
				}
				messages++
				assert.Equal(t, before.Changes[0].MessageID, change.MessageID)
				body, err := destination.GetConversationMessage(t.Context(), ConversationMessageOptions{TextMode: "visible", DatabaseID: after.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision})
				if policy == config.ArchiveContentUsage {
					require.ErrorIs(t, err, ErrArchiveContentExcluded)
					assert.Empty(t, change.Digest)
					assert.Zero(t, change.TextBytes)
				} else {
					require.NoError(t, err)
					require.NotNil(t, body.Text)
					assert.Equal(t, "Retained dialogue", *body.Text)
				}
			}
			assert.Equal(t, 1, messages, "the retained message must remain represented in the export")
		})
	}
}

func TestConversationVisibleCarriesSessionClassification(t *testing.T) {
	d := testDB(t)
	for _, tc := range []struct {
		id, relationship, want string
		automated              bool
	}{
		{id: "interactive", want: "interactive"},
		{id: "automated", automated: true, want: "automated"},
		{id: "worker", relationship: "subagent", automated: true, want: "subagent"},
		{id: "fork", relationship: "fork", want: "interactive"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			session := Session{ID: tc.id, Agent: "codex", Project: "sample", Machine: "local", IsAutomated: tc.automated, RelationshipType: tc.relationship}
			require.NoError(t, d.UpsertSession(t.Context(), session))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: tc.id, Role: "user", Content: "hello", DialogueText: new("hello"), SourceUUID: "prompt"}}))
			page, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible"})
			require.NoError(t, err)
			var selected ConversationChange
			for _, c := range page.Changes {
				if c.SessionID == tc.id && c.Type == "message" {
					selected = c
				}
			}
			require.NotEmpty(t, selected.MessageID)
			wire, err := json.Marshal(selected)
			require.NoError(t, err)
			var got struct {
				Session *struct {
					Agent      string `json:"agent"`
					Automation string `json:"automation"`
				} `json:"session"`
			}
			require.NoError(t, json.Unmarshal(wire, &got))
			require.NotNil(t, got.Session, "classification must not require a separate session-summary export")
			assert.Equal(t, "codex", got.Session.Agent)
			assert.Equal(t, tc.want, got.Session.Automation)
			body, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{TextMode: "visible", DatabaseID: page.DatabaseID, SessionID: tc.id, MessageID: selected.MessageID, Revision: selected.Revision})
			require.NoError(t, err)
			wire, err = json.Marshal(body)
			require.NoError(t, err)
			got.Session = nil
			require.NoError(t, json.Unmarshal(wire, &got))
			require.NotNil(t, got.Session)
			assert.Equal(t, "codex", got.Session.Agent)
			assert.Equal(t, tc.want, got.Session.Automation)
			// Classification edits must reach incremental readers even without text edits.
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET agent='claude', is_automated=1, relationship_type='' WHERE id=?", tc.id)
				return err
			}))
			delta, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{TextMode: "visible", Checkpoint: page.Checkpoint})
			require.NoError(t, err)
			require.Len(t, delta.Changes, 1)
			assert.Equal(t, "session", delta.Changes[0].Type)
			wire, err = json.Marshal(delta.Changes[0])
			require.NoError(t, err)
			got.Session = nil
			require.NoError(t, json.Unmarshal(wire, &got))
			require.NotNil(t, got.Session)
			assert.Equal(t, "claude", got.Session.Agent)
			assert.Equal(t, "automated", got.Session.Automation)
		})
	}
}
