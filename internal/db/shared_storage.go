package db

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/parser"
)

// ErrCodexSessionUnverified marks a legacy Codex session whose stored content
// a parser redactor would still rewrite: an encrypted payload survives on a
// surface the scoped scrub never examined, so the session cannot be certified
// at the shared-storage watermark. The condition is permanent for rows with
// no source file to re-parse, so pushers withhold these sessions instead of
// retrying them as errors.
var ErrCodexSessionUnverified = errors.New(
	"legacy Codex session carries unverified encrypted payloads",
)

// NormalizeCodexSharedStoragePreview applies the metadata-independent preview
// policy used to certify a legacy Codex session for shared storage. It ignores
// relationship metadata deliberately: mirrors must not trust a missing parent
// link when deciding whether parser-owned ciphertext is safe to expose.
func NormalizeCodexSharedStoragePreview(preview string) string {
	return parser.RedactCodexStoredSubagentPreview(preview)
}

// NormalizeCodexSharedStorageMessage applies the metadata-independent message
// policy used to certify a legacy Codex session for shared storage. A user
// turn is checked as an inter-agent delivery even when the session was never
// linked to its parent, and a formatted collaboration message is checked even
// when both its cached tool-use flag and collab tool_calls row were lost. A
// formatted non-collab tool message remains content only when its token-bearing
// blocks have headers no collab tool can produce.
func NormalizeCodexSharedStorageMessage(
	role string,
	hasToolUse, hasCollabTool bool,
	content string,
) string {
	switch {
	case role == "user":
		return parser.RedactCodexStoredSubagentMessage(content)
	case hasCollabTool:
		return parser.RedactCodexStoredToolContent(content)
	case parser.CodexStoredToolContentNeedsCollabRedaction(content):
		return parser.RedactCodexStoredToolContent(content)
	case hasToolUse &&
		!parser.CodexStoredToolContentIsProvablyNonCollab(content):
		return parser.RedactCodexStoredToolContent(content)
	}
	return content
}

// codexPreviewUnverified reports whether a stored session preview still
// carries content the shared-storage normalizer would rewrite.
func codexPreviewUnverified(preview string) bool {
	return NormalizeCodexSharedStoragePreview(preview) != preview
}

// codexMessageUnverified reports whether a stored Codex message still carries
// content the shared-storage normalizer would rewrite.
func codexMessageUnverified(
	role string,
	hasToolUse, hasCollabTool bool,
	content string,
) bool {
	return NormalizeCodexSharedStorageMessage(
		role, hasToolUse, hasCollabTool, content,
	) != content
}

// ExpectedSharedStorageDataVersion returns the compatibility version that a
// verified session will publish. It does not verify content; write paths must
// call PrepareSessionForSharedStorage before using the returned watermark.
func ExpectedSharedStorageDataVersion(sess Session) int {
	if sess.Agent == "codex" && sess.DataVersion < redactedCodexSourceDataVersion {
		return redactedCodexSourceDataVersion
	}
	return sess.DataVersion
}

