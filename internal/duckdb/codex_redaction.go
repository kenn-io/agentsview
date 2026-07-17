package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const (
	// codexEncryptedPayloadDataVersion aliases the archive-side watermark so
	// the DuckDB guards, the PG guards, and the SQLite copy scrub cannot
	// drift apart.
	codexEncryptedPayloadDataVersion       = db.CodexRedactionDataVersion
	codexEncryptedPayloadRepairMetadataKey = "agentsview_codex_encrypted_payload_repair_v1"
)

// ErrCodexEncryptedPayloadRepairRequired reports a DuckDB mirror that can
// still expose parser-owned Codex ciphertext.
var ErrCodexEncryptedPayloadRepairRequired = errors.New(
	"DuckDB Codex encrypted-payload repair is required",
)

type codexDuckDBMessageRepair struct {
	sessionID string
	ordinal   int
	role      string
	content   string
	length    int
}

type codexDuckDBToolInputRepair struct {
	messageID int64
	callIndex int
	sessionID string
	toolName  string
	content   string
	inputNull bool
}

type codexDuckDBPreviewRepair struct {
	id      string
	content string
}

type codexDuckDBRepairs struct {
	messages   []codexDuckDBMessageRepair
	toolInputs []codexDuckDBToolInputRepair
	previews   []codexDuckDBPreviewRepair
}

func (r codexDuckDBRepairs) contentSessionIDs() []string {
	ids := make(map[string]struct{}, len(r.messages)+len(r.toolInputs))
	for _, row := range r.messages {
		ids[row.sessionID] = struct{}{}
	}
	for _, row := range r.toolInputs {
		ids[row.sessionID] = struct{}{}
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	return result
}

type codexDuckDBQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type quackCodexDuckDBQueryer struct {
	db *sql.DB
}

func (q quackCodexDuckDBQueryer) QueryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	rendered, err := duckSQLWithArgs(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rendering DuckDB Codex compatibility query: %w", err)
	}
	return q.db.QueryContext(ctx,
		"SELECT * FROM "+quackAttachmentName+".query(?)", rendered,
	)
}

// collectCodexDuckDBRepairs first applies the shared-storage UTF-8/control-byte
// normalization to every legacy payload surface, then finds parser-owned
// ciphertext written by older Codex parsers. Destructive token redaction stays
// scoped to relationship and tool-call metadata; exhaustive certification
// below withholds the watermark when that scoped repair cannot vouch for a row.
func collectCodexDuckDBRepairs(
	ctx context.Context, q codexDuckDBQueryer,
) (codexDuckDBRepairs, error) {
	var repairs codexDuckDBRepairs
	type messageKey struct {
		sessionID string
		messageID int64
	}
	collabMessages := make(map[messageKey]bool)

	rows, err := q.QueryContext(ctx, `
SELECT tc.message_id, tc.call_index, tc.session_id, tc.tool_name,
	   COALESCE(tc.input_json, ''), tc.input_json IS NULL
  FROM tool_calls tc
  JOIN sessions s ON s.id = tc.session_id
 WHERE s.agent = 'codex'
	AND s.data_version < ?`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return repairs, fmt.Errorf("querying DuckDB Codex tool inputs: %w", err)
	}
	for rows.Next() {
		var row codexDuckDBToolInputRepair
		var storedToolName, storedContent string
		if err := rows.Scan(
			&row.messageID, &row.callIndex, &row.sessionID,
			&storedToolName, &storedContent, &row.inputNull,
		); err != nil {
			rows.Close()
			return repairs, fmt.Errorf("scanning DuckDB Codex tool input: %w", err)
		}
		row.toolName = db.SanitizeUTF8(storedToolName)
		row.content = db.SanitizeUTF8(storedContent)
		if parser.IsCodexCollabTool(row.toolName) {
			collabMessages[messageKey{row.sessionID, row.messageID}] = true
			row.content = parser.RedactCodexEncryptedTokens(row.content)
		}
		if row.toolName == storedToolName && row.content == storedContent {
			continue
		}
		repairs.toolInputs = append(repairs.toolInputs, row)
	}
	if err := closeCodexDuckDBRows(rows, "tool inputs"); err != nil {
		return repairs, err
	}

	rows, err = q.QueryContext(ctx, `
SELECT m.id, m.session_id, m.ordinal, m.content, m.content_length,
	   m.role, m.has_tool_use, s.relationship_type
  FROM messages m
  JOIN sessions s ON s.id = m.session_id
 WHERE s.agent = 'codex'
	AND s.data_version < ?`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return repairs, fmt.Errorf("querying DuckDB Codex messages: %w", err)
	}
	for rows.Next() {
		var row codexDuckDBMessageRepair
		var messageID int64
		var storedRole, storedContent, relationshipType string
		var storedLength int
		var hasToolUse bool
		if err := rows.Scan(
			&messageID, &row.sessionID, &row.ordinal, &storedContent,
			&storedLength, &storedRole, &hasToolUse, &relationshipType,
		); err != nil {
			rows.Close()
			return repairs, fmt.Errorf("scanning DuckDB Codex message: %w", err)
		}
		row.role = db.SanitizeUTF8(storedRole)
		row.content = db.SanitizeUTF8(storedContent)
		hasCollabTool := collabMessages[messageKey{row.sessionID, messageID}]
		if (db.SanitizeUTF8(relationshipType) == "subagent" && row.role == "user") ||
			hasCollabTool {
			row.content = db.NormalizeCodexSharedStorageMessage(
				row.role, hasToolUse, hasCollabTool, row.content,
			)
		}
		if row.role == storedRole && row.content == storedContent {
			continue
		}
		row.length = max(storedLength+len(row.content)-len(storedContent), 0)
		repairs.messages = append(repairs.messages, row)
	}
	if err := closeCodexDuckDBRows(rows, "messages"); err != nil {
		return repairs, err
	}

	rows, err = q.QueryContext(ctx, `
SELECT id, COALESCE(first_message, ''), relationship_type
  FROM sessions
 WHERE agent = 'codex'
	AND data_version < ?`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return repairs, fmt.Errorf("querying DuckDB Codex previews: %w", err)
	}
	for rows.Next() {
		var row codexDuckDBPreviewRepair
		var storedContent, relationshipType string
		if err := rows.Scan(&row.id, &storedContent, &relationshipType); err != nil {
			rows.Close()
			return repairs, fmt.Errorf("scanning DuckDB Codex preview: %w", err)
		}
		row.content = db.SanitizeUTF8(storedContent)
		if db.SanitizeUTF8(relationshipType) == "subagent" {
			row.content = db.NormalizeCodexSharedStoragePreview(row.content)
		}
		if row.content == storedContent {
			continue
		}
		repairs.previews = append(repairs.previews, row)
	}
	if err := closeCodexDuckDBRows(rows, "previews"); err != nil {
		return repairs, err
	}

	return repairs, nil
}

