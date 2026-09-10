package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
)

// fakePut returns a deterministic asset:// reference without writing any file.
func fakePut(mediaType string, _ []byte) (string, bool, error) {
	ext, ok := assets.ExtForMediaType(mediaType)
	if !ok {
		return "", false, fmt.Errorf("unsupported: %s", mediaType)
	}
	return "asset://fake" + ext, true, nil
}

// errorPut always returns an error.
func errorPut(_ string, _ []byte) (string, bool, error) {
	return "", false, errors.New("forced put failure")
}

// hexSHA256 computes the lowercase hex SHA-256 of b.
func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}

// realPut returns a put closure backed by the production asset writer.
func realPut(assetsDir string) imagePutFunc {
	return func(mediaType string, body []byte) (string, bool, error) {
		return assets.Put(assetsDir, mediaType, body)
	}
}

// TestToolImageMigrationContract verifies the core migration contract:
//   - A migratable block gains image_ref and a markdown text.
//   - A neighboring input_audio block stays byte-identical.
//   - A version:7 placeholder stays byte-identical.
//   - A failing put returns the original input plus the error.
//
// Boundary: 2 blocks preserved byte-identical.
func TestToolImageMigrationContract(t *testing.T) {
	audioBlock := `{"type":"input_audio","data":"dGVzdA==","format":"wav"}`
	placeholderV7 := `{"type":"agentsview_image","version":7,"text":"[Image]","media_type":"image/png","byte_size":3}`
	inlineBlock := `{"type":"input_image","image_url":"data:image/png;base64,AAEC"}`

	content := "[" + audioBlock + "," + inlineBlock + "," + placeholderV7 + "]"

	projected, err := migrateToolResultImages(content, fakePut)
	require.NoError(t, err)

	var blocks []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(projected), &blocks))
	require.Len(t, blocks, 3)

	// migrateToolResultImageArray re-emits skipped blocks through a bytes.Buffer
	// unchanged, so exact byte identity (not just JSON equality) is required.
	assert.Equal(t, audioBlock, string(blocks[0]))
	assert.Equal(t, placeholderV7, string(blocks[2]))
	t.Logf("2 blocks preserved byte-identical: preserved=2, migrated=1")

	// Migrated block has image_ref and markdown text.
	var migrated map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(blocks[1], &migrated))
	assert.Contains(t, string(migrated["image_ref"]), "asset://")
	assert.Contains(t, string(migrated["text"]), "![Image:")
	assert.Contains(t, string(migrated["text"]), "asset://")
	assert.Equal(t, `"agentsview_image"`, string(migrated["type"]))
	assert.Equal(t, `1`, string(migrated["version"]))

	// A failing put returns original content and the error.
	result, err2 := migrateToolResultImages(content, errorPut)
	require.Error(t, err2)
	assert.Equal(t, content, result)
}

// TestMigrateLeavesBlockInlineWhenPutFails verifies the fault guard:
// a put failure leaves the row byte-identical and rolls back.
// Boundary: 0 rows carrying image_ref after failure.
func TestMigrateLeavesBlockInlineWhenPutFails(t *testing.T) {
	d := testDB(t)
	seedArtifactOrigin(t, d)
	insertSession(t, d, "fail-put", "project")
	insertMessages(t, d, testImageMessage("fail-put"))

	var beforeContent string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "fail-put",
	).Scan(&beforeContent))
	assert.Contains(t, beforeContent, "input_image")

	_, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, errorPut)
	require.Error(t, err)

	var afterContent string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "fail-put",
	).Scan(&afterContent))
	// Boundary: 0 rows carrying image_ref after failure.
	assert.Equal(t, beforeContent, afterContent)
	assert.NotContains(t, afterContent, "image_ref")

}

// TestMigratePartialFailurePreservesReport verifies the partial-failure contract:
// when the second session's put fails, the returned report counts the first
// session's committed payloads, the error names the failed session, and the
// first session's rows are committed while the second's are unchanged.
func TestMigratePartialFailurePreservesReport(t *testing.T) {
	d := testDB(t)
	assetsDir := t.TempDir()

	insertSession(t, d, "pf-first", "project")
	insertSession(t, d, "pf-second", "project")
	insertMessages(t, d, testImageMessage("pf-first"))
	insertMessages(t, d, testImageMessage("pf-second"))

	// Snapshot second session content before migration.
	var secondBefore string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "pf-second",
	).Scan(&secondBefore))

	calls := 0
	countingPut := func(mediaType string, body []byte) (string, bool, error) {
		calls++
		if calls == 1 {
			// First call succeeds (first session).
			return realPut(assetsDir)(mediaType, body)
		}
		// Second call fails (second session).
		return "", false, errors.New("forced put failure for pf-second")
	}

	report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, countingPut)
	require.Error(t, err)

	// Error names the failed session.
	assert.Contains(t, err.Error(), "pf-second")

	// Report reflects only the committed first session.
	assert.Equal(t, 1, report.Sessions)
	assert.Equal(t, 1, report.Changed)
	assert.Equal(t, int64(1), report.Payloads)

	// First session's row is migrated.
	var firstAfter string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "pf-first",
	).Scan(&firstAfter))
	assert.Contains(t, firstAfter, "image_ref")
	assert.Contains(t, firstAfter, "asset://")

	// Second session's row is unchanged.
	var secondAfter string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "pf-second",
	).Scan(&secondAfter))
	assert.Equal(t, secondBefore, secondAfter)
	assert.NotContains(t, secondAfter, "image_ref")
}

