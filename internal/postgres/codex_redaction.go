package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const (
	// codexEncryptedPayloadDataVersion aliases the archive-side watermark so
	// the PG guards, the DuckDB guards, and the SQLite copy scrub cannot
	// drift apart. Bumping it changes the predicate baked into the persisted
	// trigger bodies below, which requires new trigger names (see
	// codexPayloadWriteGuardsInstalledPG).
	codexEncryptedPayloadDataVersion   = db.CodexRedactionDataVersion
	codexVectorRepairCompletedMetadata = "codex_encrypted_payload_vectors_v1"
	codexSessionWriteGuardTrigger      = "agentsview_codex_payload_session_guard_v7"
	codexMessageWriteGuardTrigger      = "agentsview_codex_payload_message_guard_v5"
	codexToolWriteGuardTrigger         = "agentsview_codex_payload_tool_guard_v4"
	codexVectorWriteGuardTrigger       = "agentsview_codex_payload_vector_guard_v4"
	previousCodexSessionWriteGuardV6   = "agentsview_codex_payload_session_guard_v6"
	previousCodexSessionWriteGuardV5   = "agentsview_codex_payload_session_guard_v5"
	previousCodexSessionWriteGuardV4   = "agentsview_codex_payload_session_guard_v4"
	previousCodexMessageWriteGuardV4   = "agentsview_codex_payload_message_guard_v4"
	previousCodexToolWriteGuardV3      = "agentsview_codex_payload_tool_guard_v3"
	previousCodexVectorWriteGuardV3    = "agentsview_codex_payload_vector_guard_v3"
	previousCodexSessionWriteGuardV3   = "agentsview_codex_payload_session_guard_v3"
	previousCodexMessageWriteGuardV3   = "agentsview_codex_payload_message_guard_v3"
	previousCodexSessionWriteGuardV2   = "agentsview_codex_payload_session_guard_v2"
	previousCodexMessageWriteGuardV2   = "agentsview_codex_payload_message_guard_v2"
	previousCodexToolWriteGuardV2      = "agentsview_codex_payload_tool_guard_v2"
	previousCodexVectorWriteGuardV2    = "agentsview_codex_payload_vector_guard_v2"
	legacyCodexSessionWriteGuard       = "agentsview_codex_payload_session_guard"
	legacyCodexMessageWriteGuard       = "agentsview_codex_payload_message_guard"
	legacyCodexToolWriteGuard          = "agentsview_codex_payload_tool_guard"
	legacyCodexVectorWriteGuard        = "agentsview_codex_payload_vector_guard"
	codexPayloadRepairSetting          = "agentsview.codex_payload_repair_version"
	// codexVectorRepairAdvisoryLockKey serializes the one-time exclusive
	// vector repair against ordinary vector mutations on PostgreSQL. Vector
	// pushes take the shared form, so unrelated sessions can still be pushed
	// concurrently. CockroachDB has no advisory locks; see
	// lockCodexVectorRepairExclusivePG.
	codexVectorRepairAdvisoryLockKey int64 = 0x4156434458565231 // "AVCDXVR1"
	// codexPayloadGuardModeMetadata records how legacy Codex ciphertext
	// writes are kept out of this schema: "triggers" when the write-guard
	// triggers are installed, "scan" when the server cannot run triggers
	// (CockroachDB before v24.3) and read-side gating falls back to scanning
	// for legacy ciphertext.
	codexPayloadGuardModeMetadata = "codex_payload_guard_mode"
	codexPayloadGuardModeTriggers = "triggers"
	codexPayloadGuardModeScan     = "scan"
)

// ErrCodexEncryptedPayloadRepairRequired reports a PostgreSQL dataset that a
// read-only client cannot safely expose until a writable current build applies
// the dataVersion 68 repair and exhaustive shared-storage certification.
var ErrCodexEncryptedPayloadRepairRequired = errors.New(
	"PostgreSQL Codex encrypted-payload repair is required",
)

type codexPGMessageRepair struct {
	sessionID string
	ordinal   int
	role      string
	content   string
	length    int
}

type codexPGToolInputRepair struct {
	id        int64
	sessionID string
	toolName  string
	content   string
}

type codexPGPreviewRepair struct {
	id      string
	content string
}

type codexPGRepairs struct {
	messages   []codexPGMessageRepair
	toolInputs []codexPGToolInputRepair
	previews   []codexPGPreviewRepair
}

func (r codexPGRepairs) needed() bool {
	return len(r.messages) > 0 || len(r.toolInputs) > 0 || len(r.previews) > 0
}

