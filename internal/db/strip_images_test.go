package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/secrets"
)

func TestStripToolImagesScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	insertSession(t, d, "trashed", "alpha")
	require.NoError(d.SoftDeleteSession("trashed"))
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

	report, err := d.PreviewStripToolImages(t.Context(), StripImagesFilter{Project: "alpha"})
	require.NoError(err)
	assert.Equal(5, report.Sessions)
	assert.Equal(5, report.Changed)
	assert.Len(report.Projects, 1)
	assert.Equal("alpha", report.Projects[0].Project)

	report, err = d.StripToolImages(t.Context(), StripImagesFilter{Project: "alpha"})
	require.NoError(err)
	assert.Equal(5, report.Changed)

	alpha, err := d.GetAllMessages(t.Context(), "alpha")
	require.NoError(err)
	assert.NotContains(alpha[0].ToolCalls[0].ResultContent, "input_image")
	beta, err := d.GetAllMessages(t.Context(), "beta")
	require.NoError(err)
	assert.Contains(beta[0].ToolCalls[0].ResultContent, "input_image")
	for _, id := range []string{"source-missing", "trashed"} {
		messages, err := d.GetAllMessages(t.Context(), id)
		require.NoError(err)
		assert.NotContains(messages[0].ToolCalls[0].ResultContent, "input_image", id)
	}
	sourceSession, err := d.GetSessionFull(t.Context(), "source-missing")
	require.NoError(err)
	require.NotNil(sourceSession)
	assert.NotNil(sourceSession.SourceMissingAt)
	trashSession, err := d.GetSessionFull(t.Context(), "trashed")
	require.NoError(err)
	require.NotNil(trashSession)
	assert.NotNil(trashSession.DeletedAt)
}

func TestStripToolImagesBeforeUsesSessionTimestampFallbacks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
		require.NoError(err)
	}
	messages := make([]Message, 0, len(tests))
	for _, tt := range tests {
		messages = append(messages, testImageMessage(tt.id))
	}
	insertMessages(t, d, messages...)

	preview, err := d.PreviewStripToolImages(
		t.Context(), StripImagesFilter{Before: boundary},
	)
	require.NoError(err)
	assert.Equal(3, preview.Sessions)
	assert.Equal(3, preview.Changed)

	report, err := d.StripToolImages(
		t.Context(), StripImagesFilter{Before: boundary},
	)
	require.NoError(err)
	assert.Equal(3, report.Sessions)
	assert.Equal(3, report.Changed)

	for _, tt := range tests {
		got, err := d.GetAllMessages(t.Context(), tt.id)
		require.NoError(err)
		require.Len(got, 1)
		if tt.selected {
			assert.NotContains(got[0].ToolCalls[0].ResultContent, "input_image", tt.id)
		} else {
			assert.Contains(got[0].ToolCalls[0].ResultContent, "input_image", tt.id)
		}
	}
}

func TestStripToolImagesRejectsInvalidBefore(t *testing.T) {
	d := testDB(t)
	_, err := d.PreviewStripToolImages(
		t.Context(), StripImagesFilter{Before: "2026-02-30"},
	)
	require.EqualError(t, err,
		`invalid --before date "2026-02-30", expected YYYY-MM-DD`,
	)
}

