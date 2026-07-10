package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
)

type recallEvidenceValidationError struct {
	message string
}

func (e *recallEvidenceValidationError) Error() string {
	return e.message
}

func invalidRecallEvidencef(format string, args ...any) error {
	return &recallEvidenceValidationError{
		message: fmt.Sprintf(format, args...),
	}
}

func isRecallEvidenceValidationError(err error) bool {
	var validationErr *recallEvidenceValidationError
	return errors.As(err, &validationErr)
}

const (
	recallEvidenceWindowDigestVersion    = "recall-evidence-window/v1"
	recallEvidenceContentDigestVersion   = "recall-evidence-content/v1"
	recallEvidenceOrdinalBySourceUUIDSQL = `
		SELECT COUNT(*), MIN(ordinal)
		FROM messages
		WHERE session_id = ? AND source_uuid = ?
		  AND source_uuid != ''`
)

type recallEvidenceRevocationReason string

const (
	recallEvidenceRevocationStartEndpointUnresolved     recallEvidenceRevocationReason = "start_endpoint_unresolved"
	recallEvidenceRevocationEndEndpointUnresolved       recallEvidenceRevocationReason = "end_endpoint_unresolved"
	recallEvidenceRevocationInvalidResolvedRange        recallEvidenceRevocationReason = "invalid_resolved_range"
	recallEvidenceRevocationWindowInvalid               recallEvidenceRevocationReason = "window_invalid"
	recallEvidenceRevocationSelectionInvalid            recallEvidenceRevocationReason = "selection_invalid"
	recallEvidenceRevocationMissingDigest               recallEvidenceRevocationReason = "missing_digest"
	recallEvidenceRevocationContentDigestMismatch       recallEvidenceRevocationReason = "content_digest_mismatch"
	recallEvidenceRevocationEvidenceDroppedDuringResync recallEvidenceRevocationReason = "evidence_dropped_during_resync"
)

type recallEvidenceRevocationEvent struct {
	entryID   string
	sessionID string
	reason    recallEvidenceRevocationReason
}

// recallEvidenceRevocationEvents buffers diagnostics until the transaction
// owner confirms its outermost commit succeeded.
type recallEvidenceRevocationEvents []recallEvidenceRevocationEvent

func (events recallEvidenceRevocationEvents) flush() {
	for _, event := range events {
		log.Printf(
			"recall: revoked provenance entry=%s session=%s reason=%s",
			event.entryID,
			event.sessionID,
			event.reason,
		)
	}
}

// RecallEvidenceWindow is the host-authorized transcript region supplied to an
// extractor. Its authorization digest binds the source session, exact ordinal
// coordinates, stable message identities, visible content, and tool calls.
type RecallEvidenceWindow struct {
	SessionID           string                        `json:"session_id"`
	MessageStartOrdinal int                           `json:"message_start_ordinal"`
	MessageEndOrdinal   int                           `json:"message_end_ordinal"`
	Messages            []RecallEvidenceWindowMessage `json:"messages"`
	AllowedToolUseIDs   []string                      `json:"allowed_tool_use_ids"`
	AuthorizationDigest string                        `json:"authorization_digest"`
}

// RecallEvidenceWindowMessage is the model-visible portion of one stored
// message plus the host-owned coordinates used to authorize a later selection.
type RecallEvidenceWindowMessage struct {
	Ordinal    int                            `json:"ordinal"`
	Role       string                         `json:"role"`
	Content    string                         `json:"content"`
	SourceUUID string                         `json:"source_uuid,omitempty"`
	ToolCalls  []RecallEvidenceWindowToolCall `json:"tool_calls,omitempty"`
}