func (r codexPGRepairs) sessionIDs() []string {
	ids := make(map[string]struct{}, len(r.messages)+len(r.toolInputs)+len(r.previews))
	for _, row := range r.messages {
		ids[row.sessionID] = struct{}{}
	}
	for _, row := range r.toolInputs {
		ids[row.sessionID] = struct{}{}
	}
	for _, row := range r.previews {
		ids[row.id] = struct{}{}
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	return result
}

func (r codexPGRepairs) contentSessionIDs() []string {
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

type codexPGQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// collectCodexPGRepairs first applies the shared-storage UTF-8/control-byte
// normalization to every legacy payload surface, then finds parser-owned
// ciphertext written by older Codex parsers. Destructive token redaction stays
// scoped to relationship and tool-call metadata so legitimate root text and
// non-collab tool arguments remain untouched. The separate post-repair
// certification deliberately ignores missing metadata and withholds the
// watermark when the scoped repair cannot vouch for a row.
func collectCodexPGRepairs(
	ctx context.Context, q codexPGQueryer,
) (codexPGRepairs, error) {
	var repairs codexPGRepairs
	type messageKey struct {
		sessionID string
		ordinal   int
	}
	collabMessages := make(map[messageKey]bool)

	rows, err := q.QueryContext(ctx, `
SELECT tc.id, tc.session_id, tc.message_ordinal, tc.tool_name,
       COALESCE(tc.input_json, '')
  FROM tool_calls tc
  JOIN sessions s ON s.id = tc.session_id
 WHERE s.agent = 'codex'
   AND s.data_version < $1
	`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return repairs, fmt.Errorf("querying PG Codex tool inputs: %w", err)
	}
	for rows.Next() {
		var row codexPGToolInputRepair
		var ordinal int
		var storedToolName, storedContent string
		if err := rows.Scan(
			&row.id, &row.sessionID, &ordinal, &storedToolName, &storedContent,
		); err != nil {
			rows.Close()
			return repairs, fmt.Errorf("scanning PG Codex tool input: %w", err)
		}
		row.toolName = db.SanitizeUTF8(storedToolName)
		row.content = db.SanitizeUTF8(storedContent)
		if parser.IsCodexCollabTool(row.toolName) {
			collabMessages[messageKey{row.sessionID, ordinal}] = true
			row.content = parser.RedactCodexEncryptedTokens(row.content)
		}
		if row.toolName == storedToolName && row.content == storedContent {
			continue
		}
		repairs.toolInputs = append(repairs.toolInputs, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return repairs, fmt.Errorf("iterating PG Codex tool inputs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return repairs, fmt.Errorf("closing PG Codex tool inputs: %w", err)
	}

	rows, err = q.QueryContext(ctx, `
SELECT m.session_id, m.ordinal, m.content, m.content_length,
	   m.role, m.has_tool_use, s.relationship_type
  FROM messages m
  JOIN sessions s ON s.id = m.session_id
 WHERE s.agent = 'codex'
   AND s.data_version < $1
	`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return repairs, fmt.Errorf("querying PG Codex messages: %w", err)
	}
	for rows.Next() {
		var row codexPGMessageRepair
		var storedRole, storedContent, relationshipType string
		var storedLength int
		var hasToolUse bool
		if err := rows.Scan(
			&row.sessionID, &row.ordinal, &storedContent, &storedLength,
			&storedRole, &hasToolUse, &relationshipType,
		); err != nil {
			rows.Close()
			return repairs, fmt.Errorf("scanning PG Codex message: %w", err)
		}
		row.role = db.SanitizeUTF8(storedRole)
		row.content = db.SanitizeUTF8(storedContent)
		hasCollabTool := collabMessages[messageKey{row.sessionID, row.ordinal}]
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return repairs, fmt.Errorf("iterating PG Codex messages: %w", err)
	}
	if err := rows.Close(); err != nil {
		return repairs, fmt.Errorf("closing PG Codex messages: %w", err)
	}

	rows, err = q.QueryContext(ctx, `
SELECT id, COALESCE(first_message, ''), relationship_type
  FROM sessions
 WHERE agent = 'codex'
	AND data_version < $1`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return repairs, fmt.Errorf("querying PG Codex previews: %w", err)
	}
	for rows.Next() {
		var row codexPGPreviewRepair
		var storedContent, relationshipType string
		if err := rows.Scan(&row.id, &storedContent, &relationshipType); err != nil {
			rows.Close()
			return repairs, fmt.Errorf("scanning PG Codex preview: %w", err)
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return repairs, fmt.Errorf("iterating PG Codex previews: %w", err)
	}
	if err := rows.Close(); err != nil {
		return repairs, fmt.Errorf("closing PG Codex previews: %w", err)
	}

	return repairs, nil
}

func codexPGHasLegacyPayloadVersion(
	ctx context.Context, q codexPGRowQueryer,
) (bool, error) {
	var legacy bool
	if err := q.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM sessions
     WHERE agent = 'codex' AND data_version < $1
)`, codexEncryptedPayloadDataVersion).Scan(&legacy); err != nil {
		return false, fmt.Errorf("checking PG Codex payload versions: %w", err)
	}
	return legacy, nil
}

// promoteVerifiedCodexSessionsPG advances only legacy sessions whose every
// parser-owned payload surface is a fixpoint of the shared-storage normalizers.
// Certification scans every remaining legacy row after normalization has been
// persisted; raw SQL token searches are not sufficient because a removable
// control byte can split a Fernet prefix.
func promoteVerifiedCodexSessionsPG(
	ctx context.Context, tx *sql.Tx,
) error {
	certified, err := certifiedLegacyCodexSessionIDsPG(ctx, tx)
	if err != nil {
		return err
	}
	for _, sessionID := range certified {
		if _, err := tx.ExecContext(ctx, `
UPDATE sessions SET data_version = $1
 WHERE id = $2 AND agent = 'codex' AND data_version < $1`,
			codexEncryptedPayloadDataVersion, sessionID,
		); err != nil {
			return fmt.Errorf(
				"promoting verified PG Codex session %s: %w", sessionID, err,
			)
		}
	}
	return nil
}

func certifiedLegacyCodexSessionIDsPG(
	ctx context.Context, q codexPGQueryer,
) ([]string, error) {
	unverified := make(map[string]bool)
	rows, err := q.QueryContext(ctx, `
SELECT s.id, COALESCE(s.first_message, '')
  FROM sessions s
 WHERE s.agent = 'codex'
   AND s.data_version < $1`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return nil, fmt.Errorf("querying PG Codex certification sessions: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id, preview string
		if err := rows.Scan(&id, &preview); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning PG Codex certification session: %w", err)
		}
		ids = append(ids, id)
		unverified[id] = db.NormalizeCodexSharedStoragePreview(preview) != preview
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterating PG Codex certification sessions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing PG Codex certification sessions: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err = q.QueryContext(ctx, `
SELECT m.session_id, m.role, m.has_tool_use, m.content,
       EXISTS (
           SELECT 1 FROM tool_calls tc
            WHERE tc.session_id = m.session_id
              AND tc.message_ordinal = m.ordinal
              AND tc.tool_name IN `+parser.CodexCollabToolsSQL()+`
       )
  FROM messages m
  JOIN sessions s ON s.id = m.session_id
 WHERE s.agent = 'codex'
   AND s.data_version < $1`, codexEncryptedPayloadDataVersion)
	if err != nil {
		return nil, fmt.Errorf("querying PG Codex certification messages: %w", err)
	}
	for rows.Next() {
		var sessionID, role, content string
		var hasToolUse, hasCollabTool bool
		if err := rows.Scan(
			&sessionID, &role, &hasToolUse, &content,
			&hasCollabTool,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning PG Codex certification message: %w", err)
		}
		flagged, tracked := unverified[sessionID]
		if tracked && !flagged {
			unverified[sessionID] = db.NormalizeCodexSharedStorageMessage(
				role, hasToolUse, hasCollabTool, content,
			) != content
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterating PG Codex certification messages: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing PG Codex certification messages: %w", err)
	}

	rows, err = q.QueryContext(ctx, `
SELECT tc.session_id, COALESCE(tc.input_json, '')
  FROM tool_calls tc
  JOIN sessions s ON s.id = tc.session_id
 WHERE s.agent = 'codex'
   AND s.data_version < $1
   AND tc.tool_name IN `+parser.CodexCollabToolsSQL(), codexEncryptedPayloadDataVersion)
	if err != nil {
		return nil, fmt.Errorf("querying PG Codex certification tool inputs: %w", err)
	}
	for rows.Next() {
		var sessionID, input string
		if err := rows.Scan(&sessionID, &input); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning PG Codex certification tool input: %w", err)
		}
		flagged, tracked := unverified[sessionID]
		if tracked && !flagged {
			unverified[sessionID] = parser.RedactCodexEncryptedTokens(input) != input
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterating PG Codex certification tool inputs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing PG Codex certification tool inputs: %w", err)
	}

	certified := make([]string, 0, len(ids))
	for _, id := range ids {
		if !unverified[id] {
			certified = append(certified, id)
		}
	}
	return certified, nil
}

func markCodexPayloadRepairPG(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config($1, $2, true)`,
		codexPayloadRepairSetting,
		fmt.Sprintf("%d", codexEncryptedPayloadDataVersion),
	); err != nil {
		return fmt.Errorf("marking PG Codex payload repair transaction: %w", err)
	}
	return nil
}

// repairCodexEncryptedPayloadsPG applies the current Codex payload repair to
// PG rows.
// The first run invalidates vectors for every Codex session and records an
// independent completion marker. Vector documents are pushed separately from
// session rows, so data_version cannot prove that an existing vector is safe.
// Later repairs invalidate only sessions whose stored or vector content is
// stale, allowing freshly rebuilt vectors to survive.
func repairCodexEncryptedPayloadsPG(
	ctx context.Context, pg *sql.DB, crdb bool,
) error {
	preflight, err := collectCodexPGRepairs(ctx, pg)
	if err != nil {
		// A legacy schema can reach the full EnsureSchema path precisely because
		// these columns or tables are absent. Column migration runs before this
		// repair; tolerate drivers that still report the pre-migration shape and
		// let the next schema check retry rather than masking the DDL result.
		if isUndefinedTable(err) || isUndefinedColumn(err) {
			return nil
		}
		return err
	}
	legacyVersion, err := codexPGHasLegacyPayloadVersion(ctx, pg)
	if err != nil {
		return err
	}
	vectorRepairComplete, err := codexVectorRepairCompletePG(ctx, pg)
	if err != nil {
		return err
	}
	if vectorRepairComplete && !preflight.needed() && !legacyVersion {
		staleVectorSessionIDs, err := staleCodexVectorSessionIDsPG(ctx, pg)
		if err != nil {
			return err
		}
		if len(staleVectorSessionIDs) == 0 {
			return nil
		}
	} else if !vectorRepairComplete && !preflight.needed() && !legacyVersion {
		hasDocs, err := codexPGHasTable(ctx, pg, "vector_documents")
		if err != nil {
			return err
		}
		if !hasDocs {
			// No vector mutation can race before the vector table exists. Current
			// schemas include it, so the normal marker-only path continues below
			// and takes the exclusive advisory lock before rechecking.
			return markCodexVectorRepairCompletePG(ctx, pg)
		}
	}

	return withCodexRepairTxPG(ctx, pg, crdb, func(tx *sql.Tx) error {
		if err := lockCodexVectorRepairExclusivePG(ctx, tx, crdb); err != nil {
			return err
		}
		return repairCodexEncryptedPayloadsPGTx(ctx, tx)
	})
}

func repairCodexEncryptedPayloadsPGTx(ctx context.Context, tx *sql.Tx) error {
	if err := markCodexPayloadRepairPG(ctx, tx); err != nil {
		return err
	}
	repairs, err := collectCodexPGRepairs(ctx, tx)
	if err != nil {
		// A legacy schema can reach the full EnsureSchema path precisely because
		// these columns or tables are absent. Column migration runs before this
		// repair; tolerate drivers that still report the pre-migration shape and
		// let the next schema check retry rather than masking the DDL result.
		if isUndefinedTable(err) || isUndefinedColumn(err) {
			return nil
		}
		return err
	}
	legacyVersion, err := codexPGHasLegacyPayloadVersion(ctx, tx)
	if err != nil {
		return err
	}
	vectorRepairComplete, err := codexVectorRepairCompletePG(ctx, tx)
	if err != nil {
		return err
	}
	var staleVectorSessionIDs []string
	if vectorRepairComplete {
		staleVectorSessionIDs, err = staleCodexVectorSessionIDsPG(ctx, tx)
		if err != nil {
			return err
		}
		if !repairs.needed() && len(staleVectorSessionIDs) == 0 && !legacyVersion {
			return nil
		}
	} else {
		vectors, err := codexVectorsExistPG(ctx, tx)
		if err != nil {
			return err
		}
		if !repairs.needed() && !vectors && !legacyVersion {
			if err := markCodexVectorRepairCompletePG(ctx, tx); err != nil {
				return err
			}
			return nil
		}
	}
	for _, row := range repairs.messages {
		if _, err := tx.ExecContext(ctx, `
UPDATE messages SET role = $1, content = $2, content_length = $3
 WHERE session_id = $4 AND ordinal = $5`,
			row.role, row.content, row.length, row.sessionID, row.ordinal,
		); err != nil {
			return fmt.Errorf("updating PG Codex message %s/%d: %w",
				row.sessionID, row.ordinal, err)
		}
	}
	for _, row := range repairs.toolInputs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tool_calls SET tool_name = $1, input_json = $2 WHERE id = $3`,
			row.toolName, row.content, row.id,
		); err != nil {
			return fmt.Errorf("updating PG Codex tool input %d: %w", row.id, err)
		}
	}
	for _, row := range repairs.previews {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET first_message = $1 WHERE id = $2`,
			row.content, row.id,
		); err != nil {
			return fmt.Errorf("updating PG Codex preview %s: %w", row.id, err)
		}
	}
	if err := promoteVerifiedCodexSessionsPG(ctx, tx); err != nil {
		return err
	}
	if err := invalidateCodexDerivedDataPG(
		ctx, tx, repairs.contentSessionIDs(),
	); err != nil {
		return err
	}
	invalidatedSessionIDs := append(repairs.sessionIDs(), staleVectorSessionIDs...)
	if !vectorRepairComplete || len(invalidatedSessionIDs) > 0 {
		if err := invalidateCodexVectorsPG(
			ctx, tx, invalidatedSessionIDs, !vectorRepairComplete,
		); err != nil {
			return err
		}
	}
	if err := markCodexVectorRepairCompletePG(ctx, tx); err != nil {
		return err
	}
	return nil
}

// lockCodexVectorRepairExclusivePG serializes the one-time vector repair
// against concurrent vector pushes on PostgreSQL. CockroachDB has no
// advisory locks (crdb#13546); its SERIALIZABLE isolation instead forces one
// of two conflicting transactions to retry, so the CockroachDB path takes no
// lock and callers retry on serialization failures (withCodexRepairTxPG).
func lockCodexVectorRepairExclusivePG(
	ctx context.Context, tx *sql.Tx, crdb bool,
) error {
	if crdb {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock($1)`, codexVectorRepairAdvisoryLockKey,
	); err != nil {
		return fmt.Errorf("locking PG Codex vector repair: %w", err)
	}
	return nil
}

func lockCodexVectorMutationSharedPG(
	ctx context.Context, tx *sql.Tx, crdb bool,
) error {
	if !crdb {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock_shared($1)`,
			codexVectorRepairAdvisoryLockKey,
		); err != nil {
			return fmt.Errorf("locking PG Codex vector mutation: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('agentsview.codex_payload_data_version', $1, true)`,
		fmt.Sprintf("%d", codexEncryptedPayloadDataVersion),
	); err != nil {
		return fmt.Errorf("marking PG Codex vector mutation compatibility: %w", err)
	}
	return nil
}

// withCodexRepairTxPG runs fn inside a transaction. On CockroachDB the work
// that PostgreSQL serializes with advisory locks surfaces conflicts as
// SQLSTATE 40001 instead, so the transaction retries a bounded number of
// times there; on PostgreSQL a failure is returned as-is.
func withCodexRepairTxPG(
	ctx context.Context, pg *sql.DB, crdb bool, fn func(*sql.Tx) error,
) error {
	const maxAttempts = 5
	for attempt := 1; ; attempt++ {
		err := func() error {
			tx, err := pg.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf(
					"beginning PG Codex encrypted-payload repair: %w", err,
				)
			}
			defer func() { _ = tx.Rollback() }()
			if err := fn(tx); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf(
					"committing PG Codex encrypted-payload repair: %w", err,
				)
			}
			return nil
		}()
		if err == nil || !crdb || attempt == maxAttempts ||
			!isSerializationFailure(err) {
			return err
		}
	}
}

