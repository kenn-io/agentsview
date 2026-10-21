//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func TestRawProjectionRefreshesDialogueWithoutNewContent(t *testing.T) {
	f := newProjectionFixture(t)
	_, grantErr := f.admin.Exec(`REVOKE UPDATE ON messages FROM "` + f.role + `"; GRANT UPDATE(dialogue_text) ON messages TO "` + f.role + `"`)
	require.NoError(t, grantErr)
	m, _ := f.accept(t, "device-a", "dialogue-capture", "")
	outcome := projectionOutcome("question")
	const rendered = "[Thinking]\nreasonneedle\n[/Thinking]\nanswerneedle\n[Bash] $ commandneedle"
	parsed := &outcome.Outcome.Results[0].Result
	parsed.Messages[0].Content = "question\n[Bash] $ usercommandneedle"
	parsed.Messages[0].DialogueText = new("question")
	parsed.Messages[1].Content = rendered
	parsed.Messages[1].ThinkingText = "reasonneedle"
	parsed.Messages[1].HasThinking = true
	parsed.Messages = append(parsed.Messages, parser.ParsedMessage{Ordinal: 2, Role: parser.RoleTool, Content: "toolneedle"})
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	before, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.Equal(t, RawIdentityUnique, before.State)
	var initialContent string
	var initialDialogue sql.NullString
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT content,dialogue_text FROM messages WHERE session_id=$1 AND ordinal=0`, before.SessionID).Scan(&initialContent, &initialDialogue))
	require.Equal(t, "question\n[Bash] $ usercommandneedle", initialContent)
	require.Equal(t, sql.NullString{String: "question", Valid: true}, initialDialogue)
	require.NoError(t, f.sink.SetPin(t.Context(), "codex:portable", 1, true, "saved note"))
	var revision string
	var revisions, outbox int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT raw_content_revision FROM sessions WHERE id=$1`, before.SessionID).Scan(&revision))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM raw_content_revisions`).Scan(&revisions))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM raw_embedding_outbox`).Scan(&outbox))
	// Model an existing hosted database before the dialogue upgrade.
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET data_version=139; DELETE FROM raw_message_dialogue`)
	require.NoError(t, err)
	var originalPayload []byte
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT payload FROM raw_content_revisions WHERE session_id=$1`, before.SessionID).Scan(&originalPayload))
	store := &Store{pg: f.runtime}
	// Clear one projection while classifying the assistant and tool rows.
	parsed.Messages[0].DialogueText = nil
	parsed.Messages[1].DialogueText = new("answerneedle")
	parsed.Messages[2].DialogueText = new("")
	for _, stage := range []string{"old payload", "no-op", "current payload with stale physical rows"} {
		if stage == "current payload with stale physical rows" {
			_, err := f.runtime.ExecContext(t.Context(), `UPDATE messages SET dialogue_text=NULL WHERE session_id=$1`, before.SessionID)
			require.NoError(t, err)
		}
		processingVersion := "parser-2/" + stage
		_, err := f.sink.SelectSourceGeneration(t.Context(), m, processingVersion)
		require.NoError(t, err)
		leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "dialogue-worker", 1, time.Minute)
		require.NoError(t, err)
		require.Len(t, leases, 1)
		var derivedXmin, messageXmin, sessionXmin, dialogueXmin string
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT r.xmin::text,m.xmin::text,s.xmin::text,COALESCE((SELECT jsonb_object_agg(ordinal,xmin::text)::text FROM raw_message_dialogue WHERE session_id=r.session_id),'{}') FROM raw_content_revisions r JOIN messages m ON m.session_id=r.session_id JOIN sessions s ON s.id=r.session_id WHERE r.session_id=$1 AND m.ordinal=1`, before.SessionID).Scan(&derivedXmin, &messageXmin, &sessionXmin, &dialogueXmin))
		beforeRefresh, err := f.sink.Resolve(t.Context(), "codex:portable")
		require.NoError(t, err)
		require.NoError(t, f.sink.Project(t.Context(), leases[0], m, outcome))
		after, err := f.sink.Resolve(t.Context(), "codex:portable")
		require.NoError(t, err)
		if stage == "no-op" {
			assert.Equal(t, beforeRefresh.CorpusRevision, after.CorpusRevision)
			var afterDerivedXmin, afterMessageXmin, afterSessionXmin, afterDialogueXmin string
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT r.xmin::text,m.xmin::text,s.xmin::text,COALESCE((SELECT jsonb_object_agg(ordinal,xmin::text)::text FROM raw_message_dialogue WHERE session_id=r.session_id),'{}') FROM raw_content_revisions r JOIN messages m ON m.session_id=r.session_id JOIN sessions s ON s.id=r.session_id WHERE r.session_id=$1 AND m.ordinal=1`, after.SessionID).Scan(&afterDerivedXmin, &afterMessageXmin, &afterSessionXmin, &afterDialogueXmin))
			assert.Equal(t, derivedXmin, afterDerivedXmin, "unchanged reparse leaves derived state untouched")
			assert.JSONEq(t, dialogueXmin, afterDialogueXmin, "unchanged reparse leaves retained dialogue untouched")
			assert.Equal(t, messageXmin, afterMessageXmin, "unchanged reparse leaves message rows untouched")
			assert.Equal(t, sessionXmin, afterSessionXmin, "unchanged reparse leaves the session untouched")
		} else {
			assert.Equal(t, beforeRefresh.CorpusRevision+1, after.CorpusRevision)
		}
		assert.Equal(t, before.SessionID, after.SessionID)
		var retainedDialogue []byte
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT jsonb_object_agg(ordinal,dialogue_text) FROM raw_message_dialogue WHERE session_id=$1`, after.SessionID).Scan(&retainedDialogue))
		assert.JSONEq(t, `{"1":"answerneedle","2":""}`, string(retainedDialogue))
		var version int
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT data_version FROM sessions WHERE id=$1`, after.SessionID).Scan(&version))
		assert.Equal(t, db.CurrentDataVersion(), version)
		var savedPayload []byte
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT payload FROM raw_content_revisions WHERE session_id=$1`, after.SessionID).Scan(&savedPayload))
		assert.Equal(t, originalPayload, savedPayload, "reparse keeps immutable content unchanged")
		var gotRevision string
		var gotRevisions, gotOutbox int
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT raw_content_revision FROM sessions WHERE id=$1`, after.SessionID).Scan(&gotRevision))
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM raw_content_revisions`).Scan(&gotRevisions))
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM raw_embedding_outbox`).Scan(&gotOutbox))
		assert.Equal(t, revision, gotRevision)
		assert.Equal(t, revisions, gotRevisions)
		assert.Equal(t, outbox, gotOutbox)
		for ordinal, want := range []sql.NullString{{}, {String: "answerneedle", Valid: true}, {Valid: true}} {
			var got sql.NullString
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT dialogue_text FROM messages WHERE session_id=$1 AND ordinal=$2`, after.SessionID, ordinal).Scan(&got))
			assert.Equal(t, want, got, "stage %s, ordinal %d", stage, ordinal)
		}
		pins, err := store.ListPinnedMessages(t.Context(), after.SessionID, "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		require.NotNil(t, pins[0].Note)
		assert.Equal(t, "saved note", *pins[0].Note)
		tx, err := f.runtime.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer tx.Rollback()
		payload, err := loadRawPayload(t.Context(), tx, after.SessionID)
		require.NoError(t, err)
		assert.Nil(t, payload.Messages[0].DialogueText)
		require.NotNil(t, payload.Messages[1].DialogueText)
		assert.Equal(t, "answerneedle", *payload.Messages[1].DialogueText)
		require.NoError(t, tx.Commit())
	}
}

// A prefix can retain its own revision while its branch selects a longer one.
// Refreshing that prefix must retain dialogue until it materializes.
func TestRawProjectionDialogueUpgradeRefreshesRetainedRevision(t *testing.T) {
	f := newProjectionFixture(t)
	short := projectionOutcome("prefixneedle")
	short.Outcome.Results[0].Result.Messages = short.Outcome.Results[0].Result.Messages[:1]
	a, _ := f.accept(t, "device-a", "short", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
	prefix, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	b, accepted := f.accept(t, "device-b", "long", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("prefixneedle")))
	live, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NotEqual(t, prefix.SessionID, live.SessionID)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET data_version=139; DELETE FROM raw_message_dialogue`)
	require.NoError(t, err)
	short.Outcome.Results[0].Result.Messages[0].DialogueText = new("")
	_, err = f.sink.SelectSourceGeneration(t.Context(), a, "parser-2")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "upgrade-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], a, short))
	var exists bool
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1)`, prefix.SessionID).Scan(&exists))
	require.False(t, exists, "prefix must remain unmaterialized during the refresh")
	var retainedDialogue string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT dialogue_text FROM raw_message_dialogue WHERE session_id=$1 AND ordinal=0`, prefix.SessionID).Scan(&retainedDialogue))
	assert.Empty(t, retainedDialogue)
	// Removing the longer source materializes the refreshed retained prefix.
	b, _ = f.accept(t, "device-b", "removed", accepted.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}}))
	var version int
	var dialogue sql.NullString
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT s.data_version,m.dialogue_text FROM sessions s JOIN messages m ON m.session_id=s.id WHERE s.id=$1 AND m.ordinal=0`, prefix.SessionID).Scan(&version, &dialogue))
	assert.Equal(t, db.CurrentDataVersion(), version)
	assert.Equal(t, sql.NullString{Valid: true}, dialogue)
}

func TestRawProjectionDialogueVersionChecksAtOpen(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "original", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("question")))
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE sessions SET data_version=$1`, db.CurrentDataVersion()+1)
	require.NoError(t, err)
	_, err = NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant})
	var tooNew *db.DataVersionTooNewError
	require.ErrorAs(t, err, &tooNew)
	assert.Equal(t, db.CurrentDataVersion()+1, tooNew.DatabaseVersion)
	store, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	if store != nil {
		t.Cleanup(func() { assert.NoError(t, store.Close()) })
	}
	require.ErrorAs(t, err, &tooNew)
	assert.Equal(t, db.CurrentDataVersion()+1, tooNew.DatabaseVersion)
	assert.ErrorAs(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant), &tooNew)
}