func closeCodexDuckDBRows(rows *sql.Rows, label string) error {
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterating DuckDB Codex %s: %w", label, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing DuckDB Codex %s: %w", label, err)
	}
	return nil
}

// promoteVerifiedCodexSessionsDuckDB advances only legacy sessions whose every
// parser-owned payload surface is a fixpoint of the shared-storage normalizers.
// Certification scans every remaining legacy row after normalization has been
// persisted; raw SQL token searches are not sufficient because a removable
// control byte can split a Fernet prefix.
func promoteVerifiedCodexSessionsDuckDB(
	ctx context.Context, tx *sql.Tx,
) error {
	certified, err := certifiedLegacyCodexSessionIDsDuckDB(ctx, tx)
	if err != nil {
		return err
	}
	for _, sessionID := range certified {
		if _, err := tx.ExecContext(ctx, `
UPDATE sessions SET data_version = ?
 WHERE id = ? AND agent = 'codex' AND data_version < ?`,
			codexEncryptedPayloadDataVersion, sessionID,
			codexEncryptedPayloadDataVersion,
		); err != nil {
			return fmt.Errorf(
				"promoting verified DuckDB Codex session %s: %w",
				sessionID, err,
			)
		}
	}
	return nil
}

func certifiedLegacyCodexSessionIDsDuckDB(
	ctx context.Context, q codexDuckDBQueryer,
) ([]string, error) {
	unverified := make(map[string]bool)
	rows, err := q.QueryContext(ctx, `
SELECT s.id, COALESCE(s.first_message, '')
  FROM sessions s
 WHERE s.agent = 'codex'
	AND s.data_version < ?`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return nil, fmt.Errorf("querying DuckDB Codex certification sessions: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id, preview string
		if err := rows.Scan(&id, &preview); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning DuckDB Codex certification session: %w", err)
		}
		ids = append(ids, id)
		unverified[id] = db.NormalizeCodexSharedStoragePreview(preview) != preview
	}
	if err := closeCodexDuckDBRows(rows, "certification sessions"); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err = q.QueryContext(ctx, `
SELECT m.session_id, m.role, m.has_tool_use, m.content,
       EXISTS (
           SELECT 1 FROM tool_calls tc
            WHERE tc.session_id = m.session_id
              AND tc.message_id = m.id
              AND tc.tool_name IN `+parser.CodexCollabToolsSQL()+`
       )
  FROM messages m
  JOIN sessions s ON s.id = m.session_id
 WHERE s.agent = 'codex'
	AND s.data_version < ?`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return nil, fmt.Errorf("querying DuckDB Codex certification messages: %w", err)
	}
	for rows.Next() {
		var sessionID, role, content string
		var hasToolUse, hasCollabTool bool
		if err := rows.Scan(
			&sessionID, &role, &hasToolUse, &content,
			&hasCollabTool,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning DuckDB Codex certification message: %w", err)
		}
		flagged, tracked := unverified[sessionID]
		if tracked && !flagged {
			unverified[sessionID] = db.NormalizeCodexSharedStorageMessage(
				role, hasToolUse, hasCollabTool, content,
			) != content
		}
	}
	if err := closeCodexDuckDBRows(rows, "certification messages"); err != nil {
		return nil, err
	}

	rows, err = q.QueryContext(ctx, `
SELECT tc.session_id, COALESCE(tc.input_json, '')
  FROM tool_calls tc
  JOIN sessions s ON s.id = tc.session_id
 WHERE s.agent = 'codex'
   AND s.data_version < ?
	AND tc.tool_name IN `+parser.CodexCollabToolsSQL(), codexEncryptedPayloadDataVersion)
	if err != nil {
		return nil, fmt.Errorf("querying DuckDB Codex certification tool inputs: %w", err)
	}
	for rows.Next() {
		var sessionID, input string
		if err := rows.Scan(&sessionID, &input); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning DuckDB Codex certification tool input: %w", err)
		}
		flagged, tracked := unverified[sessionID]
		if tracked && !flagged {
			unverified[sessionID] = parser.RedactCodexEncryptedTokens(input) != input
		}
	}
	if err := closeCodexDuckDBRows(rows, "certification tool inputs"); err != nil {
		return nil, err
	}

	certified := make([]string, 0, len(ids))
	for _, id := range ids {
		if !unverified[id] {
			certified = append(certified, id)
		}
	}
	return certified, nil
}