// PrepareSessionForSharedStorage verifies every parser-owned Codex payload
// surface before publishing a legacy session at the shared-storage watermark.
// The checks mirror certifiedCopiedCodexSessionIDs: they deliberately ignore
// relationship metadata and tool_calls presence, so an encrypted delivery in
// a child that was never linked to its parent, or a formatted collab payload
// whose tool_calls row was lost, fails verification instead of being
// certified. Literal Fernet lookalikes survive because the parser redactors
// only change content that they can identify as an encrypted or clipped
// payload. Verification failures wrap ErrCodexSessionUnverified.
func PrepareSessionForSharedStorage(
	sess Session, messages []Message,
) (Session, error) {
	if sess.Agent != "codex" ||
		sess.DataVersion >= redactedCodexSourceDataVersion {
		return sess, nil
	}
	if sess.FirstMessage != nil && codexPreviewUnverified(*sess.FirstMessage) {
		return sess, fmt.Errorf(
			"session %s preview requires encrypted-payload repair: %w",
			sess.ID, ErrCodexSessionUnverified,
		)
	}
	for _, message := range messages {
		hasCollabTool := false
		for _, toolCall := range message.ToolCalls {
			if parser.IsCodexCollabTool(toolCall.ToolName) {
				hasCollabTool = true
				break
			}
		}
		if codexMessageUnverified(
			message.Role, message.HasToolUse, hasCollabTool, message.Content,
		) {
			return sess, fmt.Errorf(
				"session %s message %d requires encrypted-payload repair: %w",
				sess.ID, message.Ordinal, ErrCodexSessionUnverified,
			)
		}
		for _, toolCall := range message.ToolCalls {
			if !parser.IsCodexCollabTool(toolCall.ToolName) {
				continue
			}
			if parser.RedactCodexEncryptedTokens(toolCall.InputJSON) != toolCall.InputJSON {
				return sess, fmt.Errorf(
					"session %s tool call %s requires encrypted-payload repair: %w",
					sess.ID, toolCall.ToolName, ErrCodexSessionUnverified,
				)
			}
		}
	}
	sess.DataVersion = redactedCodexSourceDataVersion
	if sess.TranscriptRevision != nil {
		sess.CodexPayloadCertifiedRevision = *sess.TranscriptRevision
	}
	sess.CodexPayloadCertificationVersion = redactedCodexSourceDataVersion
	return sess, nil
}

// SetCodexSharedStorageCertification records that the transcript at revision
// passed the current Codex payload verification. The parser data_version is
// deliberately untouched: certification proves only that shared-storage
// payload surfaces are safe, not that every historical parser rewrite ran.
// The revision predicate makes a concurrent transcript change fail closed.
func (db *DB) SetCodexSharedStorageCertification(
	id, revision string, preview *string,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	var expectedPreview any
	if preview != nil {
		expectedPreview = *preview
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().Exec(`
		UPDATE sessions
		   SET codex_payload_certified_revision = ?,
		       codex_payload_certification_version = ?,
		       local_modified_at = CASE
		           WHEN codex_payload_certified_revision <> ?
		             OR codex_payload_certification_version <> ?
		           THEN strftime('%Y-%m-%dT%H:%M:%fZ','now')
		           ELSE local_modified_at
		       END
		 WHERE id = ?
		   AND agent = 'codex'
		   AND data_version < ?
		   AND transcript_revision = ?
		   AND first_message IS ?`,
		revision, redactedCodexSourceDataVersion,
		revision, redactedCodexSourceDataVersion, id,
		redactedCodexSourceDataVersion, revision, expectedPreview,
	)
	if err != nil {
		return fmt.Errorf(
			"certifying Codex shared-storage payloads for %s: %w", id, err,
		)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"reading Codex certification result for %s: %w", id, err,
		)
	}
	if rows != 1 {
		return fmt.Errorf(
			"certifying Codex shared-storage payloads for %s: verified content changed",
			id,
		)
	}
	return nil
}

// UnverifiedCodexSessionIDs returns Codex sessions whose parser data predates
// the redaction watermark and whose current transcript revision lacks a
// matching payload certification. Mirror pushers use it to keep dependent
// rows — vector documents embed the same message content — from publishing
// payloads that session-level verification withheld, even on pushes where the
// session rows themselves are outside the incremental window.
func (db *DB) UnverifiedCodexSessionIDs(ctx context.Context) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id FROM sessions
		WHERE agent = 'codex'
		  AND data_version < ?
		  AND (
		      codex_payload_certification_version <> ?
		      OR codex_payload_certified_revision <> transcript_revision
		  )
		ORDER BY id`,
		redactedCodexSourceDataVersion, redactedCodexSourceDataVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("querying unverified codex sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning unverified codex session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating unverified codex sessions: %w", err)
	}
	return ids, nil
}