func ensureCodexEncryptedPayloadCompatibilityPG(
	ctx context.Context, pg *sql.DB,
) error {
	crdb, err := serverIsCockroachDBPG(ctx, pg)
	if err != nil {
		return err
	}
	installed, err := codexPayloadWriteGuardsInstalledPG(ctx, pg)
	if err != nil {
		return err
	}
	if installed {
		return repairCodexEncryptedPayloadsPG(ctx, pg, crdb)
	}
	if crdb {
		return installCodexPayloadCompatibilityCRDB(ctx, pg)
	}
	return installCodexPayloadCompatibilityPostgres(ctx, pg)
}

// installCodexPayloadCompatibilityPostgres installs the write guards and
// applies the repair in one transaction that locks the guarded tables, so no
// legacy write can slip between the repair scan and the trigger install.
func installCodexPayloadCompatibilityPostgres(
	ctx context.Context, pg *sql.DB,
) error {
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning PG Codex payload write-guard migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCodexVectorRepairExclusivePG(ctx, tx, false); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
LOCK TABLE sessions, messages, tool_calls IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("locking PG Codex payload tables: %w", err)
	}
	hasVectorDocuments, err := pgTxHasTable(ctx, tx, "vector_documents")
	if err != nil {
		return err
	}
	if hasVectorDocuments {
		if _, err := tx.ExecContext(ctx, `
LOCK TABLE vector_documents IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("locking PG Codex vector payload table: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_sessions_codex_data_version
    ON sessions (data_version) WHERE agent = 'codex'`); err != nil {
		return fmt.Errorf("indexing PG Codex payload versions: %w", err)
	}
	if err := installCodexPayloadWriteGuardsPG(
		ctx, tx, false, hasVectorDocuments,
	); err != nil {
		return err
	}
	if err := repairCodexEncryptedPayloadsPGTx(ctx, tx); err != nil {
		return err
	}
	if err := setCodexPayloadGuardModePG(
		ctx, tx, codexPayloadGuardModeTriggers,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing PG Codex payload write guards: %w", err)
	}
	return nil
}

