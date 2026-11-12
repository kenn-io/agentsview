package sync

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/timeutil"
)

// writeIncremental appends new messages and partially updates
// session metadata without overwriting columns that are not
// recomputed during incremental parsing (e.g. parent_session_id,
// relationship_type). Codex refreshes file_hash because parse-diff
// uses it as the transcript fingerprint for raced-skew detection;
// Claude refreshes it so providerSingleSessionFresh can use the
// stored hash as a content fingerprint against same-size in-place
// rewrites. Other agents pass an empty hash, which COALESCE leaves
// untouched.
//
// A suffix-replacement delta (suffixReplace) replaces only the stored
// rows from replaceFromOrdinal onward. The database adjusts the session
// aggregates by what the removed and inserted rows contribute, so the
// count and token fields computed here are ignored on that path.
func (e *Engine) writeIncremental(ctx context.Context,
	inc *incrementalUpdate,
) error {
	// The full path vetoes filtered sessions in prepareSessionWrite;
	// this is the equivalent veto at the incremental write seam, so
	// no producer can append to a session outside the cwd allow-list.
	// tryIncrementalJSONL already refuses such sessions — this guard
	// keeps the seam safe for any future producer.
	if !e.cwdFilter.allows(inc.cwd) {
		log.Printf(
			"incremental %s: cwd %q outside the configured "+
				"allow-list, skipping append",
			inc.sessionID, inc.cwd,
		)
		return nil
	}

	dbMsgs, err := toDBMessagesContext(
		ctx,
		pendingWrite{
			sess: parser.ParsedSession{ID: inc.sessionID, Agent: inc.agent},
			msgs: inc.msgs,
		},
		e.blockedResultCategories,
	)
	if err != nil {
		return err
	}
	dbMsgs, _ = e.db.ProjectToolResultImagesWithPolicy(dbMsgs, e.toolResultImages)
	// The incremental append path bypasses prepareSessionWrite, so run
	// the central validation/sanitization pass on the new message rows
	// here to keep coverage uniform across write paths. The fix counts
	// feed the sync summary's anomaly section.
	//
	// Deliberately only sanitize fixes are recorded here, not malformed-line
	// counts. A malformed JSONL line appended to an actively-syncing file is
	// skipped by the incremental reader, and incrementalParseFunc carries no
	// malformed-line count, so surfacing it on this path would require
	// threading a new return value through the incremental parser API across
	// every append-only agent. That is intentionally out of scope for this
	// best-effort, only-when-nonzero diagnostic: the value is still parsed and
	// persisted, and the next full sync (the periodic pass, or any
	// parser-version bump that forces a full resync) re-derives the
	// malformed-line count for the file. The incremental path therefore
	// under-reports a brand-new summary signal by at most one full-sync
	// interval; it never loses stored data and is not a regression on any
	// prior behavior (no malformed-line count was surfaced anywhere before
	// this feature). Full malformed-line coverage on the incremental path is a
	// deferred follow-up.
	sanitizeStats, err := validateAndSanitizeContext(ctx, nil, dbMsgs, nil)
	if err != nil {
		return err
	}
	e.anomalies.recordSanitize(sanitizeStats)

	// Adjust counts for blocked-category filtering.
	newTotal, newUser, err := postFilterCountsContext(ctx, dbMsgs)
	if err != nil {
		return err
	}
	filtered := len(inc.msgs) - newTotal
	msgCount := inc.msgCount - filtered
	userFiltered := countUserMsgs(inc.msgs) - newUser
	userMsgCount := inc.userMsgCount - userFiltered
	var endedAt *string
	if !inc.endedAt.IsZero() {
		s := inc.endedAt.Format(time.RFC3339Nano)
		endedAt = &s
	}
	// Run the appended ended_at through the same timestamp plausibility
	// check the full path applies in sanitizeSession, so an implausible
	// appended timestamp is blanked here instead of persisting via the
	// incremental path while a full sync of the same file would blank it
	// (an incremental-vs-full parity divergence). The session token
	// aggregates (totalOutputTokens/peakContextTokens) are accumulated from
	// per-message values already clamped to the per-message bound (see the
	// clampedTokens calls feeding this update), so a corrupt new message
	// cannot inflate them past what the stored rows justify -- parity with
	// the full path, which re-derives message-derived totals from the
	// clamped rows. The sum itself is not clamped to the per-message bound,
	// since a long session legitimately exceeds it.
	endedAt, _ = blankImplausibleTimestampPtr(endedAt)

	subagentLinks := make([]db.ToolCallSubagentLink, len(inc.links))
	for i, link := range inc.links {
		subagentLinks[i] = db.ToolCallSubagentLink{
			ToolUseID: link.ToolUseID,
			SubagentSessionID: applyIDPrefixToID(
				e.idPrefix, link.SubagentSessionID,
			),
			HasResult:        link.HasResult,
			ResultContentLen: link.ResultContentLen,
		}
		toolCall := db.ToolCall{
			ResultContent:       parser.DecodeContent(link.ResultContentRaw),
			ResultContentLength: link.ResultContentLen,
		}
		if link.HasResult {
			event := db.ToolResultEvent{
				ToolUseID: link.ToolUseID, Source: "tool_result", Status: link.ResultStatus,
				Content: toolCall.ResultContent, ContentLength: link.ResultContentLen,
				Timestamp: timeutil.Format(link.ResultTimestamp),
			}
			db.PrepareToolResultEvent(&event)
			toolCall.ResultEvents = []db.ToolResultEvent{event}
		}
		e.anomalies.recordSanitize(db.SanitizeToolCall(&toolCall))
		subagentLinks[i].ResultContent = toolCall.ResultContent
		subagentLinks[i].ResultContentLen = toolCall.ResultContentLength
		subagentLinks[i].ResultEvents = toolCall.ResultEvents
	}
	toolCallResultUpdates := make(
		[]db.ToolCallResultUpdate, len(inc.toolCallUpdates),
	)
	for i, update := range inc.toolCallUpdates {
		resultEvents, err := convertToolResultEventsContext(
			ctx, update.ResultEvents,
		)
		if err != nil {
			return err
		}
		toolCall := db.ToolCall{ResultEvents: resultEvents}
		for j := range toolCall.ResultEvents {
			db.PrepareToolResultEvent(&toolCall.ResultEvents[j])
			toolCall.ResultEvents[j].SubagentSessionID = applyIDPrefixToID(
				e.idPrefix, toolCall.ResultEvents[j].SubagentSessionID,
			)
		}
		// The database sanitizes after resolving the target category, preserving
		// raw identity and blocked-output lengths until then.
		toolCallResultUpdates[i] = db.ToolCallResultUpdate{
			ToolUseID: update.ToolUseID,
			Position: db.ToolCallPosition{
				MessageOrdinal: update.MessageOrdinal,
				CallIndex:      update.CallIndex,
			},
			Events: toolCall.ResultEvents,
		}
	}
	messageUsageUpdates := make(
		[]db.MessageTokenUsageUpdate, len(inc.messageUsageUpdates),
	)
	for i, update := range inc.messageUsageUpdates {
		messageUsageUpdates[i] = db.MessageTokenUsageUpdate{
			Ordinal:          update.Ordinal,
			TokenUsage:       append([]byte(nil), update.TokenUsage...),
			ContextTokens:    clampedTokens(update.ContextTokens),
			OutputTokens:     clampedTokens(update.OutputTokens),
			HasContextTokens: update.HasContextTokens,
			HasOutputTokens:  update.HasOutputTokens,
		}
	}

	var replaceFromOrdinal *int
	if inc.suffixReplace {
		replaceFromOrdinal = new(inc.replaceFromOrdinal)
	}
	var signalsMaintained bool
	var maintainer db.SignalMaintainer
	// Fold checkpoint-backed Codex deltas transactionally. Other append paths
	// retain their debounced recompute instead of paying for state reads and
	// signal writes on every streamed assistant message.
	if isCodexFormatAgent(inc.agent) && inc.checkpoint != nil && !e.disableSignalRecompute &&
		!inc.hasSubstantiveUserMessage() &&
		!inc.hasCompactBoundary() &&
		!inc.hasResultSubagentLink() {
		preRev, err := e.db.TranscriptRevision(ctx, inc.sessionID)
		if err == nil {
			var preSecrets string
			if preSecrets, err = e.db.SessionSecretsRulesVersion(ctx,
				inc.sessionID,
			); err == nil {
				maintainer = e.newIncrementalSignalMaintainer(
					inc, dbMsgs, toolCallResultUpdates,
					messageUsageUpdates, preRev, preSecrets,
				)
			}
		}
	}
	signalsMaintained, err = e.db.WriteSessionIncrementalWithToolResultImages(ctx,
		inc.sessionID,
		dbMsgs,
		db.IncrementalSessionUpdate{
			EndedAt:                  endedAt,
			TerminationStatus:        inc.terminationStatus,
			MsgCount:                 msgCount,
			UserMsgCount:             userMsgCount,
			FileSize:                 inc.fileSize,
			FileMtime:                inc.fileMtime,
			FileHash:                 strPtr(inc.fileHash),
			NextOrdinal:              inc.nextOrdinal,
			LastEntryUUID:            inc.lastEntryUUID,
			TotalOutputTokens:        inc.totalOutputTokens,
			PeakContextTokens:        inc.peakContextTokens,
			HasTotalOutputTokens:     inc.hasTotalOutputTokens,
			HasPeakContextTokens:     inc.hasPeakContextTokens,
			SubagentLinks:            subagentLinks,
			ToolCallResultUpdates:    toolCallResultUpdates,
			MessageTokenUsageUpdates: messageUsageUpdates,
			Checkpoint:               inc.checkpoint,
			CheckpointBlobs:          inc.checkpointBlobs,
			BlockedResultCategories:  e.blockedResultCategories,
			SignalMaintainer:         maintainer,
			ReplaceFromOrdinal:       replaceFromOrdinal,
		},
		e.toolResultImages,
	)
	if err != nil {
		return fmt.Errorf(
			"incremental write %s: %w",
			inc.sessionID, err,
		)
	}
	if e.incrementalCommittedHook != nil {
		e.incrementalCommittedHook(inc.sessionID)
	}

	// The write has committed the new file size and offset, so a later sync
	// sees no change and will not redo the project mapping or signal
	// recompute below. Finish them even if the caller cancels now. The work
	// covers this one session.
	ctx = context.WithoutCancel(ctx)
	finalProject, err := e.applyWorktreeMappingToSingleSession(
		ctx, inc.sessionID,
	)
	if err != nil {
		return err
	}
	identitySession := db.Session{
		ID:      inc.sessionID,
		Project: finalProject,
		Machine: inc.machine,
		Cwd:     inc.cwd,
	}
	if err := e.writeProjectIdentityObservationWithSnapshotProject(
		ctx,
		identitySession,
		inc.sourceProject,
		inc.recordedGitRemote,
	); err != nil {
		log.Printf(
			"incremental project identity observation %s: %v",
			inc.sessionID, err,
		)
	}

	// Signal/secret maintenance normally ran inside the incremental write
	// transaction. When the maintainer declined (user prompts, compact
	// boundaries, stale state, out-of-window updates), fall back to the
	// debounced full recompute: the first write after a quiet period
	// recomputes inline, writes during a streaming burst coalesce into one
	// recompute per interval plus a trailing flush. Recompute errors are
	// logged inside recomputeSignalsFromDB and are non-fatal; a later
	// write or flush retries.
	if !signalsMaintained {
		e.signalSched.markDirty(ctx, inc.sessionID)
	}
	if inc.providerStatHash != nil {
		e.recordProviderStatHash(
			ctx, *inc.providerStatHash,
		)
	}

	return nil
}

// writeMessages uses an incremental append when possible.
// Session files are append-only, so if the DB already has
// messages for this session and the new set is larger, we
// only insert the new messages (avoiding expensive FTS5
// delete+reinsert of existing content).
func (e *Engine) writeMessages(ctx context.Context,
	sessionID string, msgs []db.Message,
) error {
	maxOrd := e.db.MaxOrdinal(ctx, sessionID)

	// No existing messages — insert all.
	if maxOrd < 0 {
		if err := e.db.InsertMessagesWithToolResultImages(ctx,
			msgs, e.toolResultImages,
		); err != nil {
			return fmt.Errorf(
				"insert messages for %s: %w",
				sessionID, err,
			)
		}
		return nil
	}

	// Find new messages (ordinal > maxOrd).
	delta := 0
	for i, m := range msgs {
		if m.Ordinal > maxOrd {
			delta = len(msgs) - i
			msgs = msgs[i:]
			break
		}
	}

	if delta == 0 {
		return nil
	}

	if err := e.db.InsertMessagesWithToolResultImages(ctx,
		msgs, e.toolResultImages,
	); err != nil {
		return fmt.Errorf(
			"append messages for %s: %w",
			sessionID, err,
		)
	}
	return nil
}