// RecallEvidenceWindowToolCall contains the stored tool-call fields supplied
// with an evidence window. Database row IDs and derived indexing fields are not
// part of the model-visible or fingerprinted representation.
type RecallEvidenceWindowToolCall struct {
	ToolName            string `json:"tool_name"`
	Category            string `json:"category"`
	ToolUseID           string `json:"tool_use_id,omitempty"`
	InputJSON           string `json:"input_json,omitempty"`
	SkillName           string `json:"skill_name,omitempty"`
	ResultContentLength int    `json:"result_content_length,omitempty"`
	ResultContent       string `json:"result_content,omitempty"`
	SubagentSessionID   string `json:"subagent_session_id,omitempty"`
}

// RecallEvidenceSelection is the only provenance choice an extractor may
// make: a narrowed ordinal range and optional tool calls inside the authorized
// host window. The source session cannot be replaced by model output.
type RecallEvidenceSelection struct {
	MessageStartOrdinal int      `json:"message_start_ordinal"`
	MessageEndOrdinal   int      `json:"message_end_ordinal"`
	ToolUseIDs          []string `json:"tool_use_ids,omitempty"`
}

// RecallEvidenceSelectionMetadata is derived exclusively from host state and
// persisted with durable evidence. Its content digest intentionally omits
// storage IDs, timestamps, absolute ordinals, and source UUIDs so unchanged
// evidence remains recognizable after transcript coordinates shift.
type RecallEvidenceSelectionMetadata struct {
	MessageStartSourceUUID string   `json:"message_start_source_uuid,omitempty"`
	MessageEndSourceUUID   string   `json:"message_end_source_uuid,omitempty"`
	ContentDigest          string   `json:"content_digest"`
	ToolUseIDs             []string `json:"tool_use_ids,omitempty"`
}

type recallEvidenceQueryer interface {
	QueryContext(
		context.Context, string, ...any,
	) (*sql.Rows, error)
}

