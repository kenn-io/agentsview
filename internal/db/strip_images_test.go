package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/secrets"
)

func TestStripToolImagesScope(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "alpha", "alpha")
	insertSession(t, d, "beta", "beta")
	insertSession(t, d, "parent", "alpha", func(s *Session) {
		s.MessageCount = 1
	})
	child := "parent"
	insertSession(t, d, "child", "alpha", func(s *Session) {
		s.ParentSessionID = &child
	})
	sourcePath := t.TempDir() + "\\missing.jsonl"
	sourceMissingAt := "2026-01-01T00:00:00Z"
	insertSession(t, d, "source-missing", "alpha", func(s *Session) {
		s.FilePath = &sourcePath
	})
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET source_missing_at = ? WHERE id = ?",
		sourceMissingAt, "source-missing",
	)
	require.NoError(t, err)
	insertSession(t, d, "trashed", "alpha")
	require.NoError(t, d.SoftDeleteSession("trashed"))
	insertSession(t, d, "lookalike", "alpha")
	insertMessages(t, d,
		testImageMessage("alpha"),
		testImageMessage("beta"),
		testImageMessage("parent"),
		testImageMessage("child"),
		testImageMessage("source-missing"),
		testImageMessage("trashed"),
		Message{SessionID: "lookalike", Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{
			ResultContent: "ordinary input_image text",
			ResultEvents:  []ToolResultEvent{{Content: "ordinary input_image text"}},
		}}},
	)

	report, err := d.PreviewStripToolImages(context.Background(), StripImagesFilter{Project: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, 5, report.Sessions)
	assert.Equal(t, 5, report.Changed)
	assert.Len(t, report.Projects, 1)
	assert.Equal(t, "alpha", report.Projects[0].Project)

	report, err = d.StripToolImages(context.Background(), StripImagesFilter{Project: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, 5, report.Changed)

	alpha, err := d.GetAllMessages(context.Background(), "alpha")
	require.NoError(t, err)
	assert.NotContains(t, alpha[0].ToolCalls[0].ResultContent, "input_image")
	beta, err := d.GetAllMessages(context.Background(), "beta")
	require.NoError(t, err)
	assert.Contains(t, beta[0].ToolCalls[0].ResultContent, "input_image")
	for _, id := range []string{"source-missing", "trashed"} {
		messages, err := d.GetAllMessages(context.Background(), id)
		require.NoError(t, err)
		assert.NotContains(t, messages[0].ToolCalls[0].ResultContent, "input_image", id)
	}
	sourceSession, err := d.GetSessionFull(context.Background(), "source-missing")
	require.NoError(t, err)
	require.NotNil(t, sourceSession)
	assert.NotNil(t, sourceSession.SourceMissingAt)
	trashSession, err := d.GetSessionFull(context.Background(), "trashed")
	require.NoError(t, err)
	require.NotNil(t, trashSession)
	assert.NotNil(t, trashSession.DeletedAt)
}

func TestStripToolImagesBeforeUsesSessionTimestampFallbacks(t *testing.T) {
	d := testDB(t)
	d.SetToolResultImages(config.ToolResultImagesKeep)

	const boundary = "2026-02-01"
	tests := []struct {
		id       string
		ended    string
		started  string
		created  string
		selected bool
	}{
		{id: "old-ended", ended: "2026-01-15T00:00:00Z", started: "2026-02-15T00:00:00Z", created: "2026-02-15T00:00:00Z", selected: true},
		{id: "new-ended", ended: "2026-02-15T00:00:00Z", started: "2026-01-15T00:00:00Z", created: "2026-01-15T00:00:00Z"},
		{id: "old-started", ended: "", started: "2026-01-15T00:00:00Z", created: "2026-02-15T00:00:00Z", selected: true},
		{id: "new-started", ended: "", started: "2026-02-15T00:00:00Z", created: "2026-01-15T00:00:00Z"},
		{id: "old-created", ended: "", started: "", created: "2026-01-15T00:00:00Z", selected: true},
		{id: "new-created", ended: "", started: "", created: "2026-02-15T00:00:00Z"},
	}
	for _, tt := range tests {
		insertSession(t, d, tt.id, "project", func(s *Session) {
			s.EndedAt = new(tt.ended)
			s.StartedAt = new(tt.started)
		})
		_, err := d.getWriter().Exec(
			"UPDATE sessions SET created_at = ? WHERE id = ?",
			tt.created, tt.id,
		)
		require.NoError(t, err)
	}
	messages := make([]Message, 0, len(tests))
	for _, tt := range tests {
		messages = append(messages, testImageMessage(tt.id))
	}
	insertMessages(t, d, messages...)

	preview, err := d.PreviewStripToolImages(
		context.Background(), StripImagesFilter{Before: boundary},
	)
	require.NoError(t, err)
	assert.Equal(t, 3, preview.Sessions)
	assert.Equal(t, 3, preview.Changed)

	report, err := d.StripToolImages(
		context.Background(), StripImagesFilter{Before: boundary},
	)
	require.NoError(t, err)
	assert.Equal(t, 3, report.Sessions)
	assert.Equal(t, 3, report.Changed)

	for _, tt := range tests {
		got, err := d.GetAllMessages(context.Background(), tt.id)
		require.NoError(t, err)
		require.Len(t, got, 1)
		if tt.selected {
			assert.NotContains(t, got[0].ToolCalls[0].ResultContent, "input_image", tt.id)
		} else {
			assert.Contains(t, got[0].ToolCalls[0].ResultContent, "input_image", tt.id)
		}
	}
}

func TestStripToolImagesRejectsInvalidBefore(t *testing.T) {
	d := testDB(t)
	_, err := d.PreviewStripToolImages(
		context.Background(), StripImagesFilter{Before: "2026-02-30"},
	)
	require.EqualError(t, err,
		`invalid --before date "2026-02-30", expected YYYY-MM-DD`,
	)
}

func TestStripToolImagesUpdatesDeduplicatedCallLength(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "deduped", "project")
	insertMessages(t, d, testImageMessage("deduped"))

	var storedCall string
	var beforeLength int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT COALESCE(result_content, ''), result_content_length
		FROM tool_calls WHERE session_id = ?`, "deduped").Scan(
		&storedCall, &beforeLength,
	))
	assert.Empty(t, storedCall)
	assert.Positive(t, beforeLength)

	_, err := d.StripToolImages(context.Background(), StripImagesFilter{})
	require.NoError(t, err)

	var callContent, eventContent string
	var callLength, eventLength int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT COALESCE(tc.result_content, ''), tc.result_content_length,
		       ev.content, ev.content_length
		FROM tool_calls tc
		JOIN tool_result_events ev
		  ON ev.session_id = tc.session_id
		 AND ev.tool_call_message_ordinal = 0
		 AND ev.call_index = tc.call_index
		WHERE tc.session_id = ?`, "deduped").Scan(
		&callContent, &callLength, &eventContent, &eventLength,
	))
	assert.Empty(t, callContent)
	assert.Equal(t, len(eventContent), callLength)
	assert.Equal(t, len(eventContent), eventLength)
}

