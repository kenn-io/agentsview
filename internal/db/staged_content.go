package db

import (
	"context"
	"database/sql"
	"fmt"
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
	// final message coordinates in positions.
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

// StagedToolCallPosition identifies one tool call's final coordinates.
type StagedToolCallPosition struct {
	ToolUseID string
	Ordinal   int
	CallIndex int
}

// ReplaceSessionContentStaged replaces a session's content from a
// streaming parse: messages and tool-call metadata arrive as the (small)
// in-memory slice whose result events carry placeholders, while the event
// rows and per-call summaries come from the staged handle. Blocked
// categories are blanked exactly like the legacy write path. This is the
// atomic publish of the staged cold-import path; it mirrors
// ReplaceSessionContent's transaction sequence with the event/summary
// inserts replaced by staged sources.
func (db *DB) ReplaceSessionContentStaged(
	sessionID string, msgs []Message,
	signals SessionSignalUpdate, findings []SecretFinding,
	staged StagedToolResults, blocked map[string]bool,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
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
	if err := bumpTranscriptRevisionTx(tx, sessionID); err != nil {
		return err
	}
	if err := reconcileRecallEvidenceForSessionTx(
		context.Background(), tx, sessionID, &pendingRecallRevocations,
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
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if err := insertToolCallsChunkTx(tx, chunk); err != nil {
			return err
		}
		chunk = chunk[:0]
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
			}
			chunk = append(chunk, tc)
			if len(chunk) >= toolCallStagedChunkSize {
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
// transient per-chunk summary memory stays fixed.
const toolCallStagedChunkSize = 500