// installCodexPayloadCompatibilityCRDB ports the write-guard migration to
// CockroachDB, which cannot run it as one locked transaction: advisory locks
// and LOCK TABLE are unsupported (crdb#13546), and the default
// autocommit_before_ddl setting commits the surrounding transaction before
// every DDL statement anyway. The staged order stays safe without
// atomicity: guards install first via autocommitted DDL, so legacy writers
// are rejected from that point on, and the repair transaction that follows
// redacts anything that landed earlier. Concurrent migrations are idempotent
// (CREATE OR REPLACE plus DROP/CREATE tolerating the duplicate-trigger
// race), and conflicting repairs retry on serialization failures.
//
// CockroachDB gained row-level triggers in v24.3. On older servers the guard
// DDL fails with feature-unsupported errors; the migration then records scan
// mode in sync_metadata, still repairs the rows, and read-side gating falls
// back to scanning for legacy ciphertext (CheckCodexEncryptedPayloadCompat).
func installCodexPayloadCompatibilityCRDB(
	ctx context.Context, pg *sql.DB,
) error {
	if _, err := pg.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_sessions_codex_data_version
    ON sessions (data_version) WHERE agent = 'codex'`); err != nil {
		return fmt.Errorf("indexing PG Codex payload versions: %w", err)
	}
	hasVectorDocuments, err := codexPGHasTable(ctx, pg, "vector_documents")
	if err != nil {
		return err
	}
	guardMode := codexPayloadGuardModeTriggers
	if err := installCodexPayloadWriteGuardsPG(
		ctx, pg, true, hasVectorDocuments,
	); err != nil {
		switch {
		case isDuplicateObject(err):
			// A concurrent migration created a trigger between this
			// migration's DROP and CREATE. Confirm it finished the full set;
			// if not, fail and let the next schema check reinstall.
			installed, checkErr := codexPayloadWriteGuardsInstalledPG(ctx, pg)
			if checkErr != nil {
				return checkErr
			}
			if !installed {
				return err
			}
		case isFeatureUnimplemented(err) || isTriggerTypeUnsupported(err):
			guardMode = codexPayloadGuardModeScan
			log.Printf(
				"pg schema: CockroachDB server does not support the Codex"+
					" payload write-guard triggers (%v); relying on"+
					" scan-based read gating instead", err,
			)
		default:
			return err
		}
	}
	return withCodexRepairTxPG(ctx, pg, true, func(tx *sql.Tx) error {
		if err := repairCodexEncryptedPayloadsPGTx(ctx, tx); err != nil {
			return err
		}
		return setCodexPayloadGuardModePG(ctx, tx, guardMode)
	})
}

// installCodexPayloadWriteGuardsPG issues the guard trigger DDL. On
// PostgreSQL it runs inside the locked migration transaction; on CockroachDB
// each statement autocommits (see installCodexPayloadCompatibilityCRDB).
func installCodexPayloadWriteGuardsPG(
	ctx context.Context, q codexPGExecer, crdb, hasVectorDocuments bool,
) error {
	for _, stmt := range codexPayloadGuardDDL(crdb, hasVectorDocuments) {
		if _, err := q.ExecContext(ctx, stmt.sql); err != nil {
			return fmt.Errorf("%s: %w", stmt.action, err)
		}
	}
	return nil
}

type codexGuardStatement struct {
	action string
	sql    string
}

func codexCollabToolHeaderGuardSQL(contentExpr string) string {
	patterns := parser.CodexCollabToolHeaderLikePatterns()
	clauses := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.ReplaceAll(pattern, "'", "''")
		clauses = append(clauses,
			fmt.Sprintf("%s LIKE '%%%s%%'", contentExpr, pattern))
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

// codexPayloadGuardDDL returns the write-guard DDL shared by PostgreSQL and
// CockroachDB. The trigger function bodies use the (NEW).column composite
// syntax, which both servers accept — CockroachDB rejects the bare
// NEW.column form inside subqueries (crdb#114687). UPDATE column lists
// narrow when the PostgreSQL triggers fire; CockroachDB does not support
// them yet (crdb#135656), so its triggers fire on every update and the guard
// functions re-check the relevant columns.
func codexPayloadGuardDDL(
	crdb, hasVectorDocuments bool,
) []codexGuardStatement {
	sessionUpdate := "UPDATE OF agent, data_version, relationship_type, first_message"
	messageUpdate := "UPDATE OF session_id, ordinal, content, role, has_tool_use"
	toolUpdate := "UPDATE OF session_id, message_ordinal, input_json, tool_name"
	sessionUnchangedGuard := ""
	if crdb {
		sessionUpdate, messageUpdate, toolUpdate = "UPDATE", "UPDATE", "UPDATE"
		// CockroachDB cannot narrow a trigger to an UPDATE column list, so
		// curation-only writes such as trash and restore still invoke this
		// function. Leave an existing withheld payload gated for reads, but do
		// not reject a write that did not touch any session payload evidence.
		sessionUnchangedGuard = `
    IF TG_OP = 'UPDATE'
       AND (NEW).agent IS NOT DISTINCT FROM (OLD).agent
       AND (NEW).data_version IS NOT DISTINCT FROM (OLD).data_version
       AND (NEW).relationship_type IS NOT DISTINCT FROM (OLD).relationship_type
       AND (NEW).first_message IS NOT DISTINCT FROM (OLD).first_message THEN
        RETURN NEW;
    END IF;`
	}
	stmts := []codexGuardStatement{
		{"creating PG Codex session write guard", fmt.Sprintf(`
CREATE OR REPLACE FUNCTION agentsview_guard_codex_payload_session()
RETURNS trigger LANGUAGE plpgsql AS $function$
BEGIN%s
    IF (NEW).agent = 'codex'
       AND (NEW).data_version < %d
       AND current_setting('%s', true) IS DISTINCT FROM '%d'
       AND COALESCE((NEW).first_message, '') LIKE '%%gAAAAA%%' THEN
		RAISE EXCEPTION 'legacy Codex preview contains an encrypted payload'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$function$`, sessionUnchangedGuard, codexEncryptedPayloadDataVersion,
			codexPayloadRepairSetting,
			codexEncryptedPayloadDataVersion)},
		{"installing PG Codex session write guard", fmt.Sprintf(`
DROP TRIGGER IF EXISTS %s ON sessions;
DROP TRIGGER IF EXISTS %s ON sessions;
DROP TRIGGER IF EXISTS %s ON sessions;
DROP TRIGGER IF EXISTS %s ON sessions;
DROP TRIGGER IF EXISTS %s ON sessions;
DROP TRIGGER IF EXISTS %s ON sessions;
DROP TRIGGER IF EXISTS %s ON sessions;
CREATE TRIGGER %s
BEFORE INSERT OR %s ON sessions
FOR EACH ROW EXECUTE FUNCTION agentsview_guard_codex_payload_session()`,
			legacyCodexSessionWriteGuard, previousCodexSessionWriteGuardV2,
			previousCodexSessionWriteGuardV3,
			previousCodexSessionWriteGuardV4,
			previousCodexSessionWriteGuardV5,
			previousCodexSessionWriteGuardV6,
			codexSessionWriteGuardTrigger, codexSessionWriteGuardTrigger,
			sessionUpdate)},
		{"creating PG Codex message write guard", fmt.Sprintf(`
CREATE OR REPLACE FUNCTION agentsview_guard_codex_payload_message()
RETURNS trigger LANGUAGE plpgsql AS $function$
BEGIN
    IF (NEW).content LIKE '%%gAAAAA%%'
       AND current_setting('%s', true) IS DISTINCT FROM '%d'
       AND EXISTS (
           SELECT 1 FROM sessions s
            WHERE s.id = (NEW).session_id
              AND s.agent = 'codex'
              AND s.data_version < %d
			  AND ((NEW).role = 'user'
			    OR (NEW).has_tool_use
			    OR EXISTS (
			        SELECT 1 FROM tool_calls tc
			         WHERE tc.session_id = (NEW).session_id
			           AND tc.message_ordinal = (NEW).ordinal
			           AND tc.tool_name IN %s
			    )
			    OR %s
			  )
       ) THEN
        RAISE EXCEPTION 'legacy Codex message contains an encrypted payload'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$function$`, codexPayloadRepairSetting, codexEncryptedPayloadDataVersion,
			codexEncryptedPayloadDataVersion, parser.CodexCollabToolsSQL(),
			codexCollabToolHeaderGuardSQL("(NEW).content"))},
		{"installing PG Codex message write guard", fmt.Sprintf(`
DROP TRIGGER IF EXISTS %s ON messages;
DROP TRIGGER IF EXISTS %s ON messages;
DROP TRIGGER IF EXISTS %s ON messages;
DROP TRIGGER IF EXISTS %s ON messages;
DROP TRIGGER IF EXISTS %s ON messages;
CREATE TRIGGER %s
BEFORE INSERT OR %s ON messages
FOR EACH ROW EXECUTE FUNCTION agentsview_guard_codex_payload_message()`,
			legacyCodexMessageWriteGuard, previousCodexMessageWriteGuardV2,
			previousCodexMessageWriteGuardV3,
			previousCodexMessageWriteGuardV4,
			codexMessageWriteGuardTrigger, codexMessageWriteGuardTrigger,
			messageUpdate)},
		{"creating PG Codex tool write guard", fmt.Sprintf(`
CREATE OR REPLACE FUNCTION agentsview_guard_codex_payload_tool()
RETURNS trigger LANGUAGE plpgsql AS $function$
BEGIN
    IF (NEW).tool_name IN %s
       AND current_setting('%s', true) IS DISTINCT FROM '%d'
       AND EXISTS (
           SELECT 1 FROM sessions s
            WHERE s.id = (NEW).session_id
              AND s.agent = 'codex'
              AND s.data_version < %d
       )
	   AND (COALESCE((NEW).input_json, '') LIKE '%%gAAAAA%%'
	     OR EXISTS (
	         SELECT 1 FROM messages m
	          WHERE m.session_id = (NEW).session_id
	            AND m.ordinal = (NEW).message_ordinal
	            AND m.content LIKE '%%gAAAAA%%'
	     )) THEN
        RAISE EXCEPTION 'legacy Codex collaboration tool contains an encrypted payload'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$function$`, parser.CodexCollabToolsSQL(), codexPayloadRepairSetting,
			codexEncryptedPayloadDataVersion, codexEncryptedPayloadDataVersion)},
		{"installing PG Codex tool write guard", fmt.Sprintf(`
DROP TRIGGER IF EXISTS %s ON tool_calls;
DROP TRIGGER IF EXISTS %s ON tool_calls;
DROP TRIGGER IF EXISTS %s ON tool_calls;
DROP TRIGGER IF EXISTS %s ON tool_calls;
CREATE TRIGGER %s
BEFORE INSERT OR %s ON tool_calls
FOR EACH ROW EXECUTE FUNCTION agentsview_guard_codex_payload_tool()`,
			legacyCodexToolWriteGuard, previousCodexToolWriteGuardV2,
			previousCodexToolWriteGuardV3,
			codexToolWriteGuardTrigger,
			codexToolWriteGuardTrigger, toolUpdate)},
	}
	if hasVectorDocuments {
		stmts = append(stmts,
			codexGuardStatement{
				"creating PG Codex vector write guard", fmt.Sprintf(`
CREATE OR REPLACE FUNCTION agentsview_guard_codex_payload_vector()
RETURNS trigger LANGUAGE plpgsql AS $function$
BEGIN
    IF (NEW).content LIKE '%%gAAAAA%%'
       AND EXISTS (
        SELECT 1 FROM sessions
         WHERE id = (NEW).session_id AND agent = 'codex'
    ) AND current_setting('agentsview.codex_payload_data_version', true)
            IS DISTINCT FROM '%d' THEN
        RAISE EXCEPTION 'Codex vector payload writer is not compatibility-marked'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$function$`, codexEncryptedPayloadDataVersion)},
			codexGuardStatement{
				"installing PG Codex vector write guard", fmt.Sprintf(`
DROP TRIGGER IF EXISTS %s ON vector_documents;
DROP TRIGGER IF EXISTS %s ON vector_documents;
DROP TRIGGER IF EXISTS %s ON vector_documents;
DROP TRIGGER IF EXISTS %s ON vector_documents;
CREATE TRIGGER %s
BEFORE INSERT OR UPDATE ON vector_documents
FOR EACH ROW EXECUTE FUNCTION agentsview_guard_codex_payload_vector()`,
					legacyCodexVectorWriteGuard, previousCodexVectorWriteGuardV2,
					previousCodexVectorWriteGuardV3,
					codexVectorWriteGuardTrigger,
					codexVectorWriteGuardTrigger)},
		)
	}
	return stmts
}

func setCodexPayloadGuardModePG(
	ctx context.Context, q codexPGExecer, mode string,
) error {
	if _, err := q.ExecContext(ctx, `
INSERT INTO sync_metadata (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		codexPayloadGuardModeMetadata, mode,
	); err != nil {
		return fmt.Errorf("storing PG Codex payload guard mode: %w", err)
	}
	return nil
}