func TestStripToolImagesPublication(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "publish", "project")
	d.SetToolResultImages(config.ToolResultImagesKeep)
	insertMessages(t, d, testImageMessage("publish"))
	preview, err := d.PreviewStripToolImages(context.Background(), StripImagesFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), preview.Payloads)

	var before string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "publish",
	).Scan(&before))
	report, err := d.StripToolImages(context.Background(), StripImagesFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Changed)
	assert.Equal(t, preview.Payloads, report.Payloads)
	assert.Equal(t, preview.StoredBytes, report.StoredBytes)
	assert.Equal(t, preview.DecodedBytes, report.DecodedBytes)

	var after string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "publish",
	).Scan(&after))
	assert.NotEqual(t, before, after)

	report, err = d.StripToolImages(context.Background(), StripImagesFilter{})
	require.NoError(t, err)
	assert.Equal(t, 0, report.Changed)
	var repeat string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "publish",
	).Scan(&repeat))
	assert.Equal(t, after, repeat)
}

func TestStripToolImagesProjectsOrphanedStoredEvents(t *testing.T) {
	d := testDB(t)
	seedArtifactOrigin(t, d)
	insertSession(t, d, "event-only", "project")
	content := `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`
	_, err := d.getWriter().Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, agent_id, subagent_session_id,
			 source, status, content, content_length, timestamp, event_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"event-only", 7, 2, "toolu-event", "agent-1", "child-1",
		"subagent_notification", "completed", content, len(content),
		"2026-01-01T00:00:00Z", 4,
	)
	require.NoError(t, err)
	clearArtifactExportQueue(t, d)
	_, err = d.getWriter().Exec(`
		UPDATE sessions SET
			last_write_incremental = 1,
			is_automated = 1,
			quality_signal_version = 7,
			secret_leak_count = 2,
			secrets_rules_version = 'rules'
		WHERE id = ?`, "event-only")
	require.NoError(t, err)
	var beforeRevision string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "event-only",
	).Scan(&beforeRevision))

	preview, err := d.PreviewStripToolImages(
		context.Background(), StripImagesFilter{},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, preview.Sessions)
	assert.Equal(t, 1, preview.Changed)

	report, err := d.StripToolImages(
		context.Background(), StripImagesFilter{},
	)
	require.NoError(t, err)
	assert.Equal(t, preview.Changed, report.Changed)
	assert.Equal(t, preview.Payloads, report.Payloads)
	assert.Equal(t, preview.StoredBytes, report.StoredBytes)
	assert.Equal(t, preview.DecodedBytes, report.DecodedBytes)
	assert.Equal(t, 1, report.Changed)

	var stored, source, status, toolUseID, agentID, childID, timestamp string
	var contentLength, eventIndex int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT content, content_length, tool_use_id, agent_id,
		       subagent_session_id, source, status, timestamp, event_index
		FROM tool_result_events
		WHERE session_id = ?`, "event-only").Scan(
		&stored, &contentLength, &toolUseID, &agentID, &childID,
		&source, &status, &timestamp, &eventIndex,
	))
	assert.Contains(t, stored, `"text":"before"`)
	assert.Contains(t, stored, `"agentsview_image"`)
	assert.Contains(t, stored, `"text":"after"`)
	assert.Equal(t, len(stored), contentLength)
	assert.Equal(t, "toolu-event", toolUseID)
	assert.Equal(t, "agent-1", agentID)
	assert.Equal(t, "child-1", childID)
	assert.Equal(t, "subagent_notification", source)
	assert.Equal(t, "completed", status)
	assert.Equal(t, "2026-01-01T00:00:00Z", timestamp)
	assert.Equal(t, 4, eventIndex)

	var (
		afterRevision                                                      string
		lastWriteIncremental, isAutomated, qualityVersion, secretLeakCount int
	)
	require.NoError(t, d.getReader().QueryRow(`
		SELECT transcript_revision, last_write_incremental,
		       is_automated, quality_signal_version, secret_leak_count
		FROM sessions WHERE id = ?`, "event-only").Scan(
		&afterRevision, &lastWriteIncremental, &isAutomated,
		&qualityVersion, &secretLeakCount,
	))
	assert.NotEqual(t, beforeRevision, afterRevision)
	assert.Zero(t, lastWriteIncremental)
	assert.Zero(t, isAutomated)
	assert.Zero(t, qualityVersion)
	assert.Zero(t, secretLeakCount)
	var pending int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT pending FROM artifact_export_queue WHERE session_id = ?", "event-only",
	).Scan(&pending))
	assert.Equal(t, 1, pending)
}

func TestStripToolImagesRollsBackWhenEventUpdateFails(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "rollback", "project")
	message := testImageMessage("rollback")
	message.ToolCalls[0].ResultContent =
		`[{"type":"text","text":"summary"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	insertMessages(t, d, message)
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE TRIGGER fail_strip_event_update
			AFTER UPDATE OF content ON tool_result_events
			BEGIN
				SELECT RAISE(FAIL, 'forced strip event update failure');
			END`)
		return err
	}))

	_, err := d.StripToolImages(context.Background(), StripImagesFilter{})
	require.Error(t, err)
	assert.ErrorContains(t, err, "updating orphaned tool result event")

	var storedCall, storedEvent string
	require.NoError(t, d.getReader().QueryRow(`
		SELECT result_content
		FROM tool_calls
		WHERE session_id = ?`, "rollback").Scan(&storedCall))
	require.NoError(t, d.getReader().QueryRow(`
		SELECT content
		FROM tool_result_events
		WHERE session_id = ?`, "rollback").Scan(&storedEvent))
	assert.Contains(t, storedCall, "input_image")
	assert.Contains(t, storedEvent, "input_image")
}

func TestStripToolImagesCopiedSessionsOnly(t *testing.T) {
	ctx := t.Context()
	source := testDB(t)
	for _, id := range []string{"orphan", "trashed", "fresh", "excluded"} {
		insertSession(t, source, id, "project")
		insertMessages(t, source, testImageMessage(id))
	}
	require.NoError(t, source.SoftDeleteSession("trashed"))
	sourcePath := source.Path()
	require.NoError(t, source.Close())

	destination := testDB(t)
	trashedIDs, err := destination.CopyTrashedDataFrom(sourcePath)
	require.NoError(t, err)
	insertSession(t, destination, "fresh", "project")
	insertMessages(t, destination, testImageMessage("fresh"))
	orphanIDs, err := destination.CopyOrphanedDataFromExcluding(sourcePath, []string{"excluded"})
	require.NoError(t, err)
	assert.Equal(t, []string{"trashed"}, trashedIDs)
	assert.Equal(t, []string{"orphan"}, orphanIDs)
	require.NoError(t, destination.StripToolImagesForSessions(ctx, nil))
	require.NoError(t, destination.StripToolImagesForSessions(ctx, append(trashedIDs, orphanIDs...)))

	for _, id := range []string{"orphan", "trashed", "fresh"} {
		messages, err := destination.GetAllMessages(ctx, id)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		require.Len(t, messages[0].ToolCalls, 1)
		if id == "fresh" {
			assert.Contains(t, messages[0].ToolCalls[0].ResultContent, "input_image")
		} else {
			assert.Contains(t, messages[0].ToolCalls[0].ResultContent, "agentsview_image")
		}
	}
	excluded, err := destination.GetSessionFull(ctx, "excluded")
	require.NoError(t, err)
	assert.Nil(t, excluded)
}

func TestStripToolImagesRefreshesSecretFindings(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "secrets", "project")
	key := "AKIA" + "7QHWN2DKR4FYPLJA"
	message := testImageMessage("secrets")
	message.Content = "message key: " + key
	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"` + key + `"}]`
	message.ToolCalls[0].ResultContent = content
	message.ToolCalls[0].ResultEvents[0].Content = content
	insertMessages(t, d, message)
	// Seed the existing event finding at its pre-strip offset. The rescan must
	// relocate it after the larger placeholder and retain the message finding.
	require.NoError(t, d.ReplaceSessionSecretFindings("secrets", []SecretFinding{
		{RuleName: "aws-access-key", Confidence: "definite", LocationKind: "message", MatchStart: len("message key: "), MatchEnd: len(message.Content)},
		{RuleName: "aws-access-key", Confidence: "definite", LocationKind: "tool_result_event", CallIndex: new(0), EventIndex: new(0), MatchStart: strings.Index(content, key), MatchEnd: strings.Index(content, key) + len(key)},
	}, 2, secrets.RulesVersion()))
	report, err := d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Changed)
	findings, err := d.SessionSecretFindings(t.Context(), "secrets")
	require.NoError(t, err)
	require.Len(t, findings, 2)
	locations := []string{}
	for _, finding := range findings {
		locations = append(locations, finding.LocationKind)
		source, ok, err := d.SecretFindingSource(t.Context(), finding)
		require.NoError(t, err)
		require.True(t, ok)
		require.LessOrEqual(t, finding.MatchEnd, len(source))
		assert.Equal(t, key, source[finding.MatchStart:finding.MatchEnd])
		assert.Equal(t, secrets.RulesVersion(), finding.RulesVersion)
	}
	assert.ElementsMatch(t, []string{"message", "tool_result_event"}, locations)
	session, err := d.GetSessionFull(t.Context(), "secrets")
	require.NoError(t, err)
	assert.Equal(t, 2, session.SecretLeakCount)
	assert.Equal(t, secrets.RulesVersion(), session.SecretsRulesVersion)
	report, err = d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(t, err)
	assert.Zero(t, report.Changed)
	after, err := d.SessionSecretFindings(t.Context(), "secrets")
	require.NoError(t, err)
	assert.Equal(t, findings, after)
}