// TestMigrateCancellationPreservesReport verifies that cancellation after a
// committed session still reports the work that preceded it.
func TestMigrateCancellationPreservesReport(t *testing.T) {
	d := testDB(t)
	assetsDir := t.TempDir()
	insertSession(t, d, "cancel-first", "project")
	insertSession(t, d, "cancel-second", "project")
	insertMessages(t, d, testImageMessage("cancel-first"))
	insertMessages(t, d, testImageMessage("cancel-second"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	applies := 0
	apply := func(ctx context.Context, session stripImageSession) (bool, error) {
		changed, err := d.migrateStoredToolResultRows(
			ctx, session.id, realPut(assetsDir),
		)
		applies++
		if applies == 1 {
			cancel()
		}
		return changed, err
	}

	report, err := d.scanToolImages(ctx, StripImagesFilter{}, countMigratable, apply)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, report.Sessions)
	assert.Equal(t, 1, report.Changed)
	assert.Equal(t, int64(1), report.Payloads)
	t.Logf("cancellation report: sessions=%d changed=%d payloads=%d", report.Sessions, report.Changed, report.Payloads)

	var firstContent string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "cancel-first",
	).Scan(&firstContent))
	assert.Contains(t, firstContent, "image_ref")

	var secondContent string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "cancel-second",
	).Scan(&secondContent))
	assert.Contains(t, secondContent, "input_image")
}

// TestMigrateStatsFailurePreservesReport verifies that a statistics error
// after a committed session still reports the work that preceded it.
func TestMigrateStatsFailurePreservesReport(t *testing.T) {
	d := testDB(t)
	assetsDir := t.TempDir()
	insertSession(t, d, "stats-first", "project")
	insertSession(t, d, "stats-second", "project")
	insertMessages(t, d, testImageMessage("stats-first"))
	insertMessages(t, d, testImageMessage("stats-second"))

	applies := 0
	apply := func(ctx context.Context, session stripImageSession) (bool, error) {
		changed, err := d.migrateStoredToolResultRows(
			ctx, session.id, realPut(assetsDir),
		)
		if err != nil {
			return false, err
		}
		applies++
		if applies == 1 {
			_, err = d.getWriter().Exec("DROP TABLE tool_result_events")
			require.NoError(t, err)
		}
		return changed, nil
	}

	report, err := d.scanToolImages(t.Context(), StripImagesFilter{}, countMigratable, apply)
	require.Error(t, err)
	assert.ErrorContains(t, err, "tool result bytes")
	assert.Equal(t, 1, report.Sessions)
	assert.Equal(t, 1, report.Changed)
	assert.Equal(t, int64(1), report.Payloads)
	entries, readErr := os.ReadDir(assetsDir)
	require.NoError(t, readErr)
	assert.Len(t, entries, 1)
	t.Logf("stats failure report: sessions=%d changed=%d payloads=%d", report.Sessions, report.Changed, report.Payloads)
}