func codexPayloadGuardModePG(
	ctx context.Context, q codexPGRowQueryer,
) (string, error) {
	var mode string
	err := q.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		codexPayloadGuardModeMetadata,
	).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading PG Codex payload guard mode: %w", err)
	}
	return mode, nil
}

// codexPayloadWriteGuardsInstalledPG checks trigger names and requires every
// guard to fire for ordinary writes. Disabled and replica-only triggers do not
// protect normal writer sessions, so they must fall through to reinstallation
// or make read-only compatibility checks fail closed. The persisted trigger
// bodies are otherwise frozen at first install: any change to the guard
// predicates — the collab tool list, the watermark, or the guarded columns —
// must also rename the codex*WriteGuardTrigger constants so existing schemas
// fall through to a fresh install.
func codexPayloadWriteGuardsInstalledPG(
	ctx context.Context, q codexPGRowQueryer,
) (bool, error) {
	var count int
	if err := q.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT t.tgname)
  FROM pg_trigger t
  JOIN pg_class c ON c.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = current_schema()
	   AND NOT t.tgisinternal
	   AND t.tgenabled IN ('O', 'A')
	   AND ((c.relname = 'sessions' AND t.tgname = $1)
	     OR (c.relname = 'messages' AND t.tgname = $2)
	     OR (c.relname = 'tool_calls' AND t.tgname = $3)
	     OR (c.relname = 'vector_documents' AND t.tgname = $4))`,
		codexSessionWriteGuardTrigger, codexMessageWriteGuardTrigger,
		codexToolWriteGuardTrigger, codexVectorWriteGuardTrigger,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("checking PG Codex payload write guards: %w", err)
	}
	hasVectorDocuments, err := codexPGHasTable(ctx, q, "vector_documents")
	if err != nil {
		return false, err
	}
	required := 3
	if hasVectorDocuments {
		required++
	}
	return count >= required, nil
}

func invalidateCodexDerivedDataPG(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
) error {
	for _, sessionID := range sessionIDs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM secret_findings WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return fmt.Errorf(
				"deleting PG Codex secret findings for %s: %w",
				sessionID, err,
			)
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
       transcript_revision = CAST(CAST(transcript_revision AS BIGINT) + 1 AS TEXT),
       updated_at = NOW()
 WHERE id = $1`, sessionID); err != nil {
			return fmt.Errorf(
				"invalidating PG Codex derived data for %s: %w",
				sessionID, err,
			)
		}
	}
	return nil
}