func TestStripToolImagesUpdatesDeduplicatedCallLength(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "deduped", "project")
	insertMessages(t, d, testImageMessage("deduped"))

	var storedCall string
	var beforeLength int
	require.NoError(d.getReader().QueryRow(`
		SELECT COALESCE(result_content, ''), result_content_length
		FROM tool_calls WHERE session_id = ?`, "deduped").Scan(
		&storedCall, &beforeLength,
	))
	assert.Empty(storedCall)
	assert.Positive(beforeLength)

	_, err := d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(err)

	var callContent, eventContent string
	var callLength, eventLength int
	require.NoError(d.getReader().QueryRow(`
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
	assert.Empty(callContent)
	assert.Equal(len(eventContent), callLength)
	assert.Equal(len(eventContent), eventLength)
}

func TestStripToolImagesPublication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "publish", "project")
	d.SetToolResultImages(config.ToolResultImagesKeep)
	insertMessages(t, d, testImageMessage("publish"))
	preview, err := d.PreviewStripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(err)
	assert.Equal(int64(1), preview.Payloads)

	var before string
	require.NoError(d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "publish",
	).Scan(&before))
	report, err := d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(err)
	assert.Equal(1, report.Changed)
	assert.Equal(preview.Payloads, report.Payloads)
	assert.Equal(preview.StoredBytes, report.StoredBytes)
	assert.Equal(preview.DecodedBytes, report.DecodedBytes)

	var after string
	require.NoError(d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "publish",
	).Scan(&after))
	assert.NotEqual(before, after)

	report, err = d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(err)
	assert.Equal(0, report.Changed)
	var repeat string
	require.NoError(d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "publish",
	).Scan(&repeat))
	assert.Equal(after, repeat)
}

func TestStripToolImagesProjectsOrphanedStoredEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	clearArtifactExportQueue(t, d)
	_, err = d.getWriter().Exec(`
		UPDATE sessions SET
			last_write_incremental = 1,
			is_automated = 1,
			quality_signal_version = 7,
			secret_leak_count = 2,
			secrets_rules_version = 'rules'
		WHERE id = ?`, "event-only")
	require.NoError(err)
	var beforeRevision string
	require.NoError(d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "event-only",
	).Scan(&beforeRevision))

	preview, err := d.PreviewStripToolImages(
		t.Context(), StripImagesFilter{},
	)
	require.NoError(err)
	assert.Equal(1, preview.Sessions)
	assert.Equal(1, preview.Changed)

	report, err := d.StripToolImages(
		t.Context(), StripImagesFilter{},
	)
	require.NoError(err)
	assert.Equal(preview.Changed, report.Changed)
	assert.Equal(preview.Payloads, report.Payloads)
	assert.Equal(preview.StoredBytes, report.StoredBytes)
	assert.Equal(preview.DecodedBytes, report.DecodedBytes)
	assert.Equal(1, report.Changed)

	var stored, source, status, toolUseID, agentID, childID, timestamp string
	var contentLength, eventIndex int
	require.NoError(d.getReader().QueryRow(`
		SELECT content, content_length, tool_use_id, agent_id,
		       subagent_session_id, source, status, timestamp, event_index
		FROM tool_result_events
		WHERE session_id = ?`, "event-only").Scan(
		&stored, &contentLength, &toolUseID, &agentID, &childID,
		&source, &status, &timestamp, &eventIndex,
	))
	assert.Contains(stored, `"text":"before"`)
	assert.Contains(stored, `"agentsview_image"`)
	assert.Contains(stored, `"text":"after"`)
	assert.Equal(len(stored), contentLength)
	assert.Equal("toolu-event", toolUseID)
	assert.Equal("agent-1", agentID)
	assert.Equal("child-1", childID)
	assert.Equal("subagent_notification", source)
	assert.Equal("completed", status)
	assert.Equal("2026-01-01T00:00:00Z", timestamp)
	assert.Equal(4, eventIndex)

	var (
		afterRevision                                                      string
		lastWriteIncremental, isAutomated, qualityVersion, secretLeakCount int
	)
	require.NoError(d.getReader().QueryRow(`
		SELECT transcript_revision, last_write_incremental,
		       is_automated, quality_signal_version, secret_leak_count
		FROM sessions WHERE id = ?`, "event-only").Scan(
		&afterRevision, &lastWriteIncremental, &isAutomated,
		&qualityVersion, &secretLeakCount,
	))
	assert.NotEqual(beforeRevision, afterRevision)
	assert.Zero(lastWriteIncremental)
	assert.Zero(isAutomated)
	assert.Zero(qualityVersion)
	assert.Zero(secretLeakCount)
	var pending int
	require.NoError(d.getReader().QueryRow(
		"SELECT pending FROM artifact_export_queue WHERE session_id = ?", "event-only",
	).Scan(&pending))
	assert.Equal(1, pending)
}

func TestStripToolImagesRollsBackWhenEventUpdateFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "committed", "earlier-project")
	insertMessages(t, d, testImageMessage("committed"))
	insertSession(t, d, "rollback", "project")
	message := testImageMessage("rollback")
	message.ToolCalls[0].ResultContent = `[{"type":"text","text":"summary"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	insertMessages(t, d, message)
	require.NoError(d.Update(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			CREATE TRIGGER fail_strip_event_update
			AFTER UPDATE OF content ON tool_result_events
			WHEN OLD.session_id = 'rollback'
			BEGIN
				SELECT RAISE(FAIL, 'forced strip event update failure');
			END`)
		return err
	}))

	report, err := d.StripToolImages(t.Context(), StripImagesFilter{})
	require.Error(err)
	assert.ErrorContains(err, "updating orphaned tool result event")
	assert.Equal(StripImagesReport{
		Sessions: 1, Changed: 1, Payloads: 1, StoredBytes: 26, DecodedBytes: 3,
		Projects: []StripImagesProjectReport{{
			Project: "earlier-project", Sessions: 1, Changed: 1,
			Payloads: 1, StoredBytes: 26, DecodedBytes: 3,
		}},
	}, report)
	var committedContent string
	require.NoError(d.getReader().QueryRow(`
		SELECT content FROM tool_result_events WHERE session_id = 'committed'`,
	).Scan(&committedContent))
	assert.Contains(committedContent, "agentsview_image")
	assert.NotContains(committedContent, "input_image")

	var storedCall, storedEvent string
	require.NoError(d.getReader().QueryRow(`
		SELECT result_content
		FROM tool_calls
		WHERE session_id = ?`, "rollback").Scan(&storedCall))
	require.NoError(d.getReader().QueryRow(`
		SELECT content
		FROM tool_result_events
		WHERE session_id = ?`, "rollback").Scan(&storedEvent))
	assert.Contains(storedCall, "input_image")
	assert.Contains(storedEvent, "input_image")
}

func TestStripToolImagesCopiedSessionsOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	source := testDB(t)
	for _, id := range []string{"orphan", "trashed", "fresh", "excluded"} {
		insertSession(t, source, id, "project")
		insertMessages(t, source, testImageMessage(id))
	}
	require.NoError(source.SoftDeleteSession("trashed"))
	sourcePath := source.Path()
	require.NoError(source.Close())

	destination := testDB(t)
	trashedIDs, err := destination.CopyTrashedDataFrom(sourcePath)
	require.NoError(err)
	insertSession(t, destination, "fresh", "project")
	insertMessages(t, destination, testImageMessage("fresh"))
	orphanIDs, err := destination.CopyOrphanedDataFromExcluding(sourcePath, []string{"excluded"})
	require.NoError(err)
	assert.Equal([]string{"trashed"}, trashedIDs)
	assert.Equal([]string{"orphan"}, orphanIDs)
	require.NoError(destination.StripToolImagesForSessions(ctx, nil))
	require.NoError(destination.StripToolImagesForSessions(ctx, append(trashedIDs, orphanIDs...)))

	for _, id := range []string{"orphan", "trashed", "fresh"} {
		messages, err := destination.GetAllMessages(ctx, id)
		require.NoError(err)
		require.Len(messages, 1)
		require.Len(messages[0].ToolCalls, 1)
		if id == "fresh" {
			assert.Contains(messages[0].ToolCalls[0].ResultContent, "input_image")
		} else {
			assert.Contains(messages[0].ToolCalls[0].ResultContent, "agentsview_image")
		}
	}
	excluded, err := destination.GetSessionFull(ctx, "excluded")
	require.NoError(err)
	assert.Nil(excluded)
}

func TestStripToolImagesRefreshesSecretFindings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(d.ReplaceSessionSecretFindings("secrets", []SecretFinding{
		{RuleName: "aws-access-key", Confidence: "definite", LocationKind: "message", MatchStart: len("message key: "), MatchEnd: len(message.Content)},
		{RuleName: "aws-access-key", Confidence: "definite", LocationKind: "tool_result_event", CallIndex: new(0), EventIndex: new(0), MatchStart: strings.Index(content, key), MatchEnd: strings.Index(content, key) + len(key)},
	}, 2, secrets.RulesVersion()))
	report, err := d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(err)
	require.Equal(1, report.Changed)
	findings, err := d.SessionSecretFindings(t.Context(), "secrets")
	require.NoError(err)
	require.Len(findings, 2)
	locations := []string{}
	for _, finding := range findings {
		locations = append(locations, finding.LocationKind)
		source, ok, err := d.SecretFindingSource(t.Context(), finding)
		require.NoError(err)
		require.True(ok)
		require.LessOrEqual(finding.MatchEnd, len(source))
		assert.Equal(key, source[finding.MatchStart:finding.MatchEnd])
		assert.Equal(secrets.RulesVersion(), finding.RulesVersion)
	}
	assert.ElementsMatch([]string{"message", "tool_result_event"}, locations)
	session, err := d.GetSessionFull(t.Context(), "secrets")
	require.NoError(err)
	assert.Equal(2, session.SecretLeakCount)
	assert.Equal(secrets.RulesVersion(), session.SecretsRulesVersion)
	report, err = d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(err)
	assert.Zero(report.Changed)
	after, err := d.SessionSecretFindings(t.Context(), "secrets")
	require.NoError(err)
	assert.Equal(findings, after)
}

func TestStripToolImagesRescansStoredEventCoordinates(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		name := "attached"
		if orphan {
			name = "orphan"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

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
			require.NoError(err)
			if orphan {
				_, err = d.getWriter().Exec("DELETE FROM tool_calls WHERE session_id = ?", "coordinates")
				require.NoError(err)
			}
			report, err := d.StripToolImages(t.Context(), StripImagesFilter{})
			require.NoError(err)
			require.Equal(1, report.Changed)
			findings, err := d.SessionSecretFindings(t.Context(), "coordinates")
			require.NoError(err)
			require.Len(findings, 1)
			finding := findings[0]
			require.NotNil(finding.EventIndex)
			assert.Equal(4, *finding.EventIndex)
			assert.Equal("tool_result_event", finding.LocationKind)
			var stored string
			require.NoError(d.getReader().QueryRow(
				"SELECT content FROM tool_result_events WHERE session_id = ? AND event_index = ?",
				finding.SessionID, *finding.EventIndex).Scan(&stored))
			assert.Equal(key, stored[finding.MatchStart:finding.MatchEnd])
			if !orphan {
				source, ok, err := d.SecretFindingSource(t.Context(), finding)
				require.NoError(err)
				require.True(ok)
				assert.Equal(stored, source)
			}
		})
	}
}

// TestStripToolResultImagesGolden covers placeholders, unsupported headers,
// mixed blocks, and summaries with anonymous sections.
func TestStripToolResultImagesGolden(t *testing.T) {
	inputs := []string{
		// 1. Simple PNG inline.
		`[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`,
		// 2. WebP with charset parameter — parseInlineImageHeader requires exactly
		// two semicolon-delimited parts before the comma, so this passes through.
		`[{"type":"input_image","image_url":"data:image/webp;charset=utf-8;base64,AAEC"}]`,
		// 3. Already-stripped placeholder (no-op for strip).
		`[{"type":"agentsview_image","version":1,"text":"[Image: image/png, 3 bytes]","media_type":"image/png","byte_size":3,"sha256":""}]`,
		// 4. Mixed array with non-image blocks.
		`[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`,
		// 5. Labeled summary with anonymous trailing section.
		"agent-a:\n[{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,AAEC\"}]\n\n[{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,AAEC\"}]",
		// 6. Plain text with no images.
		`plain text, no images`,
	}
	want := []string{
		// 1. PNG stripped to placeholder.
		`[{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1}]`,
		// 2. WebP with charset passes through unchanged (3-part header rejected).
		`[{"type":"input_image","image_url":"data:image/webp;charset=utf-8;base64,AAEC"}]`,
		// 3. Already-stripped placeholder passes through unchanged.
		`[{"type":"agentsview_image","version":1,"text":"[Image: image/png, 3 bytes]","media_type":"image/png","byte_size":3,"sha256":""}]`,
		// 4. Mixed array: image stripped, text blocks pass through.
		`[{"type":"text","text":"before"},{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1},{"type":"text","text":"after"}]`,
		// 5. Both labeled sections stripped.
		"agent-a:\n[{\"byte_size\":3,\"media_type\":\"image/png\",\"sha256\":\"\",\"text\":\"[Image: image/png, 3 bytes]\",\"type\":\"agentsview_image\",\"version\":1}]\n\n[{\"byte_size\":3,\"media_type\":\"image/png\",\"sha256\":\"\",\"text\":\"[Image: image/png, 3 bytes]\",\"type\":\"agentsview_image\",\"version\":1}]",
		// 6. Plain text passes through unchanged.
		`plain text, no images`,
	}

	for i, input := range inputs {
		got, _ := StripToolResultImages(input)
		assert.Equal(t, want[i], got, "input %d", i+1)
	}
}

// TestStripPublicationSequence verifies that stripping changes stored content
// and its revision once, while a second strip is a no-op.
func TestStripPublicationSequence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	seedArtifactOrigin(t, d)
	insertSession(t, d, "pub-unchanged", "project")
	insertMessages(t, d, testImageMessage("pub-unchanged"))
	clearArtifactExportQueue(t, d)

	var before string
	require.NoError(d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "pub-unchanged",
	).Scan(&before))

	report, err := d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(err)
	assert.Equal(1, report.Changed)
	assert.Equal(int64(1), report.Payloads)

	var after string
	require.NoError(d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "pub-unchanged",
	).Scan(&after))
	assert.NotEqual(before, after)

	// testImageMessage seeds the mixed-array format: text+image+text.
	const wantStripped = `[{"type":"text","text":"before"},{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1},{"type":"text","text":"after"}]`
	var eventContent string
	require.NoError(d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "pub-unchanged",
	).Scan(&eventContent))
	assert.JSONEq(wantStripped, eventContent, "post-strip content preserves text around the placeholder")

	// Second strip: no change, revision stays.
	report2, err := d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(err)
	assert.Zero(report2.Changed)

	var after2 string
	require.NoError(d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "pub-unchanged",
	).Scan(&after2))
	assert.Equal(after, after2)
}