// BuildRecallEvidenceWindow loads an exact, gap-free transcript range and
// fingerprints the representation the host may supply to an extractor.
func (db *DB) BuildRecallEvidenceWindow(
	ctx context.Context,
	sessionID string,
	messageStartOrdinal int,
	messageEndOrdinal int,
) (RecallEvidenceWindow, error) {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RecallEvidenceWindow{}, fmt.Errorf(
			"beginning recall evidence snapshot: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	window, err := buildRecallEvidenceWindow(
		ctx,
		tx,
		sessionID,
		messageStartOrdinal,
		messageEndOrdinal,
	)
	if err != nil {
		return RecallEvidenceWindow{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecallEvidenceWindow{}, fmt.Errorf(
			"committing recall evidence snapshot: %w", err,
		)
	}
	return window, nil
}

func buildRecallEvidenceWindow(
	ctx context.Context,
	queryer recallEvidenceQueryer,
	sessionID string,
	messageStartOrdinal int,
	messageEndOrdinal int,
) (RecallEvidenceWindow, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return RecallEvidenceWindow{}, invalidRecallEvidencef(
			"evidence window session id is required",
		)
	}
	if messageStartOrdinal < 0 || messageEndOrdinal < messageStartOrdinal {
		return RecallEvidenceWindow{}, invalidRecallEvidencef(
			"invalid evidence window range %d-%d",
			messageStartOrdinal,
			messageEndOrdinal,
		)
	}

	window := RecallEvidenceWindow{
		SessionID:           sessionID,
		MessageStartOrdinal: messageStartOrdinal,
		MessageEndOrdinal:   messageEndOrdinal,
	}
	rows, err := queryer.QueryContext(ctx, `
		SELECT ordinal, role, content, source_uuid
		FROM messages
		WHERE session_id = ?
		  AND ordinal BETWEEN ? AND ?
		ORDER BY ordinal ASC`,
		sessionID,
		messageStartOrdinal,
		messageEndOrdinal,
	)
	if err != nil {
		return RecallEvidenceWindow{}, fmt.Errorf(
			"querying recall evidence messages: %w",
			err,
		)
	}
	for rows.Next() {
		var message RecallEvidenceWindowMessage
		if err := rows.Scan(
			&message.Ordinal,
			&message.Role,
			&message.Content,
			&message.SourceUUID,
		); err != nil {
			rows.Close()
			return RecallEvidenceWindow{}, fmt.Errorf(
				"scanning recall evidence message: %w",
				err,
			)
		}
		window.Messages = append(window.Messages, message)
	}
	if err := rows.Close(); err != nil {
		return RecallEvidenceWindow{}, fmt.Errorf(
			"closing recall evidence messages: %w",
			err,
		)
	}
	if err := rows.Err(); err != nil {
		return RecallEvidenceWindow{}, fmt.Errorf(
			"reading recall evidence messages: %w",
			err,
		)
	}
	for offset := 0; offset <= messageEndOrdinal-messageStartOrdinal; offset++ {
		want := messageStartOrdinal + offset
		if offset >= len(window.Messages) || window.Messages[offset].Ordinal != want {
			return RecallEvidenceWindow{}, invalidRecallEvidencef(
				"recall evidence window %s:%d-%d is missing ordinal %d",
				sessionID,
				messageStartOrdinal,
				messageEndOrdinal,
				want,
			)
		}
	}

	messageIndexes := make(map[int]int, len(window.Messages))
	for i := range window.Messages {
		messageIndexes[window.Messages[i].Ordinal] = i
	}
	toolRows, err := queryer.QueryContext(ctx, `
		SELECT m.ordinal, tc.tool_name, tc.category,
		       COALESCE(tc.tool_use_id, ''),
		       COALESCE(tc.input_json, ''),
		       COALESCE(tc.skill_name, ''),
		       COALESCE(tc.result_content_length, 0),
		       COALESCE(tc.result_content, ''),
		       COALESCE(tc.subagent_session_id, '')
		FROM tool_calls tc
		JOIN messages m ON m.id = tc.message_id
		WHERE m.session_id = ?
		  AND m.ordinal BETWEEN ? AND ?
		ORDER BY m.ordinal ASC,
		         COALESCE(tc.call_index, 2147483647) ASC,
		         tc.id ASC`,
		sessionID,
		messageStartOrdinal,
		messageEndOrdinal,
	)
	if err != nil {
		return RecallEvidenceWindow{}, fmt.Errorf(
			"querying recall evidence tool calls: %w",
			err,
		)
	}
	allowedToolUseIDs := make(map[string]struct{})
	for toolRows.Next() {
		var ordinal int
		var toolCall RecallEvidenceWindowToolCall
		if err := toolRows.Scan(
			&ordinal,
			&toolCall.ToolName,
			&toolCall.Category,
			&toolCall.ToolUseID,
			&toolCall.InputJSON,
			&toolCall.SkillName,
			&toolCall.ResultContentLength,
			&toolCall.ResultContent,
			&toolCall.SubagentSessionID,
		); err != nil {
			toolRows.Close()
			return RecallEvidenceWindow{}, fmt.Errorf(
				"scanning recall evidence tool call: %w",
				err,
			)
		}
		messageIndex, ok := messageIndexes[ordinal]
		if !ok {
			toolRows.Close()
			return RecallEvidenceWindow{}, fmt.Errorf(
				"tool call references missing ordinal %d",
				ordinal,
			)
		}
		window.Messages[messageIndex].ToolCalls = append(
			window.Messages[messageIndex].ToolCalls,
			toolCall,
		)
		if toolCall.ToolUseID != "" {
			allowedToolUseIDs[toolCall.ToolUseID] = struct{}{}
		}
	}
	if err := toolRows.Close(); err != nil {
		return RecallEvidenceWindow{}, fmt.Errorf(
			"closing recall evidence tool calls: %w",
			err,
		)
	}
	if err := toolRows.Err(); err != nil {
		return RecallEvidenceWindow{}, fmt.Errorf(
			"reading recall evidence tool calls: %w",
			err,
		)
	}
	window.AllowedToolUseIDs = make(
		[]string,
		0,
		len(allowedToolUseIDs),
	)
	for toolUseID := range allowedToolUseIDs {
		window.AllowedToolUseIDs = append(
			window.AllowedToolUseIDs,
			toolUseID,
		)
	}
	sort.Strings(window.AllowedToolUseIDs)
	digest, err := recallEvidenceAuthorizationDigest(window)
	if err != nil {
		return RecallEvidenceWindow{}, err
	}
	window.AuthorizationDigest = digest
	return window, nil
}