func pgTxHasTable(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var present bool
	if err := tx.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, name,
	).Scan(&present); err != nil {
		return false, fmt.Errorf("probing PG table %s: %w", name, err)
	}
	return present, nil
}

func invalidateCodexVectorsPG(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	allCodex bool,
) error {
	sessionPredicate := `SELECT id FROM sessions WHERE agent = 'codex'`
	var queryArgs []any
	if !allCodex {
		if len(sessionIDs) == 0 {
			return nil
		}
		// An array parameter rather than a temp table: CockroachDB disables
		// temporary tables by default and never supports ON COMMIT DROP
		// (crdb#46556), and the array keeps the statement shape identical on
		// both backends.
		sessionPredicate = `SELECT unnest($1::text[])`
		queryArgs = []any{sessionIDs}
	}

	hasDocs, err := pgTxHasTable(ctx, tx, "vector_documents")
	if err != nil || !hasDocs {
		return err
	}
	hasGenerations, err := pgTxHasTable(ctx, tx, "vector_generations")
	if err != nil {
		return err
	}
	if hasGenerations {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM vector_generations`)
		if err != nil {
			return fmt.Errorf("querying PG vector generations for Codex repair: %w", err)
		}
		var generationIDs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scanning PG vector generation for Codex repair: %w", err)
			}
			generationIDs = append(generationIDs, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterating PG vector generations for Codex repair: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("closing PG vector generations for Codex repair: %w", err)
		}
		for _, id := range generationIDs {
			table := vectorChunkTable(id)
			hasChunks, err := pgTxHasTable(ctx, tx, table)
			if err != nil {
				return err
			}
			if !hasChunks {
				continue
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s
 WHERE doc_key IN (
       SELECT d.doc_key FROM vector_documents d
        WHERE d.session_id IN (`+sessionPredicate+`)
	 )`, table), queryArgs...); err != nil {
				return fmt.Errorf("deleting stale Codex chunks from %s: %w", table, err)
			}
		}
	}
	hasPushState, err := pgTxHasTable(ctx, tx, "vector_push_state")
	if err != nil {
		return err
	}
	if hasPushState {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM vector_push_state
 WHERE session_id IN (`+sessionPredicate+`)`, queryArgs...); err != nil {
			return fmt.Errorf("deleting stale Codex vector push state: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM vector_documents
 WHERE session_id IN (`+sessionPredicate+`)`, queryArgs...); err != nil {
		return fmt.Errorf("deleting stale Codex vector documents: %w", err)
	}
	return nil
}