// TestMigratedReferenceMatchesStoredFile verifies that migration repairs a
// same-size corrupt object before removing the inline source.
func TestMigratedReferenceMatchesStoredFile(t *testing.T) {
	decoded, err := base64.StdEncoding.DecodeString("AAEC")
	require.NoError(t, err)
	sum := sha256.Sum256(decoded)
	filename := fmt.Sprintf("%x.png", sum[:])
	for _, tt := range []struct {
		name        string
		seedCorrupt bool
	}{
		{name: "missing-object"},
		{name: "same-size-corrupt-object", seedCorrupt: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			assetsDir := t.TempDir()
			insertSession(t, d, "ref-match", "project")
			insertMessages(t, d, testImageMessage("ref-match"))

			if tt.seedCorrupt {
				corrupt := append([]byte(nil), decoded...)
				corrupt[0] ^= 0xff
				require.NoError(t, os.WriteFile(filepath.Join(assetsDir, filename), corrupt, 0o644))
				assert.Len(t, corrupt, len(decoded))
			}

			report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
			require.NoError(t, err)
			assert.Equal(t, 1, report.Changed)

			var storedContent string
			require.NoError(t, d.getReader().QueryRow(
				"SELECT content FROM tool_result_events WHERE session_id = ?", "ref-match",
			).Scan(&storedContent))
			assert.NotContains(t, storedContent, "input_image")
			assert.Contains(t, storedContent, `"text":"before"`)
			assert.Contains(t, storedContent, `"text":"after"`)

			var blocks []json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(storedContent), &blocks))

			var imageRef, sha256Hex string
			for _, raw := range blocks {
				var m map[string]json.RawMessage
				if json.Unmarshal(raw, &m) != nil {
					continue
				}
				if refRaw, ok := m["image_ref"]; ok {
					require.NoError(t, json.Unmarshal(refRaw, &imageRef))
					require.NoError(t, json.Unmarshal(m["sha256"], &sha256Hex))
					break
				}
			}
			require.Equal(t, "asset://"+filename, imageRef)

			stored, err := os.ReadFile(filepath.Join(assetsDir, filename))
			require.NoError(t, err)
			assert.Equal(t, decoded, stored)
			assert.Equal(t, sum, sha256.Sum256(stored))
			assert.Equal(t, hexSHA256(decoded), sha256Hex)

			messages, err := d.GetAllMessages(t.Context(), "ref-match")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 1)
			call := messages[0].ToolCalls[0]
			assert.Equal(t, storedContent, call.ResultContent)
			assert.NotContains(t, call.ResultContent, "input_image")

			var summaryLength, eventLength int
			require.NoError(t, d.getReader().QueryRow(`
				SELECT tc.result_content_length, ev.content_length
				FROM tool_calls tc
				JOIN messages m ON m.id = tc.message_id
				JOIN tool_result_events ev
				  ON ev.session_id = tc.session_id
				 AND ev.tool_call_message_ordinal = m.ordinal
				 AND ev.call_index = tc.call_index
				WHERE tc.session_id = ? AND tc.tool_use_id = ?`,
				"ref-match", "call-1",
			).Scan(&summaryLength, &eventLength))
			assert.Equal(t, len(storedContent), eventLength)
			assert.Equal(t, eventLength, summaryLength)
			assert.Equal(t, eventLength, call.ResultContentLength)
		})
	}
}

// TestMigrateWritesBeforeCommit verifies the write-before-commit ordering:
// after a forced transaction failure the asset file exists on disk but the
// archive row is unchanged, and a retry converges.
// Boundary: 1 file after 2 attempts.
func TestMigrateWritesBeforeCommit(t *testing.T) {
	d := testDB(t)
	seedArtifactOrigin(t, d)
	assetsDir := t.TempDir()

	insertSession(t, d, "write-before-commit", "project")
	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	_, err := d.getWriter().Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, agent_id, subagent_session_id,
			 source, status, content, content_length, timestamp, event_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"write-before-commit", 0, 0, "toolu-wbc", "", "",
		"tool", "completed", content, len(content),
		"2026-01-01T00:00:00Z", 0,
	)
	require.NoError(t, err)
	clearArtifactExportQueue(t, d)

	// Trigger that fires after bumpTranscriptRevisionTx updates transcript_revision,
	// aborting the transaction. At that point content UPDATEs have run but the
	// file has already been written to disk (projection runs before UPDATEs).
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE TRIGGER fail_after_revision_bump
			AFTER UPDATE OF transcript_revision ON sessions
			BEGIN
				SELECT RAISE(ABORT, 'forced failure after content update');
			END`)
		return err
	}))

	put := realPut(assetsDir)

	// First attempt: file written but transaction rolls back.
	_, err = d.MigrateToolImages(t.Context(), StripImagesFilter{}, put)
	require.Error(t, err)

	// Boundary: 1 file on disk despite the rollback.
	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	// Archive row still carries inline content.
	var storedAfterFail string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?",
		"write-before-commit",
	).Scan(&storedAfterFail))
	assert.Contains(t, storedAfterFail, "input_image")
	assert.NotContains(t, storedAfterFail, "image_ref")

	// Drop the trigger so the retry can succeed.
	_, err = d.getWriter().Exec(`DROP TRIGGER fail_after_revision_bump`)
	require.NoError(t, err)

	// Retry converges: same file (created=false), row now has image_ref.
	report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, put)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Changed)

	// Boundary: still 1 file after 2 attempts.
	entries2, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, entries2, 1)
	t.Logf("1 file after 2 attempts: %d on disk", len(entries2))
}

// TestMigrateSkipsStrippedAndUnservable verifies the negative space:
// a v1 placeholder with a provider sha256 and no image_ref is skipped,
// and an inline image/svg+xml payload stays inline.
// Boundary: 0 files written.
func TestMigrateSkipsStrippedAndUnservable(t *testing.T) {
	d := testDB(t)
	assetsDir := t.TempDir()
	insertSession(t, d, "skip-ineligible", "project")

	// Slice-1 placeholder with provider sha256 but no image_ref.
	placeholderContent := `[{"type":"agentsview_image","version":1,"text":"[Image: image/png, 3 bytes]","media_type":"image/png","byte_size":3,"sha256":"abc123"}]`
	// Inline SVG: accepted by decodeInlineImage but not in the four passive media types.
	svgContent := `[{"type":"input_image","image_url":"data:image/svg+xml;base64,PHN2Zy8+"}]`

	_, err := d.getWriter().Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, agent_id, subagent_session_id,
			 source, status, content, content_length, timestamp, event_index)
		VALUES
			(?, 0, 0, 'p1', '', '', 'tool', 'completed', ?, ?, '2026-01-01T00:00:00Z', 0),
			(?, 0, 1, 'p2', '', '', 'tool', 'completed', ?, ?, '2026-01-01T00:00:01Z', 1)`,
		"skip-ineligible", placeholderContent, len(placeholderContent),
		"skip-ineligible", svgContent, len(svgContent),
	)
	require.NoError(t, err)

	var beforeRevision string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "skip-ineligible",
	).Scan(&beforeRevision))

	put := realPut(assetsDir)
	report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, put)
	require.NoError(t, err)

	// Boundary: 0 files written.
	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
	t.Logf("0 files written: %d in assets dir", len(entries))
	assert.Zero(t, report.Sessions)
	assert.Zero(t, report.Changed)

	// SVG payload stays inline.
	var svgAfter string
	require.NoError(t, d.getReader().QueryRow(`
		SELECT content FROM tool_result_events
		WHERE session_id = ? AND event_index = 1`, "skip-ineligible",
	).Scan(&svgAfter))
	assert.Equal(t, svgContent, svgAfter)

	// Revision unchanged.
	var afterRevision string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "skip-ineligible",
	).Scan(&afterRevision))
	assert.Equal(t, beforeRevision, afterRevision)
}