// BindSelection validates that an extractor selection is contained within the
// host authorization and returns durable metadata derived from that host-owned
// window. Duplicate tool IDs are normalized to a sorted unique list.
func (window RecallEvidenceWindow) BindSelection(
	selection RecallEvidenceSelection,
) (RecallEvidenceSelectionMetadata, error) {
	wantAuthorizationDigest, err := recallEvidenceAuthorizationDigest(window)
	if err != nil {
		return RecallEvidenceSelectionMetadata{}, err
	}
	if window.AuthorizationDigest == "" ||
		window.AuthorizationDigest != wantAuthorizationDigest {
		return RecallEvidenceSelectionMetadata{}, invalidRecallEvidencef(
			"recall evidence window authorization digest is invalid",
		)
	}
	if selection.MessageStartOrdinal < 0 ||
		selection.MessageEndOrdinal < selection.MessageStartOrdinal {
		return RecallEvidenceSelectionMetadata{}, invalidRecallEvidencef(
			"invalid evidence selection range %d-%d",
			selection.MessageStartOrdinal,
			selection.MessageEndOrdinal,
		)
	}
	if selection.MessageStartOrdinal < window.MessageStartOrdinal ||
		selection.MessageEndOrdinal > window.MessageEndOrdinal {
		return RecallEvidenceSelectionMetadata{}, invalidRecallEvidencef(
			"evidence selection %d-%d is outside authorized window %d-%d",
			selection.MessageStartOrdinal,
			selection.MessageEndOrdinal,
			window.MessageStartOrdinal,
			window.MessageEndOrdinal,
		)
	}

	toolUseIDs, err := normalizeRecallEvidenceToolUseIDs(selection.ToolUseIDs)
	if err != nil {
		return RecallEvidenceSelectionMetadata{}, err
	}
	allowedToolUseIDs := make(map[string]struct{}, len(window.AllowedToolUseIDs))
	for _, toolUseID := range window.AllowedToolUseIDs {
		allowedToolUseIDs[toolUseID] = struct{}{}
	}
	for _, toolUseID := range toolUseIDs {
		if _, ok := allowedToolUseIDs[toolUseID]; !ok {
			return RecallEvidenceSelectionMetadata{}, invalidRecallEvidencef(
				"tool use %s is outside authorized window",
				toolUseID,
			)
		}
	}

	requestedToolUseIDs := make(map[string]struct{}, len(toolUseIDs))
	for _, toolUseID := range toolUseIDs {
		requestedToolUseIDs[toolUseID] = struct{}{}
	}
	seenToolUseIDs := make(map[string]struct{}, len(toolUseIDs))
	selectedMessages := make([]canonicalRecallEvidenceMessage, 0)
	var startSourceUUID string
	var endSourceUUID string
	for _, message := range window.Messages {
		if message.Ordinal < selection.MessageStartOrdinal ||
			message.Ordinal > selection.MessageEndOrdinal {
			continue
		}
		if message.Ordinal == selection.MessageStartOrdinal {
			startSourceUUID = message.SourceUUID
		}
		if message.Ordinal == selection.MessageEndOrdinal {
			endSourceUUID = message.SourceUUID
		}
		canonical := canonicalRecallEvidenceMessage{
			Role:    message.Role,
			Content: message.Content,
		}
		for _, toolCall := range message.ToolCalls {
			if _, ok := requestedToolUseIDs[toolCall.ToolUseID]; !ok {
				continue
			}
			canonical.ToolCalls = append(canonical.ToolCalls, toolCall)
			seenToolUseIDs[toolCall.ToolUseID] = struct{}{}
		}
		selectedMessages = append(selectedMessages, canonical)
	}
	wantMessageCount := selection.MessageEndOrdinal -
		selection.MessageStartOrdinal + 1
	if len(selectedMessages) != wantMessageCount {
		return RecallEvidenceSelectionMetadata{}, invalidRecallEvidencef(
			"evidence selection %d-%d contains a missing ordinal",
			selection.MessageStartOrdinal,
			selection.MessageEndOrdinal,
		)
	}
	for _, toolUseID := range toolUseIDs {
		if _, ok := seenToolUseIDs[toolUseID]; !ok {
			return RecallEvidenceSelectionMetadata{}, invalidRecallEvidencef(
				"tool use %s is outside selected messages",
				toolUseID,
			)
		}
	}
	contentDigest, err := digestRecallEvidenceCanonical(
		canonicalRecallEvidenceSelection{
			Version:  recallEvidenceContentDigestVersion,
			Messages: selectedMessages,
		},
	)
	if err != nil {
		return RecallEvidenceSelectionMetadata{}, err
	}
	return RecallEvidenceSelectionMetadata{
		MessageStartSourceUUID: startSourceUUID,
		MessageEndSourceUUID:   endSourceUUID,
		ContentDigest:          contentDigest,
		ToolUseIDs:             toolUseIDs,
	}, nil
}

