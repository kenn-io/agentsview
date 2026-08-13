package sync

import (
	"context"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/signals"
)

// incrementalSignalMaintainer folds one incremental write delta into a
// session's signal columns, secret findings, and compact state inside the
// write transaction. It declines (returns a nil delta) whenever the delta
// cannot be folded exactly — the write then invalidates the signal version
// and the debounced full recompute reseeds the state.
type incrementalSignalMaintainer struct {
	engine    *Engine
	sessionID string

	// appended carries the sanitized, filtered message rows the write
	// transaction is inserting.
	appended []db.Message
	// resultUpdates carries the sanitized late tool-result updates.
	resultUpdates []db.ToolCallResultUpdate
	// preWriteRevision is the transcript revision before this write's
	// bump; the persisted state token must match it.
	preWriteRevision string
}

func (e *Engine) newIncrementalSignalMaintainer(
	inc *incrementalUpdate,
	appended []db.Message,
	resultUpdates []db.ToolCallResultUpdate,
	preWriteRevision string,
) db.SignalMaintainer {
	return &incrementalSignalMaintainer{
		engine:           e,
		sessionID:        inc.sessionID,
		appended:         appended,
		resultUpdates:    resultUpdates,
		preWriteRevision: preWriteRevision,
	}
}

func (m *incrementalSignalMaintainer) MaintainTx(
	ctx context.Context, q db.SignalQuery,
) (*db.SignalDelta, error) {
	sess, err := q.Session(ctx)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}

	stored, hasState, err := q.SignalState(ctx)
	if err != nil {
		return nil, err
	}
	if !hasState {
		return nil, nil // seed via the debounced full recompute
	}
	var state signals.IncrementalState
	if err := state.UnmarshalBinary(stored.State); err != nil {
		return nil, nil
	}
	if stored.TranscriptRevision != m.preWriteRevision ||
		stored.SignalVersion != sess.QualitySignalVersion {
		return nil, nil // state fell behind the rows: reseed
	}

	// Appended tool-call rows in the same shape the full compute uses.
	appendedRows := extractToolCallRows(m.appended)

	// Modified facts for late result updates, resolved in-transaction so
	// the fold sees the post-update stored facts.
	modified := make(map[signals.CallPos]signals.ToolFact)
	deleteKeys := make([]db.FindingDeleteKey, 0, len(m.resultUpdates))
	var insertFindings []db.SecretFinding
	useIDs := make([]string, 0, len(m.resultUpdates))
	for _, u := range m.resultUpdates {
		if u.ToolUseID != "" {
			useIDs = append(useIDs, u.ToolUseID)
		}
	}
	callFacts, err := q.ToolCallsByUseID(ctx, useIDs)
	if err != nil {
		return nil, err
	}
	factByUseID := make(map[string]db.ToolCallSignalFact, len(callFacts))
	for _, f := range callFacts {
		factByUseID[f.ToolUseID] = f
	}
	for _, u := range m.resultUpdates {
		fact, ok := factByUseID[u.ToolUseID]
		if !ok {
			continue // update targeted nothing stored
		}
		row := signals.ToolCallRow{
			ToolName:       fact.ToolName,
			Category:       fact.Category,
			InputJSON:      fact.InputJSON,
			ResultContent:  fact.ResultContent,
			MessageOrdinal: fact.MessageOrdinal,
			CallIndex:      fact.CallIndex,
			EventStatus:    fact.EventStatus,
		}
		f := signals.ToolFact{
			CallPos: signals.CallPos{
				MessageOrdinal: fact.MessageOrdinal,
				CallIndex:      fact.CallIndex,
			},
			Failure:        signals.IsFailure(row),
			ExactSignature: signals.ExactToolSignature(row),
			CommandClass:   signals.CommandClass(row),
		}
		modified[f.CallPos] = f

		// Stored events for the call (post-update). When any event
		// exists, the full compute scans events instead of the
		// result_content summary, so the summary-derived findings must go
		// and the stored events are scanned with their real indexes.
		events, err := q.CallResultEvents(
			ctx, fact.MessageOrdinal, fact.CallIndex,
		)
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			continue
		}
		deleteKeys = append(deleteKeys, db.FindingDeleteKey{
			MessageOrdinal: fact.MessageOrdinal,
			CallIndex:      fact.CallIndex,
			LocationKind:   "tool_result",
		})
		for _, ev := range events {
			secretScanBytes.Add(int64(len(ev.Content)))
			matches := secrets.ScanDefinite(ev.Content)
			for _, match := range matches {
				callIdx := fact.CallIndex
				evIdx := ev.EventIndex
				insertFindings = append(insertFindings, db.SecretFinding{
					SessionID:      m.sessionID,
					RuleName:       match.Rule,
					Confidence:     match.Confidence,
					LocationKind:   "tool_result_event",
					MessageOrdinal: fact.MessageOrdinal,
					CallIndex:      &callIdx,
					EventIndex:     &evIdx,
					MatchStart:     match.Start,
					MatchEnd:       match.End,
					MatchIndex:     match.Index,
					RedactedMatch:  match.Redacted,
					RulesVersion:   secrets.DefiniteRulesVersion(),
				})
			}
		}
	}

	// Appended message content: scan exactly the stored rows.
	newFindings, _ := scanSecretsFromMessages(
		db.Session{}, m.appended, secrets.ScanDefinite,
	)
	insertFindings = append(insertFindings, newFindings...)

	row := signals.ToolHealthRow{
		FailureCount:   sess.ToolFailureSignalCount,
		RetryCount:     sess.ToolRetryCount,
		EditChurnCount: sess.EditChurnCount,
	}
	nextState, toolHealth, ok := state.FoldToolHealth(
		appendedRows, modified, row,
	)
	if !ok {
		return nil, nil // out-of-window modification: reseed
	}

	// Message-derived aggregates.
	lastRole, lastContent := nextState.LastRole, nextState.LastContent
	msgIndex := nextState.MsgIndex
	modelCounts := cloneCounts(nextState.ModelCounts)
	modelFirstSeen := cloneCounts(nextState.ModelFirstSeen)
	compactionDelta := 0
	lastTokens := nextState.LastValidTokens
	appendedHasContextData := false
	for _, msg := range m.appended {
		msgIndex++
		if msg.IsSystem {
			continue
		}
		lastRole, lastContent = string(msg.Role), msg.Content
		if msg.Role == "assistant" && msg.Model != "" {
			if _, seen := modelCounts[msg.Model]; !seen {
				modelFirstSeen[msg.Model] = msgIndex
			}
			modelCounts[msg.Model]++
		}
		if msg.Role == "assistant" && msg.HasContextTokens {
			appendedHasContextData = true
			if lastTokens > 0 &&
				float64(msg.ContextTokens) < 0.7*float64(lastTokens) {
				compactionDelta++
			}
			lastTokens = msg.ContextTokens
		}
	}
	nextState.LastRole = lastRole
	nextState.LastContent = lastContent
	nextState.MsgIndex = msgIndex
	nextState.ModelCounts = modelCounts
	nextState.ModelFirstSeen = modelFirstSeen
	nextState.LastValidTokens = lastTokens

	hasToolCalls := sess.HasToolCalls || len(appendedRows) > 0
	hasContextData := sess.HasContextData || appendedHasContextData
	noCodeContext := sess.NoCodeContextCount
	if noCodeContext > 0 {
		for _, r := range appendedRows {
			if signals.IsContextToolCall(r) {
				noCodeContext = 0
				break
			}
		}
	}
	compactionCount := sess.CompactionCount + compactionDelta
	midTaskCount := sess.MidTaskCompactionCount +
		toolHealth.MidTaskCompactions

	// Outcome and score from the compact aggregates.
	var lastActivity time.Time
	if sess.EndedAt != nil {
		lastActivity, _ = time.Parse(time.RFC3339Nano, *sess.EndedAt)
	}
	outcomeResult := signals.ClassifyOutcome(signals.OutcomeInput{
		IsAutomated:        sess.IsAutomated,
		MessageCount:       sess.MessageCount,
		EndedWithRole:      lastRole,
		FinalFailureStreak: toolHealth.FinalFailureStreak,
		LastAssistantText:  lastContent,
		LastActivity:       lastActivity,
	})
	model := mostCommonModelFromCounts(modelCounts, modelFirstSeen)
	pressure := signals.ComputeContextPressure(
		nil, sess.PeakContextTokens, model,
	)
	scoreResult := signals.ComputeHealthScore(signals.ScoreInput{
		Outcome:                outcomeResult.Outcome,
		OutcomeConfidence:      outcomeResult.Confidence,
		HasToolCalls:           hasToolCalls,
		FailureSignalCount:     toolHealth.FailureCount,
		RetryCount:             toolHealth.RetryCount,
		EditChurnCount:         toolHealth.EditChurnCount,
		ConsecutiveFailMax:     toolHealth.ConsecutiveFailureMax,
		HasContextData:         hasContextData,
		CompactionCount:        compactionCount,
		MidTaskCompactionCount: midTaskCount,
		PressureMax:            pressure.PressureMax,
		Heuristics: signals.HeuristicSignals{
			ShortPromptCount:            sess.ShortPromptCount,
			UnstructuredStart:           sess.UnstructuredStart,
			MissingSuccessCriteriaCount: sess.MissingSuccessCriteriaCount,
			MissingVerificationCount:    sess.MissingVerificationCount,
			DuplicatePromptCount:        sess.DuplicatePromptCount,
			NoCodeContextCount:          noCodeContext,
			RunawayToolLoopCount:        toolHealth.RunawayToolLoopCount,
		},
	})

	var pendingSince *string
	if outcomeResult.IsRecent {
		now := time.Now().UTC().Format(time.RFC3339)
		pendingSince = &now
	}
	var healthGrade *string
	if scoreResult.Grade != "" {
		healthGrade = &scoreResult.Grade
	}

	update := db.SessionSignalUpdate{
		ToolFailureSignalCount: toolHealth.FailureCount,
		ToolRetryCount:         toolHealth.RetryCount,
		EditChurnCount:         toolHealth.EditChurnCount,
		ConsecutiveFailureMax:  toolHealth.ConsecutiveFailureMax,
		Outcome:                outcomeResult.Outcome,
		OutcomeConfidence:      outcomeResult.Confidence,
		EndedWithRole:          lastRole,
		FinalFailureStreak:     toolHealth.FinalFailureStreak,
		SignalsPendingSince:    pendingSince,
		CompactionCount:        compactionCount,
		MidTaskCompactionCount: midTaskCount,
		ContextPressureMax:     pressure.PressureMax,
		HealthScore:            scoreResult.Score,
		HealthGrade:            healthGrade,
		HasToolCalls:           hasToolCalls,
		HasContextData:         hasContextData,
		SecretsRulesVersion:    secrets.DefiniteRulesVersion(),
		QualitySignals: db.QualitySignals{
			Version:           db.CurrentQualitySignalVersion,
			ShortPromptCount:  sess.ShortPromptCount,
			UnstructuredStart: sess.UnstructuredStart,
			MissingSuccessCriteriaCount: sess.
				MissingSuccessCriteriaCount,
			MissingVerificationCount: sess.
				MissingVerificationCount,
			DuplicatePromptCount: sess.DuplicatePromptCount,
			NoCodeContextCount:   noCodeContext,
			RunawayToolLoopCount: toolHealth.RunawayToolLoopCount,
		},
	}

	revision, err := q.TranscriptRevision(ctx)
	if err != nil {
		return nil, err
	}
	blob, err := nextState.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf(
			"encoding signal state %s: %w", m.sessionID, err,
		)
	}

	return &db.SignalDelta{
		Update:            update,
		InsertFindings:    insertFindings,
		DeleteFindingKeys: deleteKeys,
		State: &db.SessionSignalState{
			SessionID:          m.sessionID,
			State:              blob,
			TranscriptRevision: revision,
			SignalVersion:      db.CurrentQualitySignalVersion,
		},
	}, nil
}

