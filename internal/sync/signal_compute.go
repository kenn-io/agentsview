package sync

import (
	"time"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/signals"
)

// computeSignalsFromMessages produces a SessionSignalUpdate from
// in-memory session metadata and messages. Pure function with no
// DB access — the caller already holds everything we need.
//
// Used by both the live write paths (writeBatch, writeSessionFull,
// writeIncremental) and the legacy backfill path (RecomputeSignals,
// which reads msgs from the DB once and then calls this).
func computeSignalsFromMessages(
	sess db.Session, msgs []db.Message,
) db.SessionSignalUpdate {
	toolRows := extractToolCallRows(msgs)
	ctxTokens := extractContextTokens(msgs)
	boundaries := extractCompactBoundaryOrdinals(msgs)
	model := extractMostCommonModel(msgs)
	lastRole, lastContent := extractLastMessageRole(msgs)

	toolHealth := signals.ComputeToolHealth(toolRows)
	ctxPressure := signals.ComputeContextPressure(
		ctxTokens, sess.PeakContextTokens, model,
	)

	// Prefer explicit boundary count when available; fall back
	// to the token-drop heuristic for sessions without
	// compact-boundary messages.
	compactionCount := ctxPressure.CompactionCount
	if len(boundaries) > 0 {
		compactionCount = len(boundaries)
	}

	midTaskCalls := make(
		[]signals.ToolCallOrdinal, 0, len(toolRows),
	)
	for _, t := range toolRows {
		midTaskCalls = append(midTaskCalls,
			signals.ToolCallOrdinal{
				MessageOrdinal: t.MessageOrdinal,
				ToolName:       t.ToolName,
			})
	}
	midTaskCount := signals.CountMidTaskCompactions(
		boundaries, midTaskCalls,
	)

	finalStreak := computeFinalStreak(toolRows)

	var lastActivity time.Time
	if sess.EndedAt != nil {
		lastActivity, _ = time.Parse(
			time.RFC3339Nano, *sess.EndedAt,
		)
	}

	outcomeResult := signals.ClassifyOutcome(signals.OutcomeInput{
		IsAutomated:        sess.IsAutomated,
		MessageCount:       sess.MessageCount,
		EndedWithRole:      lastRole,
		FinalFailureStreak: finalStreak,
		LastAssistantText:  lastContent,
		LastActivity:       lastActivity,
	})

	hasContextData := sess.HasPeakContextTokens
	if !hasContextData {
		for _, t := range ctxTokens {
			if t.HasContextTokens {
				hasContextData = true
				break
			}
		}
	}

	scoreResult := signals.ComputeHealthScore(signals.ScoreInput{
		Outcome:                outcomeResult.Outcome,
		OutcomeConfidence:      outcomeResult.Confidence,
		HasToolCalls:           len(toolRows) > 0,
		FailureSignalCount:     toolHealth.FailureSignalCount,
		RetryCount:             toolHealth.RetryCount,
		EditChurnCount:         toolHealth.EditChurnCount,
		ConsecutiveFailMax:     toolHealth.ConsecutiveFailureMax,
		HasContextData:         hasContextData,
		CompactionCount:        compactionCount,
		MidTaskCompactionCount: midTaskCount,
		PressureMax:            ctxPressure.PressureMax,
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

	return db.SessionSignalUpdate{
		ToolFailureSignalCount: toolHealth.FailureSignalCount,
		ToolRetryCount:         toolHealth.RetryCount,
		EditChurnCount:         toolHealth.EditChurnCount,
		ConsecutiveFailureMax:  toolHealth.ConsecutiveFailureMax,
		Outcome:                outcomeResult.Outcome,
		OutcomeConfidence:      outcomeResult.Confidence,
		EndedWithRole:          lastRole,
		FinalFailureStreak:     finalStreak,
		SignalsPendingSince:    pendingSince,
		CompactionCount:        compactionCount,
		MidTaskCompactionCount: midTaskCount,
		ContextPressureMax:     ctxPressure.PressureMax,
		HealthScore:            scoreResult.Score,
		HealthGrade:            healthGrade,
		HasToolCalls:           len(toolRows) > 0,
		HasContextData:         hasContextData,
	}
}

// extractToolCallRows builds signal inputs from in-memory tool
// calls. CallIndex is the call's position within its message.
// EventStatus is the status of the latest result event (events
// are stored in event_index order, so the last one wins).
func extractToolCallRows(
	msgs []db.Message,
) []signals.ToolCallRow {
	rows := make([]signals.ToolCallRow, 0)
	for _, m := range msgs {
		for callIdx, tc := range m.ToolCalls {
			status := ""
			if n := len(tc.ResultEvents); n > 0 {
				status = tc.ResultEvents[n-1].Status
			}
			rows = append(rows, signals.ToolCallRow{
				ToolName:       tc.ToolName,
				Category:       tc.Category,
				InputJSON:      tc.InputJSON,
				ResultContent:  tc.ResultContent,
				MessageOrdinal: m.Ordinal,
				CallIndex:      callIdx,
				EventStatus:    status,
			})
		}
	}
	return rows
}

// extractContextTokens returns context-token measurements for
// assistant messages in order.
func extractContextTokens(
	msgs []db.Message,
) []signals.ContextTokenRow {
	var rows []signals.ContextTokenRow
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		rows = append(rows, signals.ContextTokenRow{
			ContextTokens:    m.ContextTokens,
			HasContextTokens: m.HasContextTokens,
		})
	}
	return rows
}

// extractCompactBoundaryOrdinals returns ordinals of explicit
// compact-boundary messages in ascending order.
func extractCompactBoundaryOrdinals(msgs []db.Message) []int {
	var ords []int
	for _, m := range msgs {
		if m.IsCompactBoundary {
			ords = append(ords, m.Ordinal)
		}
	}
	return ords
}

// extractMostCommonModel returns the assistant model name that
// appears most often. Ties are broken by the model that appears
// first chronologically — matches SQLite's GROUP BY iteration
// order closely enough that legacy and live computations agree
// on real sessions (every observed session has a clear majority
// model).
func extractMostCommonModel(msgs []db.Message) string {
	counts := map[string]int{}
	firstSeen := map[string]int{}
	for i, m := range msgs {
		if m.Role != "assistant" || m.Model == "" {
			continue
		}
		counts[m.Model]++
		if _, ok := firstSeen[m.Model]; !ok {
			firstSeen[m.Model] = i
		}
	}
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

// extractLastMessageRole returns the role and content of the
// last non-system message. Empty strings if none.
func extractLastMessageRole(
	msgs []db.Message,
) (role, content string) {
	if msgs == nil {
		return "", ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if !msgs[i].IsSystem {
			return msgs[i].Role, msgs[i].Content
		}
	}
	return "", ""
}