type canonicalRecallEvidenceWindow struct {
	Version             string                        `json:"version"`
	SessionID           string                        `json:"session_id"`
	MessageStartOrdinal int                           `json:"message_start_ordinal"`
	MessageEndOrdinal   int                           `json:"message_end_ordinal"`
	Messages            []RecallEvidenceWindowMessage `json:"messages"`
	AllowedToolUseIDs   []string                      `json:"allowed_tool_use_ids"`
}

type canonicalRecallEvidenceSelection struct {
	Version  string                           `json:"version"`
	Messages []canonicalRecallEvidenceMessage `json:"messages"`
}

type canonicalRecallEvidenceMessage struct {
	Role      string                         `json:"role"`
	Content   string                         `json:"content"`
	ToolCalls []RecallEvidenceWindowToolCall `json:"tool_calls,omitempty"`
}

func recallEvidenceAuthorizationDigest(
	window RecallEvidenceWindow,
) (string, error) {
	return digestRecallEvidenceCanonical(canonicalRecallEvidenceWindow{
		Version:             recallEvidenceWindowDigestVersion,
		SessionID:           window.SessionID,
		MessageStartOrdinal: window.MessageStartOrdinal,
		MessageEndOrdinal:   window.MessageEndOrdinal,
		Messages:            window.Messages,
		AllowedToolUseIDs:   window.AllowedToolUseIDs,
	})
}