func cloneCounts(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// mostCommonModelFromCounts mirrors extractMostCommonModel's tie-break:
// the model appearing most often, ties broken by first chronological
// appearance.
func mostCommonModelFromCounts(
	counts, firstSeen map[string]int,
) string {
	var best string
	bestCount := -1
	for model, n := range counts {
		switch {
		case n > bestCount:
			best, bestCount = model, n
		case n == bestCount && firstSeen[model] < firstSeen[best]:
			best = model
		}
	}
	return best
}

// extractModelCounts mirrors extractMostCommonModel's walk: per-model
// counts, first chronological appearance (by message index), and the total
// message count the maintainer continues indexing from.
func extractModelCounts(
	msgs []db.Message,
) (counts, firstSeen map[string]int, msgIndex int) {
	counts = map[string]int{}
	firstSeen = map[string]int{}
	for i, m := range msgs {
		if m.Role != "assistant" || m.Model == "" {
			continue
		}
		counts[m.Model]++
		if _, ok := firstSeen[m.Model]; !ok {
			firstSeen[m.Model] = i
		}
	}
	return counts, firstSeen, len(msgs)
}

// seedSignalStateFromFull builds and persists the compact incremental state
// after a full signal recompute so later incremental deltas can fold. The
// persisted token (transcript revision + signal version) is read after the
// recompute's rows committed; a state that falls behind is rejected by the
// maintainer and reseeded by the next full recompute.
func (e *Engine) seedSignalStateFromFull(
	sessionID string, msgs []db.Message,
) error {
	lastRole, lastContent := extractLastMessageRole(msgs)
	counts, firstSeen, msgIndex := extractModelCounts(msgs)
	lastTokens := 0
	for _, m := range slices.Backward(msgs) {
		if m.Role == "assistant" && m.HasContextTokens {
			lastTokens = m.ContextTokens
			break
		}
	}
	state := signals.SeedIncrementalState(
		extractToolCallRows(msgs),
		extractCompactBoundaryOrdinals(msgs),
		lastRole, lastContent,
		counts, firstSeen, msgIndex, lastTokens,
	)
	blob, err := state.MarshalBinary()
	if err != nil {
		return fmt.Errorf(
			"encoding signal state %s: %w", sessionID, err,
		)
	}
	rev, err := e.db.TranscriptRevision(sessionID)
	if err != nil {
		return err
	}
	return e.db.UpsertSessionSignalState(db.SessionSignalState{
		SessionID:          sessionID,
		State:              blob,
		TranscriptRevision: rev,
		SignalVersion:      db.CurrentQualitySignalVersion,
	})
}
