package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"strconv"
)

// StagedToolResults is the publish-side handle for tool-result rows staged
// during a streaming parse. The staged write inserts event rows straight
// from the handle and resolves per-call result summaries with transient
// memory, so neither the event contents nor the summaries ever live as one
// full in-memory session slice.
type StagedToolResults interface {
	// ResolveSummary returns the stored result summary and its length for
	// one tool call, mirroring SummarizeToolResultEvents over the staged
	// events.
	ResolveSummary(
		ctx context.Context, toolUseID string,
	) (string, int, error)
	// InsertEventsTx inserts the staged rows for the whole session into
	// tool_result_events within tx, keyed by tool_use_id through the
	// final message coordinates in positions. The scratch database is
	// already attached to the tx's connection by the caller.
	InsertEventsTx(
		ctx context.Context,
		tx *sql.Tx,
		sessionID string,
		positions map[string]StagedToolCallPosition,
	) error
	// Path returns the staging storage path for ATTACH-based publishing.
	Path() string
	// Close releases the staging storage.
	Close() error
}

// StagedSignalsFunc computes the final signal update and secret findings
// for a staged session once every per-call summary has been resolved in
// the publish transaction. verdicts carries the per-call content-failure
// verdicts the summary resolution recorded, so the returned values can be
// persisted atomically with the message, tool-call, and event rows instead
// of being recomputed after commit.
type StagedSignalsFunc func(
	verdicts map[string]bool,
) (SessionSignalUpdate, []SecretFinding, error)

// StagedToolCallPosition identifies one tool call's final coordinates.
type StagedToolCallPosition struct {
	ToolUseID string
	Ordinal   int
	CallIndex int
}

// StagedToolCallKey returns the internal staging key for one occurrence of a
// provider call ID. Provider call IDs are not guaranteed unique within a
// transcript, so staged rows use occurrence identity while the published
// tool_use_id remains unchanged.
func StagedToolCallKey(toolUseID string, occurrence int) string {
	// Use a printable, length-prefixed key. Embedded NUL bytes are legal in
	// Go strings but are not a safe SQLite TEXT identity across every
	// go-sqlite3 binding path: a bound value may be observed only up to the
	// first NUL on a later connection. The byte length keeps the encoding
	// unambiguous even when the provider call ID itself contains colons.
	return strconv.Itoa(len(toolUseID)) + ":" + toolUseID + ":" +
		strconv.Itoa(occurrence)
}

// stagedAttachName is the schema name the scratch database is attached
// under for the publish transaction.
const stagedAttachName = "codex_staging"

// ReplaceSessionContentStaged replaces a session's content from a
// streaming parse: messages and tool-call metadata arrive as the (small)
// in-memory slice whose result events carry placeholders, while the event
// rows and per-call summaries come from the staged handle. Blocked
// categories are blanked exactly like the legacy write path. This is the
// atomic publish of the staged cold-import path; it mirrors
// ReplaceSessionContent's transaction sequence with the event/summary
// inserts replaced by staged sources.
//
// The scratch database is attached to the writer connection before the
// transaction begins and detached after it settles, so consecutive staged
// publishes on the same single-connection writer pool never collide with
// a leftover attachment. signalsFn runs inside the transaction after all
// summaries are resolved, so the persisted signals and findings carry the
// real content-failure verdicts in the same atomic commit as the rows
// they describe.
func (db *DB) ReplaceSessionContentStaged(
	ctx context.Context,
	sessionID string, msgs []Message,
	staged StagedToolResults, blocked map[string]bool,
	signalsFn StagedSignalsFunc,
) error {
	return db.replaceSessionContentStaged(
		ctx, sessionID, msgs, staged, blocked, signalsFn, nil, nil,
	)
}

// ReplaceSessionContentStagedWithCheckpoint is the staged publish plus an
// in-transaction parser checkpoint upsert, so content and resume state
// commit atomically.
func (db *DB) ReplaceSessionContentStagedWithCheckpoint(
	ctx context.Context,
	sessionID string, msgs []Message,
	staged StagedToolResults, blocked map[string]bool,
	signalsFn StagedSignalsFunc,
	cp *ParserCheckpoint, blobs *ParserCheckpointBlobs,
) error {
	return db.replaceSessionContentStaged(
		ctx, sessionID, msgs, staged, blocked, signalsFn, cp, blobs,
	)
}