func digestRecallEvidenceCanonical(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding canonical recall evidence: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func normalizeRecallEvidenceToolUseIDs(values []string) ([]string, error) {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, invalidRecallEvidencef(
				"evidence selection contains an empty tool use id",
			)
		}
		unique[value] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

type recallEvidenceGroupKey struct {
	entryID              string
	messageStartOrdinal  int
	messageEndOrdinal    int
	messageStartSourceID string
	messageEndSourceID   string
	contentDigest        string
}

type recallEvidenceGroup struct {
	key         recallEvidenceGroupKey
	evidenceIDs []int64
	toolUseIDs  []string
}

// reconcileRecallEvidenceForSessionTx verifies every currently trusted
// evidence selection for sessionID against messages and tool calls visible in
// tx. Invalid selections revoke the parent entry without deleting historical
// evidence. Structural database failures are returned so the caller's
// transcript mutation rolls back atomically.
func reconcileRecallEvidenceForSessionTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	pending *recallEvidenceRevocationEvents,
) error {
	groups, err := loadTrustedRecallEvidenceGroupsTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	for _, group := range groups {
		startOrdinal := group.key.messageStartOrdinal
		endOrdinal := group.key.messageEndOrdinal
		if group.key.messageStartSourceID != "" {
			startOrdinal, err = uniqueRecallEvidenceOrdinalTx(
				ctx,
				tx,
				sessionID,
				group.key.messageStartSourceID,
			)
			if err != nil {
				return err
			}
			if startOrdinal < 0 {
				if err := revokeRecallEvidenceEntryTx(
					ctx,
					tx,
					group.key.entryID,
					sessionID,
					recallEvidenceRevocationStartEndpointUnresolved,
					pending,
				); err != nil {
					return err
				}
				continue
			}
		}
		if group.key.messageEndSourceID != "" {
			endOrdinal, err = uniqueRecallEvidenceOrdinalTx(
				ctx,
				tx,
				sessionID,
				group.key.messageEndSourceID,
			)
			if err != nil {
				return err
			}
			if endOrdinal < 0 {
				if err := revokeRecallEvidenceEntryTx(
					ctx,
					tx,
					group.key.entryID,
					sessionID,
					recallEvidenceRevocationEndEndpointUnresolved,
					pending,
				); err != nil {
					return err
				}
				continue
			}
		}
		if startOrdinal < 0 || endOrdinal < 0 || endOrdinal < startOrdinal {
			if err := revokeRecallEvidenceEntryTx(
				ctx,
				tx,
				group.key.entryID,
				sessionID,
				recallEvidenceRevocationInvalidResolvedRange,
				pending,
			); err != nil {
				return err
			}
			continue
		}

		window, err := buildRecallEvidenceWindow(
			ctx,
			tx,
			sessionID,
			startOrdinal,
			endOrdinal,
		)
		if err != nil {
			if isRecallEvidenceValidationError(err) {
				if err := revokeRecallEvidenceEntryTx(
					ctx,
					tx,
					group.key.entryID,
					sessionID,
					recallEvidenceRevocationWindowInvalid,
					pending,
				); err != nil {
					return err
				}
				continue
			}
			return err
		}
		metadata, err := window.BindSelection(RecallEvidenceSelection{
			MessageStartOrdinal: startOrdinal,
			MessageEndOrdinal:   endOrdinal,
			ToolUseIDs:          group.toolUseIDs,
		})
		if err != nil {
			if isRecallEvidenceValidationError(err) {
				if err := revokeRecallEvidenceEntryTx(
					ctx,
					tx,
					group.key.entryID,
					sessionID,
					recallEvidenceRevocationSelectionInvalid,
					pending,
				); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if group.key.contentDigest == "" {
			if err := revokeRecallEvidenceEntryTx(
				ctx,
				tx,
				group.key.entryID,
				sessionID,
				recallEvidenceRevocationMissingDigest,
				pending,
			); err != nil {
				return err
			}
			continue
		}
		if group.key.contentDigest != metadata.ContentDigest {
			if err := revokeRecallEvidenceEntryTx(
				ctx,
				tx,
				group.key.entryID,
				sessionID,
				recallEvidenceRevocationContentDigestMismatch,
				pending,
			); err != nil {
				return err
			}
			continue
		}
		for _, evidenceID := range group.evidenceIDs {
			if _, err := tx.ExecContext(ctx, `
				UPDATE recall_evidence
				SET message_start_ordinal = ?,
				    message_end_ordinal = ?,
				    message_start_source_uuid = ?,
				    message_end_source_uuid = ?,
				    content_digest = ?
				WHERE id = ?`,
				startOrdinal,
				endOrdinal,
				metadata.MessageStartSourceUUID,
				metadata.MessageEndSourceUUID,
				metadata.ContentDigest,
				evidenceID,
			); err != nil {
				return fmt.Errorf(
					"updating reconciled recall evidence %d: %w",
					evidenceID,
					err,
				)
			}
		}
	}
	return nil
}

func loadTrustedRecallEvidenceGroupsTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
) ([]recallEvidenceGroup, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.entry_id,
		       e.message_start_ordinal, e.message_end_ordinal,
		       e.message_start_source_uuid, e.message_end_source_uuid,
		       e.content_digest, e.tool_use_id
		FROM recall_evidence e
		JOIN recall_entries r ON r.id = e.entry_id
		WHERE e.session_id = ?
		  AND r.provenance_ok = 1
		ORDER BY e.entry_id ASC, e.id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying recall evidence to reconcile: %w", err)
	}
	defer rows.Close()
	groups := make([]recallEvidenceGroup, 0)
	indexes := make(map[recallEvidenceGroupKey]int)
	for rows.Next() {
		var evidenceID int64
		var key recallEvidenceGroupKey
		var toolUseID string
		if err := rows.Scan(
			&evidenceID,
			&key.entryID,
			&key.messageStartOrdinal,
			&key.messageEndOrdinal,
			&key.messageStartSourceID,
			&key.messageEndSourceID,
			&key.contentDigest,
			&toolUseID,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning recall evidence to reconcile: %w",
				err,
			)
		}
		index, ok := indexes[key]
		if !ok {
			index = len(groups)
			indexes[key] = index
			groups = append(groups, recallEvidenceGroup{key: key})
		}
		groups[index].evidenceIDs = append(
			groups[index].evidenceIDs,
			evidenceID,
		)
		if toolUseID != "" {
			groups[index].toolUseIDs = append(
				groups[index].toolUseIDs,
				toolUseID,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading recall evidence to reconcile: %w", err)
	}
	return groups, nil
}

func uniqueRecallEvidenceOrdinalTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	sourceUUID string,
) (int, error) {
	var count int
	var ordinal sql.NullInt64
	if err := tx.QueryRowContext(ctx, recallEvidenceOrdinalBySourceUUIDSQL,
		sessionID,
		sourceUUID,
	).Scan(&count, &ordinal); err != nil {
		return -1, fmt.Errorf(
			"resolving recall evidence source UUID %s: %w",
			sourceUUID,
			err,
		)
	}
	if count != 1 || !ordinal.Valid {
		return -1, nil
	}
	return int(ordinal.Int64), nil
}

