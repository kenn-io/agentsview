package ingest

import (
	"slices"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/signals"
)

var secretScanBytes atomic.Int64

// SecretScanBytes returns the total content bytes scanned by shared ingestion.
func SecretScanBytes() int64 {
	return secretScanBytes.Load()
}

// ComputeSignalsAndSecrets derives aggregate signals and definite findings
// from the same retained message graph.
func ComputeSignalsAndSecrets(
	session db.Session, messages []db.Message,
) (db.SessionSignalUpdate, []db.SecretFinding) {
	update := ComputeSignalsFromMessages(session, messages)
	findings, leak := ScanSecretsFromMessages(
		session, messages, secrets.ScanDefinite,
	)
	update.SecretLeakCount = leak
	update.SecretsRulesVersion = secrets.DefiniteRulesVersion()
	return update, findings
}

// ComputeSignalsAndSecretsWithContentFailures uses precomputed staged result
// failures when placeholder content cannot carry the verdict.
func ComputeSignalsAndSecretsWithContentFailures(
	session db.Session, messages []db.Message, failures map[string]bool,
) (db.SessionSignalUpdate, []db.SecretFinding) {
	update := ComputeSignalsFromMessagesWithContentFailures(
		session, messages, failures,
	)
	findings, leak := ScanSecretsFromMessages(
		session, messages, secrets.ScanDefinite,
	)
	update.SecretLeakCount = leak
	update.SecretsRulesVersion = secrets.DefiniteRulesVersion()
	return update, findings
}

// ComputeSignalsFromMessages derives session signals from normalized rows.
func ComputeSignalsFromMessages(
	session db.Session, messages []db.Message,
) db.SessionSignalUpdate {
	return ComputeSignalsFromToolRows(
		session, messages, ExtractToolCallRows(messages),
	)
}

// ComputeSignalsFromMessagesWithContentFailures derives signals using staged
// content-failure verdicts.
func ComputeSignalsFromMessagesWithContentFailures(
	session db.Session, messages []db.Message, failures map[string]bool,
) db.SessionSignalUpdate {
	rows := ExtractToolCallRows(messages)
	PatchToolCallRowsWithContentFailures(rows, messages, failures)
	return ComputeSignalsFromToolRows(session, messages, rows)
}

// PatchToolCallRowsWithContentFailures applies staged verdicts to matching
// occurrence-qualified calls whose event status does not already decide them.
func PatchToolCallRowsWithContentFailures(
	rows []signals.ToolCallRow, messages []db.Message,
	failures map[string]bool,
) {
	if len(failures) == 0 {
		return
	}
	index := 0
	occurrences := make(map[string]int)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if index >= len(rows) {
				return
			}
			failed := false
			if call.ToolUseID != "" {
				occurrence := occurrences[call.ToolUseID]
				occurrences[call.ToolUseID] = occurrence + 1
				failed = failures[db.StagedToolCallKey(
					call.ToolUseID, occurrence,
				)]
			}
			if failed && rows[index].EventStatus == "" {
				rows[index].ContentFailure = true
			}
			index++
		}
	}
}

// ComputeSignalsFromToolRows derives the complete aggregate signal update.
func ComputeSignalsFromToolRows(
	session db.Session, messages []db.Message, rows []signals.ToolCallRow,
) db.SessionSignalUpdate {
	heuristics := signals.AnalyzeHeuristics(signals.HeuristicInput{
		Messages: ExtractHeuristicMessages(messages), ToolRows: rows,
	})
	contextTokens := ExtractContextTokens(messages)
	boundaries := ExtractCompactBoundaryOrdinals(messages)
	model := ExtractMostCommonModel(messages)
	lastRole, _ := ExtractLastMessageRole(messages)
	toolHealth := signals.ComputeToolHealth(rows)
	contextPressure := signals.ComputeContextPressure(
		contextTokens, session.PeakContextTokens, model,
	)
	compactionCount := contextPressure.CompactionCount
	if len(boundaries) > 0 {
		compactionCount = len(boundaries)
	}
	ordinalRows := make([]signals.ToolCallOrdinal, 0, len(rows))
	for _, row := range rows {
		ordinalRows = append(ordinalRows, signals.ToolCallOrdinal{
			MessageOrdinal: row.MessageOrdinal, ToolName: row.ToolName,
		})
	}
	midTaskCount := signals.CountMidTaskCompactions(boundaries, ordinalRows)
	finalStreak := ComputeFinalStreak(rows)
	hasContextData := session.HasPeakContextTokens
	if !hasContextData {
		for _, row := range contextTokens {
			if row.HasContextTokens {
				hasContextData = true
				break
			}
		}
	}
	update := db.SessionSignalUpdate{
		ToolFailureSignalCount: toolHealth.FailureSignalCount,
		ToolRetryCount:         toolHealth.RetryCount,
		EditChurnCount:         toolHealth.EditChurnCount,
		ConsecutiveFailureMax:  toolHealth.ConsecutiveFailureMax,
		EndedWithRole:          lastRole, FinalFailureStreak: finalStreak,
		CompactionCount:        compactionCount,
		MidTaskCompactionCount: midTaskCount,
		ContextPressureMax:     contextPressure.PressureMax,
		HasToolCalls:           len(rows) > 0, HasContextData: hasContextData,
		QualitySignals: db.QualitySignals{
			Version:                     db.CurrentQualitySignalVersion,
			ShortPromptCount:            heuristics.ShortPromptCount,
			UnstructuredStart:           heuristics.UnstructuredStart,
			MissingSuccessCriteriaCount: heuristics.MissingSuccessCriteriaCount,
			MissingVerificationCount:    heuristics.MissingVerificationCount,
			DuplicatePromptCount:        heuristics.DuplicatePromptCount,
			NoCodeContextCount:          heuristics.NoCodeContextCount,
			RunawayToolLoopCount:        heuristics.RunawayToolLoopCount,
		},
	}
	return RefreshSignalRecencyAt(session, messages, update, time.Now())
}