func repairCodexEncryptedPayloadsDuckDB(ctx context.Context, duck *sql.DB) error {
	repairRecorded, err := metadataKeyExists(
		ctx, duck, codexEncryptedPayloadRepairMetadataKey,
	)
	if err != nil {
		return err
	}
	if repairRecorded {
		// Keep checking for rows written later by an older client, but make
		// the common clean-startup path read-only and cheap: every collect
		// query filters on data_version below the watermark, so the LIMIT-1
		// version probe alone decides whether repair work can exist — no
		// content scan is needed until it says yes.
		legacyVersion, err := codexDuckDBHasLegacyPayloadVersion(ctx, duck)
		if err != nil {
			return err
		}
		if !legacyVersion {
			return nil
		}
	}

	tx, err := duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning DuckDB Codex encrypted-payload repair: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	repairs, err := collectCodexDuckDBRepairs(ctx, tx)
	if err != nil {
		return err
	}
	for _, row := range repairs.messages {
		if _, err := tx.ExecContext(ctx, `
UPDATE messages SET role = ?, content = ?, content_length = ?
 WHERE session_id = ? AND ordinal = ?`,
			row.role, row.content, row.length, row.sessionID, row.ordinal,
		); err != nil {
			return fmt.Errorf("updating DuckDB Codex message %s/%d: %w",
				row.sessionID, row.ordinal, err)
		}
	}
	for _, row := range repairs.toolInputs {
		input := any(row.content)
		if row.inputNull {
			input = nil
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE tool_calls SET tool_name = ?, input_json = ?
 WHERE session_id = ? AND message_id = ? AND call_index = ?`,
			row.toolName, input, row.sessionID, row.messageID, row.callIndex,
		); err != nil {
			return fmt.Errorf(
				"updating DuckDB Codex tool input %s/%d/%d: %w",
				row.sessionID, row.messageID, row.callIndex, err,
			)
		}
	}
	for _, row := range repairs.previews {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET first_message = ? WHERE id = ?`,
			row.content, row.id,
		); err != nil {
			return fmt.Errorf("updating DuckDB Codex preview %s: %w", row.id, err)
		}
	}
	for _, sessionID := range repairs.contentSessionIDs() {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM secret_findings WHERE session_id = ?`, sessionID,
		); err != nil {
			return fmt.Errorf("deleting DuckDB Codex secret findings for %s: %w",
				sessionID, err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE sessions
   SET quality_signal_version = 0,
	   health_score = NULL,
	   health_grade = NULL,
       short_prompt_count = 0,
       unstructured_start = FALSE,
       missing_success_criteria_count = 0,
       missing_verification_count = 0,
       duplicate_prompt_count = 0,
       no_code_context_count = 0,
       runaway_tool_loop_count = 0,
       secret_leak_count = 0,
       secrets_rules_version = '',
       transcript_revision = CAST(CAST(transcript_revision AS BIGINT) + 1 AS TEXT)
 WHERE id = ?`, sessionID); err != nil {
			return fmt.Errorf("invalidating DuckDB Codex derived data for %s: %w",
				sessionID, err)
		}
	}
	if err := promoteVerifiedCodexSessionsDuckDB(ctx, tx); err != nil {
		return err
	}
	if !repairRecorded {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO sync_metadata (key, value) VALUES (?, '1')
ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
			codexEncryptedPayloadRepairMetadataKey,
		); err != nil {
			return fmt.Errorf("recording DuckDB Codex payload repair: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing DuckDB Codex encrypted-payload repair: %w", err)
	}
	return nil
}