func revokeRecallEvidenceEntryTx(
	ctx context.Context,
	tx *sql.Tx,
	entryID string,
	sessionID string,
	reason recallEvidenceRevocationReason,
	pending *recallEvidenceRevocationEvents,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE recall_entries
		SET provenance_ok = 0,
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ? AND provenance_ok = 1`,
		entryID,
	)
	if err != nil {
		return fmt.Errorf(
			"revoking recall evidence provenance for %s: %w",
			entryID,
			err,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"checking recall evidence provenance revocation for %s: %w",
			entryID,
			err,
		)
	}
	if affected == 1 {
		*pending = append(*pending, recallEvidenceRevocationEvent{
			entryID:   entryID,
			sessionID: sessionID,
			reason:    reason,
		})
	}
	return nil
}

func reconcileAllRecallEvidenceTx(
	ctx context.Context,
	tx *sql.Tx,
	pending *recallEvidenceRevocationEvents,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT e.session_id
		FROM recall_evidence e
		JOIN recall_entries r ON r.id = e.entry_id
		WHERE r.provenance_ok = 1
		ORDER BY e.session_id ASC`)
	if err != nil {
		return fmt.Errorf("querying recall evidence sessions: %w", err)
	}
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			rows.Close()
			return fmt.Errorf("scanning recall evidence session: %w", err)
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing recall evidence sessions: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading recall evidence sessions: %w", err)
	}
	for _, sessionID := range sessionIDs {
		if err := reconcileRecallEvidenceForSessionTx(
			ctx,
			tx,
			sessionID,
			pending,
		); err != nil {
			return err
		}
	}
	return nil
}