func codexVectorsExistPG(ctx context.Context, q codexPGReadQueryer) (bool, error) {
	hasDocs, err := codexPGHasTable(ctx, q, "vector_documents")
	if err != nil {
		return false, err
	}
	if !hasDocs {
		return false, nil
	}
	var present bool
	if err := q.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM vector_documents d
    JOIN sessions s ON s.id = d.session_id
    WHERE s.agent = 'codex'
)`).Scan(&present); err != nil {
		return false, fmt.Errorf("checking PG Codex vector compatibility: %w", err)
	}
	return present, nil
}

// staleCodexVectorSessionIDsPG distinguishes freshly rebuilt vectors from
// stale vectors by examining only document content
// derived from parser-owned subagent user turns or collab tool calls. This
// scan still runs after the completion marker exists because an older client
// can push stale vector documents independently of the session data version.
// Root user text and unrelated tool content are deliberately outside scope.
func staleCodexVectorSessionIDsPG(
	ctx context.Context, q codexPGReadQueryer,
) ([]string, error) {
	hasDocs, err := codexPGHasTable(ctx, q, "vector_documents")
	if err != nil {
		return nil, err
	}
	if !hasDocs {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
SELECT d.session_id, d.content, d.offsets, d.ordinal,
       COALESCE((
           SELECT json_agg(DISTINCT m.ordinal ORDER BY m.ordinal)
             FROM messages m
            WHERE m.session_id = d.session_id
              AND m.ordinal BETWEEN d.ordinal AND d.ordinal_end
              AND m.role = 'user'
              AND s.relationship_type = 'subagent'
       ), '[]'::json) AS subagent_user_ordinals,
       COALESCE((
           SELECT json_agg(DISTINCT tc.message_ordinal ORDER BY tc.message_ordinal)
             FROM tool_calls tc
            WHERE tc.session_id = d.session_id
              AND tc.message_ordinal BETWEEN d.ordinal AND d.ordinal_end
              AND tc.tool_name IN `+parser.CodexCollabToolsSQL()+`
       ), '[]'::json) AS collab_tool_ordinals
  FROM vector_documents d
  JOIN sessions s ON s.id = d.session_id
 WHERE s.agent = 'codex'
   AND d.content LIKE '%gAAAAA%'`)
	if err != nil {
		return nil, fmt.Errorf("querying PG Codex vector payload compatibility: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]struct{})
	var sessionIDs []string
	for rows.Next() {
		var sessionID, content, rawOffsets string
		var rawSubagentOrdinals, rawCollabOrdinals string
		var docOrdinal int
		if err := rows.Scan(
			&sessionID, &content, &rawOffsets, &docOrdinal,
			&rawSubagentOrdinals, &rawCollabOrdinals,
		); err != nil {
			return nil, fmt.Errorf("scanning PG Codex vector payload compatibility: %w", err)
		}
		stale, err := staleCodexVectorPayloadSegments(
			content, rawOffsets, docOrdinal, rawSubagentOrdinals,
			parser.RedactCodexStoredSubagentMessage, "subagent user",
		)
		if err != nil {
			return nil, err
		}
		if !stale {
			stale, err = staleCodexCollabVectorPayload(
				content, rawOffsets, docOrdinal, rawCollabOrdinals,
			)
			if err != nil {
				return nil, err
			}
		}
		if !stale {
			continue
		}
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating PG Codex vector payload compatibility: %w", err)
	}
	return sessionIDs, nil
}

func staleCodexCollabVectorPayload(
	content, rawOffsets string, docOrdinal int, rawCollabOrdinals string,
) (bool, error) {
	return staleCodexVectorPayloadSegments(
		content, rawOffsets, docOrdinal, rawCollabOrdinals,
		parser.RedactCodexStoredToolContent, "collab",
	)
}

func staleCodexVectorPayloadSegments(
	content, rawOffsets string,
	docOrdinal int,
	rawOrdinals string,
	redact func(string) string,
	kind string,
) (bool, error) {
	var ordinals []int
	if err := json.Unmarshal([]byte(rawOrdinals), &ordinals); err != nil {
		return false, fmt.Errorf(
			"parsing PG Codex vector %s ordinals: %w", kind, err,
		)
	}
	if len(ordinals) == 0 {
		return false, nil
	}
	targets := make(map[int]struct{}, len(ordinals))
	for _, ordinal := range ordinals {
		targets[ordinal] = struct{}{}
	}

	var offsets []db.UnitOffset
	if err := json.Unmarshal([]byte(rawOffsets), &offsets); err != nil {
		return false, fmt.Errorf(
			"parsing PG Codex vector %s offsets: %w", kind, err,
		)
	}
	if len(offsets) == 0 {
		if _, ok := targets[docOrdinal]; !ok {
			return false, nil
		}
		return redact(content) != content, nil
	}

	for i, offset := range offsets {
		if _, ok := targets[offset.Ordinal]; !ok {
			continue
		}
		start := min(max(offset.ByteStart, 0), len(content))
		end := len(content)
		if i+1 < len(offsets) {
			end = min(max(offsets[i+1].ByteStart-2, start), len(content))
		}
		if redact(content[start:end]) != content[start:end] {
			return true, nil
		}
	}
	return false, nil
}

type codexPGRowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type codexPGReadQueryer interface {
	codexPGQueryer
	codexPGRowQueryer
}

type codexPGExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func codexPGHasTable(
	ctx context.Context, q codexPGRowQueryer, name string,
) (bool, error) {
	var present bool
	if err := q.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
     WHERE table_schema = current_schema() AND table_name = $1
)`, name).Scan(&present); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("probing PG table %s: %w", name, err)
	}
	return present, nil
}

// codexScanModeGatingAllowedPG reports whether a schema without installed
// write guards may fall back to scan-based read gating instead of failing
// closed outright. That is only ever true on CockroachDB: servers without
// trigger support record scan mode during the migration, and a role that
// cannot read sync_metadata is still admitted to the scans when the server
// is CockroachDB. On PostgreSQL missing guards always fail closed.
func codexScanModeGatingAllowedPG(
	ctx context.Context, pg *sql.DB,
) (bool, error) {
	mode, err := codexPayloadGuardModePG(ctx, pg)
	if err != nil {
		if !isUndefinedTable(err) &&
			!isUndefinedColumn(err) &&
			!isInsufficientPrivilege(err) {
			return false, err
		}
		mode = ""
	}
	if mode == codexPayloadGuardModeTriggers {
		// The migration recorded installed triggers, yet the guards are
		// gone: something removed them, so fail closed on any server.
		return false, nil
	}
	// Scan-mode metadata alone is not sufficient: a stale or copied row must
	// not weaken the fail-closed invariant on PostgreSQL, where triggers are
	// always available, so any non-trigger mode is admitted only when the
	// server itself is CockroachDB.
	return serverIsCockroachDBPG(ctx, pg)
}

func codexVectorRepairCompletePG(
	ctx context.Context, q codexPGRowQueryer,
) (bool, error) {
	var complete bool
	if err := q.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM sync_metadata WHERE key = $1
)`, codexVectorRepairCompletedMetadata).Scan(&complete); err != nil {
		return false, fmt.Errorf("checking PG Codex vector repair metadata: %w", err)
	}
	return complete, nil
}