// TestMigrateRejectsUnsupportedImageMediaTypes verifies that acceptance is the
// asset store's media-type map rather than the image/ prefix parseInlineImageHeader
// checks: image/bmp and the non-canonical image/jpg spelling providers emit both
// stay inline. Boundary: 0 files written per media type.
func TestMigrateRejectsUnsupportedImageMediaTypes(t *testing.T) {
	for _, mediaType := range []string{"image/bmp", "image/jpg"} {
		t.Run(mediaType, func(t *testing.T) {
			d := testDB(t)
			assetsDir := t.TempDir()
			insertSession(t, d, "reject-media", "project")

			content := `[{"type":"input_image","image_url":"data:` +
				mediaType + `;base64,AAEC"}]`
			_, err := d.getWriter().Exec(`
				INSERT INTO tool_result_events
					(session_id, tool_call_message_ordinal, call_index,
					 tool_use_id, agent_id, subagent_session_id,
					 source, status, content, content_length, timestamp, event_index)
				VALUES (?, 0, 0, 'reject', '', '', 'tool', 'completed', ?, ?,
					'2026-01-01T00:00:00Z', 0)`,
				"reject-media", content, len(content),
			)
			require.NoError(t, err)

			report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
			require.NoError(t, err)
			assert.Zero(t, report.Sessions)
			assert.Zero(t, report.Payloads)

			var after string
			require.NoError(t, d.getReader().QueryRow(
				"SELECT content FROM tool_result_events WHERE session_id = ?", "reject-media",
			).Scan(&after))
			assert.Equal(t, content, after)

			entries, err := os.ReadDir(assetsDir)
			require.NoError(t, err)
			assert.Empty(t, entries)
			t.Logf("%s: 0 files written, block unchanged: %d in assets dir", mediaType, len(entries))

			// db strip --images still removes these payloads: strip accepts any
			// image/* data URI, so the two commands disagree by design.
			stripped, stats := StripToolResultImages(content)
			assert.Equal(t, int64(1), stats.Payloads)
			assert.Contains(t, stripped, `"type":"agentsview_image"`)
		})
	}
}