func TestStripToolImagesRescansStoredEventCoordinates(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		name := "attached"
		if orphan {
			name = "orphan"
		}
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "coordinates", "project")
			message := testImageMessage("coordinates")
			key := "AKIA" + "7QHWN2DKR4FYPLJA"
			content := message.ToolCalls[0].ResultEvents[0].Content
			content = strings.TrimSuffix(content, "]") + `,{"type":"text","text":"` + key + `"}]`
			message.ToolCalls[0].ResultContent = content
			message.ToolCalls[0].ResultEvents[0].Content = content
			insertMessages(t, d, message)
			// Copied archives keep stored event IDs, including rows with no call.
			_, err := d.getWriter().Exec("UPDATE tool_result_events SET event_index = 4 WHERE session_id = ?", "coordinates")
			require.NoError(t, err)
			if orphan {
				_, err = d.getWriter().Exec("DELETE FROM tool_calls WHERE session_id = ?", "coordinates")
				require.NoError(t, err)
			}
			report, err := d.StripToolImages(t.Context(), StripImagesFilter{})
			require.NoError(t, err)
			require.Equal(t, 1, report.Changed)
			findings, err := d.SessionSecretFindings(t.Context(), "coordinates")
			require.NoError(t, err)
			require.Len(t, findings, 1)
			finding := findings[0]
			require.NotNil(t, finding.EventIndex)
			assert.Equal(t, 4, *finding.EventIndex)
			assert.Equal(t, "tool_result_event", finding.LocationKind)
			var stored string
			require.NoError(t, d.getReader().QueryRow(
				"SELECT content FROM tool_result_events WHERE session_id = ? AND event_index = ?",
				finding.SessionID, *finding.EventIndex).Scan(&stored))
			assert.Equal(t, key, stored[finding.MatchStart:finding.MatchEnd])
			if !orphan {
				source, ok, err := d.SecretFindingSource(t.Context(), finding)
				require.NoError(t, err)
				require.True(t, ok)
				assert.Equal(t, stored, source)
			}
		})
	}
}