// RefreshSignalRecencyAt settles only clock-derived state using the same stable
// signal counters and retained transcript. It does not rescan secrets or tools.
func RefreshSignalRecencyAt(session db.Session, messages []db.Message, update db.SessionSignalUpdate, now time.Time) db.SessionSignalUpdate {
	var lastActivity time.Time
	if session.EndedAt != nil {
		lastActivity, _ = time.Parse(time.RFC3339Nano, *session.EndedAt)
	}
	lastRole, lastContent := ExtractLastMessageRole(messages)
	outcome := signals.ClassifyOutcomeAt(signals.OutcomeInput{
		IsAutomated: session.IsAutomated, MessageCount: session.MessageCount,
		EndedWithRole: lastRole, FinalFailureStreak: update.FinalFailureStreak,
		LastAssistantText: lastContent, LastActivity: lastActivity,
	}, now)
	q := update.QualitySignals
	score := signals.ComputeHealthScore(signals.ScoreInput{
		Outcome: outcome.Outcome, OutcomeConfidence: outcome.Confidence,
		HasToolCalls: update.HasToolCalls, FailureSignalCount: update.ToolFailureSignalCount,
		RetryCount: update.ToolRetryCount, EditChurnCount: update.EditChurnCount,
		ConsecutiveFailMax: update.ConsecutiveFailureMax, HasContextData: update.HasContextData,
		CompactionCount: update.CompactionCount, MidTaskCompactionCount: update.MidTaskCompactionCount,
		PressureMax: update.ContextPressureMax, Heuristics: signals.HeuristicSignals{
			ShortPromptCount: q.ShortPromptCount, UnstructuredStart: q.UnstructuredStart,
			MissingSuccessCriteriaCount: q.MissingSuccessCriteriaCount, MissingVerificationCount: q.MissingVerificationCount,
			DuplicatePromptCount: q.DuplicatePromptCount, NoCodeContextCount: q.NoCodeContextCount,
			RunawayToolLoopCount: q.RunawayToolLoopCount,
		},
	})
	update.Outcome, update.OutcomeConfidence = outcome.Outcome, outcome.Confidence
	update.SignalsPendingSince = nil
	if outcome.IsRecent {
		pending := now.UTC().Format(time.RFC3339)
		update.SignalsPendingSince = &pending
	}
	update.HealthScore, update.HealthGrade = score.Score, nil
	if score.Grade != "" {
		update.HealthGrade = &score.Grade
	}
	return update
}

// ExtractToolCallRows builds the signal analyzer input from normalized calls.
func ExtractToolCallRows(messages []db.Message) []signals.ToolCallRow {
	rows := make([]signals.ToolCallRow, 0)
	for _, message := range messages {
		for callIndex, call := range message.ToolCalls {
			status := ""
			if count := len(call.ResultEvents); count > 0 {
				status = call.ResultEvents[count-1].Status
			}
			rows = append(rows, signals.ToolCallRow{
				ToolName: call.ToolName, Category: call.Category,
				InputJSON: call.InputJSON, ResultContent: call.ResultContent,
				MessageOrdinal: message.Ordinal, CallIndex: callIndex,
				EventStatus: status,
			})
		}
	}
	return rows
}