func writeStagedDigestString(h interface{ Write([]byte) (int, error) }, value string) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(value))
}

func writeStagedDigestInt(h interface{ Write([]byte) (int, error) }, value int64) {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], uint64(value))
	_, _ = h.Write(encoded[:])
}

// stagedSessionHasStoredMessagesTx reports whether sessionID already has any
// stored message rows. tool_calls and tool_result_events are always inserted
// alongside the message they belong to, so a session with no message rows
// can never have any content stagedSessionContentDigestTx would find either.
// A cold staged import can use this to skip both full-table digest scans
// entirely instead of proving byte-for-byte equality against nothing.
func stagedSessionHasStoredMessagesTx(
	tx *sql.Tx, sessionID string,
) (bool, error) {
	var exists int
	err := tx.QueryRow(
		`SELECT 1 FROM messages WHERE session_id = ? LIMIT 1`, sessionID,
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf(
			"checking existing staged content for %s: %w", sessionID, err,
		)
	}
	return true, nil
}

// stagedSessionContentDigestTx hashes every parser-owned transcript field while
// ignoring SQLite row IDs. It lets a forced staged verification prove that the
// normalized content is unchanged and avoid delete/reinsert revision churn.
func stagedSessionContentDigestTx(
	tx *sql.Tx, sessionID string,
) ([sha256.Size]byte, error) {
	h := sha256.New()
	rows, err := tx.Query(`
		SELECT ordinal, role, content, thinking_text, COALESCE(timestamp, ''),
		       has_thinking, has_tool_use, content_length, is_system, model,
		       token_usage, context_tokens, output_tokens,
		       has_context_tokens, has_output_tokens,
		       claude_message_id, claude_request_id, source_type,
		       source_subtype, prompt_source, source_uuid,
		       source_parent_uuid, is_sidechain, is_compact_boundary
		FROM messages WHERE session_id = ? ORDER BY ordinal`, sessionID)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	for rows.Next() {
		var ordinal, hasThinking, hasToolUse, contentLength, isSystem int64
		var contextTokens, outputTokens, hasContext, hasOutput int64
		var isSidechain, isCompact int64
		var role, content, thinking, timestamp, model, tokenUsage string
		var claudeMessageID, claudeRequestID, sourceType, sourceSubtype string
		var promptSource, sourceUUID, sourceParentUUID string
		if err := rows.Scan(
			&ordinal, &role, &content, &thinking, &timestamp,
			&hasThinking, &hasToolUse, &contentLength, &isSystem, &model,
			&tokenUsage, &contextTokens, &outputTokens, &hasContext, &hasOutput,
			&claudeMessageID, &claudeRequestID, &sourceType, &sourceSubtype,
			&promptSource, &sourceUUID, &sourceParentUUID, &isSidechain, &isCompact,
		); err != nil {
			rows.Close()
			return [sha256.Size]byte{}, err
		}
		writeStagedDigestString(h, "message")
		for _, value := range []int64{
			ordinal, hasThinking, hasToolUse, contentLength, isSystem,
			contextTokens, outputTokens, hasContext, hasOutput,
			isSidechain, isCompact,
		} {
			writeStagedDigestInt(h, value)
		}
		for _, value := range []string{
			role, content, thinking, timestamp, model, tokenUsage,
			claudeMessageID, claudeRequestID, sourceType, sourceSubtype,
			promptSource, sourceUUID, sourceParentUUID,
		} {
			writeStagedDigestString(h, value)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return [sha256.Size]byte{}, err
	}
	if err := rows.Close(); err != nil {
		return [sha256.Size]byte{}, err
	}

	rows, err = tx.Query(`
		SELECT m.ordinal, COALESCE(tc.call_index, 0), tc.tool_name, tc.category,
		       COALESCE(tc.tool_use_id, ''), COALESCE(tc.input_json, ''),
		       COALESCE(tc.skill_name, ''),
		       COALESCE(tc.result_content_length, 0),
		       COALESCE(tc.result_content, ''),
		       COALESCE(tc.subagent_session_id, ''), COALESCE(tc.file_path, '')
		FROM tool_calls tc JOIN messages m ON m.id = tc.message_id
		WHERE tc.session_id = ?
		ORDER BY m.ordinal, COALESCE(tc.call_index, 0), tc.id`, sessionID)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	for rows.Next() {
		var ordinal, callIndex, resultLength int64
		var toolName, category, toolUseID, inputJSON, skillName string
		var resultContent, subagentSessionID, filePath string
		if err := rows.Scan(
			&ordinal, &callIndex, &toolName, &category, &toolUseID,
			&inputJSON, &skillName, &resultLength, &resultContent,
			&subagentSessionID, &filePath,
		); err != nil {
			rows.Close()
			return [sha256.Size]byte{}, err
		}
		writeStagedDigestString(h, "tool_call")
		writeStagedDigestInt(h, ordinal)
		writeStagedDigestInt(h, callIndex)
		writeStagedDigestInt(h, resultLength)
		for _, value := range []string{
			toolName, category, toolUseID, inputJSON, skillName,
			resultContent, subagentSessionID, filePath,
		} {
			writeStagedDigestString(h, value)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return [sha256.Size]byte{}, err
	}
	if err := rows.Close(); err != nil {
		return [sha256.Size]byte{}, err
	}

	rows, err = tx.Query(`
		SELECT tool_call_message_ordinal, call_index,
		       COALESCE(tool_use_id, ''), COALESCE(agent_id, ''),
		       COALESCE(subagent_session_id, ''), source, status, content,
		       content_length, COALESCE(timestamp, ''), event_index
		FROM tool_result_events WHERE session_id = ?
		ORDER BY tool_call_message_ordinal, call_index, event_index, id`, sessionID)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	for rows.Next() {
		var ordinal, callIndex, contentLength, eventIndex int64
		var toolUseID, agentID, subagentID, source, status, content, timestamp string
		if err := rows.Scan(
			&ordinal, &callIndex, &toolUseID, &agentID, &subagentID,
			&source, &status, &content, &contentLength, &timestamp, &eventIndex,
		); err != nil {
			rows.Close()
			return [sha256.Size]byte{}, err
		}
		writeStagedDigestString(h, "event")
		writeStagedDigestInt(h, ordinal)
		writeStagedDigestInt(h, callIndex)
		writeStagedDigestInt(h, contentLength)
		writeStagedDigestInt(h, eventIndex)
		for _, value := range []string{
			toolUseID, agentID, subagentID, source, status, content, timestamp,
		} {
			writeStagedDigestString(h, value)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return [sha256.Size]byte{}, err
	}
	if err := rows.Close(); err != nil {
		return [sha256.Size]byte{}, err
	}

	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

func commitStagedDerivedStateAndCheckpoint(
	ctx context.Context, conn *sql.Conn, sessionID string,
	signals SessionSignalUpdate, findings []SecretFinding,
	cp *ParserCheckpoint, blobs *ParserCheckpointBlobs,
) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// The staged transcript is byte-for-byte identical, so preserve every
	// message/tool row and the transcript revision. Metadata, detector rules,
	// and prior failed post-processing may still have changed; refresh every
	// derived projection in one transaction with the checkpoint.
	if err := resetIncrementalMarkerTx(tx, sessionID); err != nil {
		return err
	}
	if err := updateSessionAutomationFromMessagesTx(tx, sessionID); err != nil {
		return err
	}
	if err := updateSessionSignalsTx(tx, sessionID, signals); err != nil {
		return err
	}
	if err := replaceSecretFindingsTx(
		tx, sessionID, findings,
		signals.SecretLeakCount, signals.SecretsRulesVersion,
	); err != nil {
		return err
	}
	if cp == nil || blobs == nil {
		if err := deleteParserCheckpointTx(tx, sessionID); err != nil {
			return err
		}
	} else {
		c := *cp
		b := *blobs
		c.SessionID = sessionID
		b.SessionID = sessionID
		if err := upsertParserCheckpointTx(tx, c, b); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) replaceSessionContentStaged(
	ctx context.Context,
	sessionID string, msgs []Message,
	staged StagedToolResults, blocked map[string]bool,
	signalsFn StagedSignalsFunc,
	cp *ParserCheckpoint, blobs *ParserCheckpointBlobs,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("pinning writer connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(
		ctx,
		"ATTACH DATABASE ? AS "+stagedAttachName,
		staged.Path(),
	); err != nil {
		return fmt.Errorf("attaching codex staging db: %w", err)
	}
	defer detachStagedConn(ctx, conn)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	queueGenerationBefore, queueExistedBefore, err := artifactExportGenerationTx(
		tx, sessionID,
	)
	if err != nil {
		return err
	}
	// A cold import has no stored rows for stagedSessionContentDigestTx to
	// find, so contentBefore can only ever prove "changed" against
	// contentAfter -- skip both full-table scans and go straight to the
	// changed-content path below instead of paying to read back every byte
	// this same transaction is about to write.
	hadStoredContent, err := stagedSessionHasStoredMessagesTx(tx, sessionID)
	if err != nil {
		return err
	}
	var contentBefore [sha256.Size]byte
	if hadStoredContent {
		contentBefore, err = stagedSessionContentDigestTx(tx, sessionID)
		if err != nil {
			return fmt.Errorf("fingerprinting stored staged content: %w", err)
		}
	}
	var pendingRecallRevocations recallEvidenceRevocationEvents

	if err := replaceSessionMessagesTxStaged(
		tx, sessionID, msgs, staged, blocked,
	); err != nil {
		return err
	}
	contentUnchanged := false
	if hadStoredContent {
		contentAfter, err := stagedSessionContentDigestTx(tx, sessionID)
		if err != nil {
			return fmt.Errorf("fingerprinting proposed staged content: %w", err)
		}
		contentUnchanged = contentBefore == contentAfter
	}
	if contentUnchanged {
		// Keep the existing row identities, transcript revision, and recall
		// evidence. The session row may have received metadata-only updates
		// before this publish, and detector rules may have advanced, so derived
		// state still has to be refreshed from the verified staged snapshot.
		signals, findings, err := signalsFn(contentFailureVerdicts(staged))
		if err != nil {
			return fmt.Errorf(
				"computing unchanged staged signals for %s: %w",
				sessionID, err,
			)
		}
		if err := tx.Rollback(); err != nil {
			return err
		}
		return commitStagedDerivedStateAndCheckpoint(
			ctx, conn, sessionID, signals, findings, cp, blobs,
		)
	}
	// Summary resolution above recorded per-call content-failure
	// verdicts; fold them into the final signal update and findings now,
	// inside the same transaction as the rows they describe.
	signals, findings, err := signalsFn(contentFailureVerdicts(staged))
	if err != nil {
		return fmt.Errorf(
			"computing staged signals for %s: %w", sessionID, err,
		)
	}
	if err := bumpTranscriptRevisionTx(tx, sessionID); err != nil {
		return err
	}
	if err := reconcileRecallEvidenceForSessionTx(
		ctx, tx, sessionID, &pendingRecallRevocations,
	); err != nil {
		return err
	}
	if err := resetIncrementalMarkerTx(tx, sessionID); err != nil {
		return err
	}
	if err := updateSessionAutomationFromMessagesTx(tx, sessionID); err != nil {
		return err
	}
	if err := updateSessionSignalsTx(tx, sessionID, signals); err != nil {
		return err
	}
	if err := replaceSecretFindingsTx(tx, sessionID, findings,
		signals.SecretLeakCount, signals.SecretsRulesVersion); err != nil {
		return err
	}
	if err := enqueueArtifactExportIfGenerationUnchangedTx(
		tx, sessionID, queueGenerationBefore, queueExistedBefore,
	); err != nil {
		return err
	}
	if cp == nil || blobs == nil {
		if err := deleteParserCheckpointTx(tx, sessionID); err != nil {
			return err
		}
	} else {
		c := *cp
		b := *blobs
		// The checkpoint row is keyed by session id just like the blobs;
		// an idPrefix-rewritten publish must not strand the row under the
		// parser-native id or collide with a local session sharing it.
		c.SessionID = sessionID
		b.SessionID = sessionID
		if err := upsertParserCheckpointTx(tx, c, b); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	pendingRecallRevocations.flush()
	return nil
}

// contentFailureVerdicts reads the per-call verdicts a staging handle
// captured during summary resolution. Handles that do not expose them
// yield nil, which signalsFn treats as an empty verdict set.
func contentFailureVerdicts(staged StagedToolResults) map[string]bool {
	if v, ok := staged.(interface {
		ContentFailures() map[string]bool
	}); ok {
		return v.ContentFailures()
	}
	return nil
}

// detachStagedConn detaches the scratch database from the writer
// connection. It runs after the transaction settles (deferred), so the
// connection returns to the pool clean. A failed detach would poison the
// single-connection writer pool for every later publish, so the
// connection is discarded instead.
func detachStagedConn(ctx context.Context, conn *sql.Conn) {
	if _, err := conn.ExecContext(
		ctx, "DETACH DATABASE "+stagedAttachName,
	); err != nil {
		log.Printf("detaching codex staging db: %v", err)
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// replaceSessionMessagesTxStaged mirrors replaceSessionMessagesTx with the
// tool-result event insert and summary pairing replaced by staged sources:
// tool_calls insert in bounded chunks with per-call summaries resolved on
// the fly, and the event rows come from the staged handle.
func replaceSessionMessagesTxStaged(
	tx *sql.Tx, sessionID string, msgs []Message,
	staged StagedToolResults, blocked map[string]bool,
) error {
	pins, err := savePinsTx(tx, sessionID)
	if err != nil {
		return err
	}
	if err := deleteSessionMessagesTx(tx, sessionID); err != nil {
		return err
	}
	if len(msgs) == 0 {
		return restorePinsTx(tx, sessionID, pins)
	}

	ids, err := insertMessagesTx(tx, msgs)
	if err != nil {
		return err
	}

	positions := make(map[string]StagedToolCallPosition)
	callOccurrences := make(map[string]int)
	chunk := make([]ToolCall, 0, toolCallStagedChunkSize)
	var chunkBytes int64
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if err := insertToolCallsChunkTx(tx, chunk); err != nil {
			return err
		}
		chunk = chunk[:0]
		chunkBytes = 0
		return nil
	}
	for i, m := range msgs {
		for callIdx := range m.ToolCalls {
			tc := ToolCall{
				MessageID:         ids[i],
				SessionID:         m.SessionID,
				ToolName:          m.ToolCalls[callIdx].ToolName,
				Category:          m.ToolCalls[callIdx].Category,
				ToolUseID:         m.ToolCalls[callIdx].ToolUseID,
				InputJSON:         m.ToolCalls[callIdx].InputJSON,
				SkillName:         m.ToolCalls[callIdx].SkillName,
				SubagentSessionID: m.ToolCalls[callIdx].SubagentSessionID,
				FilePath:          m.ToolCalls[callIdx].FilePath,
				CallIndex:         callIdx,
			}
			if tc.ToolUseID != "" {
				occurrence := callOccurrences[tc.ToolUseID]
				callOccurrences[tc.ToolUseID] = occurrence + 1
				stageKey := StagedToolCallKey(tc.ToolUseID, occurrence)
				positions[stageKey] = StagedToolCallPosition{
					ToolUseID: tc.ToolUseID,
					Ordinal:   m.Ordinal,
					CallIndex: callIdx,
				}
				summary, length, err := staged.ResolveSummary(
					context.Background(), stageKey,
				)
				if err != nil {
					return fmt.Errorf(
						"resolving staged summary for %s/%s: %w",
						sessionID, tc.ToolUseID, err,
					)
				}
				tc.ResultContentLength = length
				if !blocked[tc.Category] {
					tc.ResultContent = summary
				}
				chunkBytes += int64(length)
			}
			chunk = append(chunk, tc)
			// Flush by byte budget as well as count: resolved summaries
			// are the largest per-call strings, and a count-only bound
			// (toolCallStagedChunkSize) would still accumulate up to
			// count * max-summary-size bytes of resolved content before
			// the insert.
			if len(chunk) >= toolCallStagedChunkSize ||
				chunkBytes >= toolCallStagedChunkBytes {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}

	if err := staged.InsertEventsTx(
		context.Background(), tx, sessionID, positions,
	); err != nil {
		return err
	}
	return restorePinsTx(tx, sessionID, pins)
}

// toolCallStagedChunkSize bounds the tool-call insert chunks so the
// transient per-chunk summary memory stays fixed. toolCallStagedChunkBytes
// bounds the same chunks by resolved summary content bytes, since one
// call's summary can dwarf hundreds of ordinary rows.
const (
	toolCallStagedChunkSize  = 500
	toolCallStagedChunkBytes = 16 << 20
)