func markCodexVectorRepairCompletePG(
	ctx context.Context, q codexPGExecer,
) error {
	if _, err := q.ExecContext(ctx, `
INSERT INTO sync_metadata (key, value) VALUES ($1, '1')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		codexVectorRepairCompletedMetadata,
	); err != nil {
		return fmt.Errorf("storing PG Codex vector repair metadata: %w", err)
	}
	return nil
}

// CheckCodexEncryptedPayloadCompat is the read-only counterpart of the PG
// repair. A writable EnsureSchema call repairs these rows first; a read-only
// operation must fail closed rather than expose ciphertext or stale embeddings
// and tells the operator how to apply the migration.
//
// On PostgreSQL, missing write guards always fail closed. CockroachDB
// servers without trigger support record scan mode during the migration and
// are gated by the ciphertext scans below instead. This fallback is suitable
// for repair verification, but direct readers must use one of the stricter
// reader gates below because their queries do not share this scan's snapshot.
func CheckCodexEncryptedPayloadCompat(ctx context.Context, pg *sql.DB) error {
	guarded, err := codexPayloadWriteGuardsInstalledPG(ctx, pg)
	if err != nil {
		return err
	}
	if !guarded {
		scanGated, err := codexScanModeGatingAllowedPG(ctx, pg)
		if err != nil {
			return err
		}
		if !scanGated {
			return fmt.Errorf(
				"%w: connect with a writable role using a current AgentsView build to install the shared-storage write guards",
				ErrCodexEncryptedPayloadRepairRequired,
			)
		}
	}
	legacyVersion, err := codexPGHasLegacyPayloadVersion(ctx, pg)
	if err != nil {
		return err
	}
	if legacyVersion {
		return fmt.Errorf(
			"%w: connect with a writable role using a current AgentsView build to certify legacy Codex sessions before retrying this PostgreSQL read",
			ErrCodexEncryptedPayloadRepairRequired,
		)
	}
	repairs, err := collectCodexPGRepairs(ctx, pg)
	if err != nil {
		return err
	}
	vectorRepairComplete, err := codexVectorRepairCompletePG(ctx, pg)
	metadataUnavailable := false
	if err != nil {
		// sync_metadata is push-only and is not part of the read-only
		// CheckSchemaCompat contract. A missing/legacy shape or a role that
		// cannot read it means the one-time vector sweep is unmarked, so
		// inspect the actual legacy rows below instead of rejecting an
		// otherwise clean read-only schema.
		if !isUndefinedTable(err) &&
			!isUndefinedColumn(err) &&
			!isInsufficientPrivilege(err) {
			return err
		}
		vectorRepairComplete = false
		metadataUnavailable = true
	}
	var vectors bool
	if metadataUnavailable || vectorRepairComplete {
		var staleVectorSessionIDs []string
		staleVectorSessionIDs, err = staleCodexVectorSessionIDsPG(ctx, pg)
		vectors = len(staleVectorSessionIDs) > 0
	} else {
		vectors, err = codexVectorsExistPG(ctx, pg)
	}
	if err != nil {
		return err
	}
	if !repairs.needed() && !vectors {
		return nil
	}
	return fmt.Errorf(
		"%w: connect with a writable role using a current AgentsView build, then run pg push or pg serve once before retrying this PostgreSQL read",
		ErrCodexEncryptedPayloadRepairRequired,
	)
}

// CheckCodexEncryptedPayloadBoundedReadCompat gates a bounded direct reader.
// Even a short-lived command cannot rely on triggerless scan mode: a legacy
// writer could commit ciphertext after the scan and before the command's
// service queries. Until those reads share the scan transaction, require
// durable write guards just like a persistent reader.
func CheckCodexEncryptedPayloadBoundedReadCompat(
	ctx context.Context, pg *sql.DB,
) error {
	return checkCodexEncryptedPayloadGuardedReadCompat(
		ctx, pg, "bounded shared-storage reads require",
	)
}

// CheckCodexEncryptedPayloadPersistentReadCompat gates a long-running reader.
// Persistent services require write guards even on CockroachDB: scan mode can
// prove only that the database is clean at one instant, while a triggerless
// legacy writer could insert ciphertext immediately after startup.
func CheckCodexEncryptedPayloadPersistentReadCompat(
	ctx context.Context, pg *sql.DB,
) error {
	return checkCodexEncryptedPayloadGuardedReadCompat(
		ctx, pg, "persistent shared-storage serving requires",
	)
}

func checkCodexEncryptedPayloadGuardedReadCompat(
	ctx context.Context, pg *sql.DB, requirement string,
) error {
	guarded, err := codexPayloadWriteGuardsInstalledPG(ctx, pg)
	if err != nil {
		return err
	}
	if !guarded {
		return fmt.Errorf(
			"%w: %s installed Codex payload write guards; use a trigger-capable PostgreSQL or CockroachDB server and run the migration with a writable current AgentsView build",
			ErrCodexEncryptedPayloadRepairRequired, requirement,
		)
	}
	return CheckCodexEncryptedPayloadCompat(ctx, pg)
}