// ComputeFinalStreak returns the trailing consecutive tool-failure count.
func ComputeFinalStreak(rows []signals.ToolCallRow) int {
	streak := 0
	for _, row := range slices.Backward(rows) {
		if !signals.IsFailure(row) {
			break
		}
		streak++
	}
	return streak
}

// ScanSecretsFromMessages detects secrets in normalized retained content.
func ScanSecretsFromMessages(
	_ db.Session, messages []db.Message,
	scan func(string) []secrets.Match,
) (findings []db.SecretFinding, definiteCount int) {
	findings = make([]db.SecretFinding, 0)
	add := func(
		sessionID, location string, ordinal int, callIndex, eventIndex *int,
		content string,
	) {
		secretScanBytes.Add(int64(len(content)))
		for _, match := range scan(content) {
			findings = append(findings, db.SecretFinding{
				SessionID: sessionID, RuleName: match.Rule,
				Confidence: match.Confidence, LocationKind: location,
				MessageOrdinal: ordinal, CallIndex: callIndex,
				EventIndex: eventIndex, MatchStart: match.Start,
				MatchEnd: match.End, MatchIndex: match.Index,
				RedactedMatch: match.Redacted,
				RulesVersion:  secrets.DefiniteRulesVersion(),
			})
			if match.Confidence == secrets.ConfidenceDefinite {
				definiteCount++
			}
		}
	}
	for _, message := range messages {
		add(message.SessionID, "message", message.Ordinal, nil, nil,
			message.Content)
		for callIndex := range message.ToolCalls {
			call := message.ToolCalls[callIndex]
			ci := callIndex
			add(message.SessionID, "tool_input", message.Ordinal, &ci, nil,
				call.InputJSON)
			if len(call.ResultEvents) > 0 {
				for eventIndex := range call.ResultEvents {
					ei := eventIndex
					add(message.SessionID, "tool_result_event", message.Ordinal,
						&ci, &ei, call.ResultEvents[eventIndex].Content)
				}
			} else {
				add(message.SessionID, "tool_result", message.Ordinal, &ci, nil,
					call.ResultContent)
			}
		}
	}
	return findings, definiteCount
}

// ExtractHeuristicMessages builds prompt-quality analyzer inputs.
func ExtractHeuristicMessages(messages []db.Message) []signals.HeuristicMessage {
	rows := make([]signals.HeuristicMessage, 0, len(messages))
	for _, message := range messages {
		rows = append(rows, signals.HeuristicMessage{
			Role: message.Role, SourceSubtype: message.SourceSubtype,
			Content: message.Content, IsSystem: message.IsSystem,
			Ordinal: message.Ordinal, Timestamp: message.Timestamp,
		})
	}
	return rows
}

// ExtractContextTokens returns assistant context measurements in order.
func ExtractContextTokens(messages []db.Message) []signals.ContextTokenRow {
	var rows []signals.ContextTokenRow
	for _, message := range messages {
		if message.Role == "assistant" {
			rows = append(rows, signals.ContextTokenRow{
				ContextTokens:    message.ContextTokens,
				HasContextTokens: message.HasContextTokens,
			})
		}
	}
	return rows
}

// ExtractCompactBoundaryOrdinals returns explicit compaction boundaries.
func ExtractCompactBoundaryOrdinals(messages []db.Message) []int {
	var ordinals []int
	for _, message := range messages {
		if message.IsCompactBoundary {
			ordinals = append(ordinals, message.Ordinal)
		}
	}
	return ordinals
}

// ExtractMostCommonModel returns the majority assistant model, with first-seen
// order breaking ties.
func ExtractMostCommonModel(messages []db.Message) string {
	counts := make(map[string]int)
	firstSeen := make(map[string]int)
	for index, message := range messages {
		if message.Role != "assistant" || message.Model == "" {
			continue
		}
		counts[message.Model]++
		if _, exists := firstSeen[message.Model]; !exists {
			firstSeen[message.Model] = index
		}
	}
	best := ""
	bestCount := -1
	for model, count := range counts {
		if count > bestCount ||
			(count == bestCount && firstSeen[model] < firstSeen[best]) {
			best, bestCount = model, count
		}
	}
	return best
}

// ExtractLastMessageRole returns the final visible transcript row.
func ExtractLastMessageRole(messages []db.Message) (role, content string) {
	for _, message := range slices.Backward(messages) {
		if !message.IsSystem && message.SourceSubtype != "tool_result" {
			return message.Role, message.Content
		}
	}
	return "", ""
}