// TestMigratePreviewAndApplyAgreeOnUnsupportedType verifies that preview and
// apply share one migratability predicate. A session mixing a migratable PNG
// with an unsupported media type must report the same payload count either way,
// and put must never see the unsupported block: assets.Put would reject it and
// the projection error would abort the whole session transaction.
func TestMigratePreviewAndApplyAgreeOnUnsupportedType(t *testing.T) {
	d := testDB(t)
	assetsDir := t.TempDir()
	insertSession(t, d, "mixed-media", "project")

	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"},` +
		`{"type":"input_image","image_url":"data:image/bmp;base64,AAEC"}]`
	_, err := d.getWriter().Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, agent_id, subagent_session_id,
			 source, status, content, content_length, timestamp, event_index)
		VALUES (?, 0, 0, 'mixed', '', '', 'tool', 'completed', ?, ?,
			'2026-01-01T00:00:00Z', 0)`,
		"mixed-media", content, len(content),
	)
	require.NoError(t, err)

	preview, err := d.PreviewMigrateToolImages(t.Context(), StripImagesFilter{})
	require.NoError(t, err)

	write := realPut(assetsDir)
	var putTypes []string
	put := func(mediaType string, body []byte) (string, bool, error) {
		putTypes = append(putTypes, mediaType)
		return write(mediaType, body)
	}
	applied, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, put)
	require.NoError(t, err)

	assert.Equal(t, preview.Payloads, applied.Payloads)
	assert.Equal(t, int64(1), applied.Payloads)
	assert.Equal(t, preview.Sessions, applied.Sessions)
	assert.Equal(t, []string{"image/png"}, putTypes)

	var after string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "mixed-media",
	).Scan(&after))
	assert.Contains(t, after, "image_ref")
	assert.Contains(t, after, "data:image/bmp;base64,AAEC")
	t.Logf("preview payloads=%d, applied payloads=%d, put calls=%d",
		preview.Payloads, applied.Payloads, len(putTypes))
}

// TestMigrateSummaryPutFailurePreservesContent verifies that a failure in a
// later summary section returns the original content.
func TestMigrateSummaryPutFailurePreservesContent(t *testing.T) {
	content := "agent-a:\n" + testInlineImageContent() +
		"\n\nagent-b:\n" + testInlineImageContent()

	calls := 0
	put := func(mediaType string, body []byte) (string, bool, error) {
		calls++
		if calls == 1 {
			return fakePut(mediaType, body)
		}
		return "", false, errors.New("forced put failure in the second section")
	}

	got, err := migrateToolResultImages(content, put)
	require.Error(t, err)
	assert.Equal(t, content, got)
	assert.Equal(t, 2, calls, "the first section must migrate before the failure")
}

// TestMigrateNoOpPreservesRevisionAndQueue verifies that a session with no
// migratable payloads produces no publications, no revision bump, no queue row.
// Boundary: 0 publications.
func TestMigrateNoOpPreservesRevisionAndQueue(t *testing.T) {
	d := testDB(t)
	seedArtifactOrigin(t, d)
	assetsDir := t.TempDir()
	insertSession(t, d, "noop-session", "project")
	insertMessages(t, d, Message{
		SessionID: "noop-session",
		Ordinal:   0,
		Role:      "assistant",
		Content:   "plain text only",
		ToolCalls: []ToolCall{{
			ToolUseID:     "call-plain",
			ResultContent: "plain result",
			ResultEvents:  []ToolResultEvent{{Content: "plain result"}},
		}},
	})

	var beforeRevision string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "noop-session",
	).Scan(&beforeRevision))

	// Seed observable tail-step state so a spurious execution of any step is
	// detectable. rewriteStoredToolResultRows touches quality_signal_version
	// (invalidateSessionSignalsTx) and last_write_incremental
	// (resetIncrementalMarkerTx) on every change; we verify both are unchanged.
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET quality_signal_version = 7, last_write_incremental = 1 WHERE id = ?",
		"noop-session",
	)
	require.NoError(t, err)
	// Clear after seeding: the sessions UPDATE may trigger an artifact-export enqueue.
	clearArtifactExportQueue(t, d)

	put := realPut(assetsDir)
	report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, put)
	require.NoError(t, err)

	// Boundary: 0 publications.
	assert.Zero(t, report.Sessions)
	assert.Zero(t, report.Changed)

	var afterRevision string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT transcript_revision FROM sessions WHERE id = ?", "noop-session",
	).Scan(&afterRevision))
	assert.Equal(t, beforeRevision, afterRevision)

	// No new publication queued: pending stays 0 (clearArtifactExportQueue zeros
	// rows but does not delete them).
	var pending int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT pending FROM artifact_export_queue WHERE session_id = ?", "noop-session",
	).Scan(&pending))
	assert.Zero(t, pending)

	// Signal invalidation and incremental-marker reset must not fire.
	var qualitySignalVersion, lastWriteIncremental int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT quality_signal_version, last_write_incremental FROM sessions WHERE id = ?",
		"noop-session",
	).Scan(&qualitySignalVersion, &lastWriteIncremental))
	assert.Equal(t, 7, qualitySignalVersion, "quality_signal_version must not be zeroed by invalidateSessionSignalsTx")
	assert.Equal(t, 1, lastWriteIncremental, "last_write_incremental must not be zeroed by resetIncrementalMarkerTx")

	// Nothing written to the assets directory.
	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
	t.Logf("0 publications: sessions=%d, changed=%d, assets=%d", report.Sessions, report.Changed, len(entries))
}

// TestMigrateDeduplicatesAcrossSessions verifies that the same inline image
// in two sessions produces one file and two identical asset:// references.
// Boundary: 1 file for 2 references.
func TestMigrateDeduplicatesAcrossSessions(t *testing.T) {
	d := testDB(t)
	assetsDir := t.TempDir()
	for _, id := range []string{"dedup-a", "dedup-b"} {
		insertSession(t, d, id, "project")
		insertMessages(t, d, testImageMessage(id))
	}

	put := realPut(assetsDir)
	report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, put)
	require.NoError(t, err)
	assert.Equal(t, 2, report.Changed)

	// Boundary: 1 file for 2 references.
	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	t.Logf("1 file for 2 references: files=%d", len(entries))

	refs := make([]string, 0, 2)
	for _, id := range []string{"dedup-a", "dedup-b"} {
		var content string
		require.NoError(t, d.getReader().QueryRow(
			"SELECT content FROM tool_result_events WHERE session_id = ?", id,
		).Scan(&content))
		var blocks []json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(content), &blocks))
		for _, raw := range blocks {
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			if refRaw, ok := m["image_ref"]; ok {
				var ref string
				require.NoError(t, json.Unmarshal(refRaw, &ref))
				refs = append(refs, ref)
			}
		}
	}
	require.Len(t, refs, 2)
	assert.Equal(t, refs[0], refs[1])
}

// TestMigrateStateMatrix covers keep, drop, tombstoned-session, and orphan
// rows over stored content.
func TestMigrateStateMatrix(t *testing.T) {
	// keep: inline payload migrated.
	t.Run("keep", func(t *testing.T) {
		d := testDB(t)
		assetsDir := t.TempDir()
		insertSession(t, d, "keep-session", "project")
		insertMessages(t, d, testImageMessage("keep-session"))
		report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
		require.NoError(t, err)
		assert.Equal(t, 1, report.Changed)
	})

	// drop: slice-1 placeholder carries no inline bytes; nothing to migrate.
	t.Run("drop", func(t *testing.T) {
		d := testDB(t)
		d.SetToolResultImages(config.ToolResultImagesDrop)
		insertSession(t, d, "drop-session", "project")
		insertMessages(t, d, testImageMessage("drop-session"))
		report, err := d.PreviewMigrateToolImages(t.Context(), StripImagesFilter{})
		require.NoError(t, err)
		assert.Zero(t, report.Sessions)
	})

	// tombstoned: session deleted from table; not selected.
	t.Run("tombstoned", func(t *testing.T) {
		d := testDB(t)
		assetsDir := t.TempDir()
		insertSession(t, d, "tombstoned", "project")
		insertMessages(t, d, testImageMessage("tombstoned"))
		_, err := d.getWriter().Exec("DELETE FROM sessions WHERE id = ?", "tombstoned")
		require.NoError(t, err)
		report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
		require.NoError(t, err)
		assert.Zero(t, report.Sessions)
		entries, err := os.ReadDir(assetsDir)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	// trashed and source-missing sessions are reachable.
	t.Run("trashed-and-source-missing", func(t *testing.T) {
		d := testDB(t)
		assetsDir := t.TempDir()
		insertSession(t, d, "trashed-mig", "project")
		insertSession(t, d, "srcmissing-mig", "project")
		insertMessages(t, d, testImageMessage("trashed-mig"))
		insertMessages(t, d, testImageMessage("srcmissing-mig"))
		require.NoError(t, d.SoftDeleteSession("trashed-mig"))
		sourcePath := filepath.Join(t.TempDir(), "missing.jsonl")
		insertSession(t, d, "srcmissing-mig", "project", func(s *Session) {
			s.FilePath = &sourcePath
		})
		_, err := d.getWriter().Exec(
			"UPDATE sessions SET source_missing_at = ? WHERE id = ?",
			"2026-01-01T00:00:00Z", "srcmissing-mig",
		)
		require.NoError(t, err)
		report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
		require.NoError(t, err)
		assert.Equal(t, 2, report.Changed)
	})

	// labeled-summary: content in multiline labeled-summary format (multi-agent)
	// exercises migrateToolResultSummaryImages. Only the inline image block
	// within the labeled section is migrated; the plain-text section is unchanged.
	t.Run("labeled-summary", func(t *testing.T) {
		d := testDB(t)
		assetsDir := t.TempDir()
		insertSession(t, d, "labeled-sum", "project")

		// Multiline labeled-summary: agent-a has an inline image, agent-b is plain.
		labeledContent := "agent-a:\n" + testInlineImageContent() + "\n\nagent-b:\n[{\"type\":\"text\",\"text\":\"plain\"}]"
		_, err := d.getWriter().Exec(`
			INSERT INTO tool_result_events
				(session_id, tool_call_message_ordinal, call_index,
				 tool_use_id, agent_id, subagent_session_id,
				 source, status, content, content_length, timestamp, event_index)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"labeled-sum", 0, 0, "toolu-ls", "", "",
			"tool", "completed", labeledContent, len(labeledContent),
			"2026-01-01T00:00:00Z", 0,
		)
		require.NoError(t, err)

		report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
		require.NoError(t, err)
		assert.Equal(t, 1, report.Changed)

		var stored string
		require.NoError(t, d.getReader().QueryRow(
			"SELECT content FROM tool_result_events WHERE session_id = ?", "labeled-sum",
		).Scan(&stored))
		assert.Contains(t, stored, "image_ref")
		assert.Contains(t, stored, "asset://")
		assert.Contains(t, stored, "agent-b:")
		assert.Contains(t, stored, `"text":"plain"`)
		assert.NotContains(t, stored, "input_image")
	})

	// keep-to-drop-historical: session with a mix of historical inline content
	// (stored under keep policy) and an already-stripped placeholder (stored
	// under drop policy). Migration migrates only the inline block.
	t.Run("keep-to-drop-historical", func(t *testing.T) {
		d := testDB(t)
		assetsDir := t.TempDir()
		insertSession(t, d, "ktd-session", "project")

		inlineContent := testInlineImageContent()
		// Slice-1 placeholder already stored (simulates a later result stripped at ingest).
		strippedContent := `[{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1}]`

		_, err := d.getWriter().Exec(`
			INSERT INTO tool_result_events
				(session_id, tool_call_message_ordinal, call_index,
				 tool_use_id, agent_id, subagent_session_id,
				 source, status, content, content_length, timestamp, event_index)
			VALUES
				(?, 0, 0, 'ktd-inline', '', '', 'tool', 'completed', ?, ?, '2026-01-01T00:00:00Z', 0),
				(?, 0, 1, 'ktd-stripped', '', '', 'tool', 'completed', ?, ?, '2026-01-01T00:00:01Z', 1)`,
			"ktd-session", inlineContent, len(inlineContent),
			"ktd-session", strippedContent, len(strippedContent),
		)
		require.NoError(t, err)

		report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
		require.NoError(t, err)
		assert.Equal(t, 1, report.Changed)
		assert.Equal(t, int64(1), report.Payloads, "only the historical inline event should be migrated")

		// Inline event is migrated.
		var inlineAfter string
		require.NoError(t, d.getReader().QueryRow(`
			SELECT content FROM tool_result_events
			WHERE session_id = ? AND event_index = 0`, "ktd-session",
		).Scan(&inlineAfter))
		assert.Contains(t, inlineAfter, "image_ref")

		// Already-stripped event is unchanged.
		var strippedAfter string
		require.NoError(t, d.getReader().QueryRow(`
			SELECT content FROM tool_result_events
			WHERE session_id = ? AND event_index = 1`, "ktd-session",
		).Scan(&strippedAfter))
		assert.Equal(t, strippedContent, strippedAfter)
	})

	// late-result: a tool result event that arrived via an incremental write
	// (event_index > 0) carries an inline image. Migration reaches it.
	t.Run("late-result", func(t *testing.T) {
		d := testDB(t)
		assetsDir := t.TempDir()
		insertSession(t, d, "late-mig", "project")

		// Insert a late-arriving event directly (simulates WriteSessionIncremental).
		lateContent := `[{"type":"input_image","image_url":"data:image/gif;base64,AAEC"}]`
		_, err := d.getWriter().Exec(`
			INSERT INTO tool_result_events
				(session_id, tool_call_message_ordinal, call_index,
				 tool_use_id, agent_id, subagent_session_id,
				 source, status, content, content_length, timestamp, event_index)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"late-mig", 1, 0, "toolu-late", "agent-x", "",
			"function_call_output", "completed", lateContent, len(lateContent),
			"2026-01-01T00:00:01Z", 1,
		)
		require.NoError(t, err)

		report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
		require.NoError(t, err)
		assert.Equal(t, 1, report.Changed)

		var stored string
		require.NoError(t, d.getReader().QueryRow(
			"SELECT content FROM tool_result_events WHERE session_id = ?", "late-mig",
		).Scan(&stored))
		assert.Contains(t, stored, "image_ref")
		assert.Contains(t, stored, "asset://")
		assert.NotContains(t, stored, "input_image")
	})

	// blocked-result-neighbor: a withheld (blocked) tool result beside a migratable
	// inline image. Migration must not alter the blocked result's accounting length.
	// Boundary: 913, the neighboring withheld empty result's retained accounting length.
	t.Run("blocked-result-neighbor", func(t *testing.T) {
		d := testDB(t)
		assetsDir := t.TempDir()
		insertSession(t, d, "blocked-nbr", "project")

		const blockedLen = 913
		blockedRaw := strings.Repeat("x", blockedLen)

		insertMessages(t, d, Message{
			SessionID: "blocked-nbr",
			Ordinal:   0,
			Role:      "assistant",
			ToolCalls: []ToolCall{
				{
					ToolName:      "Read",
					Category:      "Read",
					ToolUseID:     "call-img",
					ResultContent: testInlineImageContent(),
					ResultEvents: []ToolResultEvent{{
						ToolUseID: "call-img",
						Source:    "tool",
						Status:    "completed",
						Content:   testInlineImageContent(),
					}},
				},
				{
					ToolName:            "exec_command",
					Category:            "Bash",
					ToolUseID:           "call-blocked",
					ResultContentLength: blockedLen,
					ResultEvents: []ToolResultEvent{{
						ToolUseID:     "call-blocked",
						Source:        "function_call_output",
						Status:        "completed",
						Content:       blockedRaw,
						ContentLength: blockedLen,
					}},
				},
			},
		})

		report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
		require.NoError(t, err)
		assert.Equal(t, 1, report.Changed)

		var storedLen int
		require.NoError(t, d.getReader().QueryRow(`
			SELECT COALESCE(result_content_length, 0)
			FROM tool_calls WHERE session_id = ? AND tool_use_id = ?`,
			"blocked-nbr", "call-blocked",
		).Scan(&storedLen))
		assert.Equal(t, blockedLen, storedLen)
		t.Logf("neighboring withheld empty result retained accounting length: %d — boundary 913", storedLen)
	})
}

// TestMigrateDeduplicatedSummaryKeepsCollapse verifies that the
// deduplicated-summary collapse in rewriteStoredToolResultRows is preserved:
// a collapsed call (empty result_content, non-zero result_content_length from
// the event) migrates the event's content and keeps the length in sync.
// testImageMessage already produces a dedup scenario: result_content is NULL in
// tool_calls (the db deduplicates it against the event content on insert).
func TestMigrateDeduplicatedSummaryKeepsCollapse(t *testing.T) {
	d := testDB(t)
	assetsDir := t.TempDir()
	put := realPut(assetsDir)
	seedArtifactOrigin(t, d)
	insertSession(t, d, "dedup-collapse", "project")
	insertMessages(t, d, testImageMessage("dedup-collapse"))
	clearArtifactExportQueue(t, d)

	// Verify the dedup invariant: result_content is empty, result_content_length
	// is positive (set to the event length at insert time).
	var beforeCallContent string
	var beforeLength int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT COALESCE(result_content, ''), result_content_length
		FROM tool_calls WHERE session_id = ?`, "dedup-collapse",
	).Scan(&beforeCallContent, &beforeLength))
	assert.Empty(t, beforeCallContent)
	assert.Positive(t, beforeLength)

	report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, put)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Changed)

	var callContent string
	var callLength int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT COALESCE(result_content, ''), result_content_length
		FROM tool_calls WHERE session_id = ?`, "dedup-collapse",
	).Scan(&callContent, &callLength))

	var eventContent string
	var eventLength int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT content, content_length
		FROM tool_result_events WHERE session_id = ?`, "dedup-collapse",
	).Scan(&eventContent, &eventLength))

	// Call stays collapsed (result_content still empty) but length follows the
	// migrated event content.
	assert.Empty(t, callContent)
	assert.Equal(t, eventLength, callLength)
	assert.Equal(t, len(eventContent), eventLength)
	// Event carries image_ref and asset:// reference.
	assert.Contains(t, eventContent, "image_ref")
	assert.Contains(t, eventContent, "asset://")
}

// TestMigrateReachesOrphanEventRows verifies that orphaned tool_result_events
// rows (with no matching tool_call) are migrated.
func TestMigrateReachesOrphanEventRows(t *testing.T) {
	d := testDB(t)
	seedArtifactOrigin(t, d)
	assetsDir := t.TempDir()
	insertSession(t, d, "orphan-event", "project")

	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	_, err := d.getWriter().Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, agent_id, subagent_session_id,
			 source, status, content, content_length, timestamp, event_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"orphan-event", 7, 2, "toolu-orphan", "agent-1", "child-1",
		"subagent_notification", "completed", content, len(content),
		"2026-01-01T00:00:00Z", 0,
	)
	require.NoError(t, err)
	clearArtifactExportQueue(t, d)

	put := realPut(assetsDir)
	report, err := d.MigrateToolImages(t.Context(), StripImagesFilter{}, put)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Changed)

	var stored string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "orphan-event",
	).Scan(&stored))
	assert.Contains(t, stored, "image_ref")
	assert.Contains(t, stored, "asset://")
}
