package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
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
	var pendingRecallRevocations recallEvidenceRevocationEvents

	if err := replaceSessionMessagesTxStaged(
		tx, sessionID, msgs, staged, blocked,
	); err != nil {
		return err
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
				positions[tc.ToolUseID] = StagedToolCallPosition{
					ToolUseID: tc.ToolUseID,
					Ordinal:   m.Ordinal,
					CallIndex: callIdx,
				}
				summary, length, err := staged.ResolveSummary(
					context.Background(), tc.ToolUseID,
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