func checkCodexEncryptedPayloadCompatDuckDB(
	ctx context.Context, q codexDuckDBQueryer,
) error {
	// The version probe alone decides compatibility: every ciphertext
	// surface the repair would collect lives in a session below the
	// watermark (the collect queries all filter on it), and the in-statement
	// Quack read guard gates on the same predicate. Over Quack this also
	// keeps the check to one remote statement instead of five.
	legacyVersion, err := codexDuckDBHasLegacyPayloadVersion(ctx, q)
	if err != nil {
		return err
	}
	if !legacyVersion {
		return nil
	}
	return fmt.Errorf(
		"%w: run a current AgentsView build against the base DuckDB mirror before retrying this read",
		ErrCodexEncryptedPayloadRepairRequired,
	)
}

// codexEncryptedPayloadGuardMessage is raised remotely by the in-statement
// guard and matched client-side to restore the sentinel error, so it must
// stay unique enough not to collide with ordinary remote error text.
const codexEncryptedPayloadGuardMessage = "agentsview: DuckDB Codex encrypted-payload repair required"

// codexGuardedQuackReadSQL embeds the legacy Codex payload check into the
// remote read itself. Quack ships each read as one remote statement, and a
// statement executes under a single snapshot, so a check issued as a separate
// statement can never cover the read: a legacy writer may commit between the
// two, and a concurrent repair may clear the marker after the read already
// returned ciphertext. Wrapping keeps check and read on the same snapshot.
// The guard also fires for empty result sets (DuckDB evaluates the
// uncorrelated subquery regardless of probe rows), matching the previous
// fail-all-reads behavior of the separate check.
func codexGuardedQuackReadSQL(sqlText string) string {
	return "SELECT * FROM (\n" + sqlText + "\n) AS __agentsview_codex_gated" +
		fmt.Sprintf(
			"\nWHERE CASE WHEN EXISTS (SELECT 1 FROM sessions WHERE agent = 'codex' AND data_version < %d) THEN error(%s) ELSE TRUE END",
			codexEncryptedPayloadDataVersion,
			duckLiteral(codexEncryptedPayloadGuardMessage),
		)
}

// codexGuardMappedRows applies mapCodexDuckDBGuardError to every error a
// row iterator can surface. The driver usually reports the remote guard
// failure when the query starts, but database/sql permits deferring
// execution errors to iteration, so the sentinel contract has to hold on
// Scan, Err, and Close as well.
type codexGuardMappedRows struct{ *sql.Rows }

func (r codexGuardMappedRows) Scan(dest ...any) error {
	return mapCodexDuckDBGuardError(r.Rows.Scan(dest...))
}

func (r codexGuardMappedRows) Err() error {
	return mapCodexDuckDBGuardError(r.Rows.Err())
}

func (r codexGuardMappedRows) Close() error {
	return mapCodexDuckDBGuardError(r.Rows.Close())
}

// mapCodexDuckDBGuardError converts the remote guard failure back into the
// package sentinel so callers observe the same error as the startup check.
func mapCodexDuckDBGuardError(err error) error {
	if err == nil ||
		!strings.Contains(err.Error(), codexEncryptedPayloadGuardMessage) {
		return err
	}
	return fmt.Errorf(
		"%w: a legacy Codex push is visible on the Quack remote; run a current AgentsView build against the base DuckDB mirror before retrying this read",
		ErrCodexEncryptedPayloadRepairRequired,
	)
}

func codexDuckDBHasLegacyPayloadVersion(
	ctx context.Context, q codexDuckDBQueryer,
) (bool, error) {
	rows, err := q.QueryContext(ctx, `
SELECT 1 FROM sessions
 WHERE agent = 'codex' AND data_version < ?
 LIMIT 1`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return false, fmt.Errorf("checking DuckDB Codex payload versions: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return true, nil
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterating DuckDB Codex payload versions: %w", err)
	}
	return false, nil
}
