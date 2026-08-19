package sync

import (
	"context"
	"fmt"
	"maps"
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
	// messageUsageUpdates carries token metadata attached to assistant
	// messages committed before this incremental batch. These updates are
	// ordered before appended messages in the canonical transcript.
	messageUsageUpdates []db.MessageTokenUsageUpdate
	// preWriteRevision is the transcript revision before this write's
	// bump; the persisted state token must match it.
	preWriteRevision string

	// preWriteSecretsVersion is the session's secrets rules version
	// before this write. The incremental write transaction blanks the
	// session's secrets_rules_version when it bumps the transcript
	// revision (the recorded scan no longer covers the new rows), so the
	// maintainer must compare the pre-write value against the current
	// definite version instead of reading the blanked session row.
	preWriteSecretsVersion string

	// qualitySignalVersion and secretsRulesVersion are the current
	// detector versions. Maintenance is only valid when the persisted
	// state and the pre-write session row are both at these versions;
	// otherwise a stale-but-self-consistent session would be folded with
	// newly-added rules and stamped current without rescanning history.
	qualitySignalVersion int
	secretsRulesVersion  string
}

func (e *Engine) newIncrementalSignalMaintainer(
	inc *incrementalUpdate,
	appended []db.Message,
	resultUpdates []db.ToolCallResultUpdate,
	messageUsageUpdates []db.MessageTokenUsageUpdate,
	preWriteRevision, preWriteSecretsVersion string,
) db.SignalMaintainer {
	return &incrementalSignalMaintainer{
		engine:                 e,
		sessionID:              inc.sessionID,
		appended:               appended,
		resultUpdates:          resultUpdates,
		messageUsageUpdates:    messageUsageUpdates,
		preWriteRevision:       preWriteRevision,
		preWriteSecretsVersion: preWriteSecretsVersion,
		qualitySignalVersion:   db.CurrentQualitySignalVersion,
		secretsRulesVersion:    secrets.DefiniteRulesVersion(),
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
	// The persisted state and the pre-write session row must both be at
	// the current quality and secrets rules versions. Requiring the
	// state's signal version to equal the current version (not merely the
	// session's) and the pre-write secrets version to equal the current
	// definite version prevents a rules-version upgrade from folding a
	// stale-but-equal session and stamping it current without rescanning
	// history. An empty pre-write secrets version means the row was never
	// stamped, which also reseeds.
	if stored.TranscriptRevision != m.preWriteRevision ||
		stored.SignalVersion != m.qualitySignalVersion ||
		sess.QualitySignalVersion != m.qualitySignalVersion ||
		m.preWriteSecretsVersion != m.secretsRulesVersion {
		return nil, nil // state fell behind the rows: reseed
	}

	// Appended tool-call rows in the same shape the full compute uses.
	appendedRows := extractToolCallRows(m.appended)

	// Modified facts for late result updates, resolved in-transaction so
	// the fold sees the post-update stored facts.
	modified := make(map[signals.CallPos]signals.ToolFact)
	deleteKeys := make([]db.FindingDeleteKey, 0, len(m.resultUpdates))
	var insertFindings []db.SecretFinding
	positions := make([]db.ToolCallPosition, 0, len(m.resultUpdates))
	for _, u := range m.resultUpdates {
		if u.ToolUseID != "" {
			positions = append(positions, u.Position)
		}
	}
	callFacts, err := q.ToolCallsByPosition(ctx, positions)
	if err != nil {
		return nil, err
	}
	factByPosition := make(
		map[db.ToolCallPosition]db.ToolCallSignalFact, len(callFacts),
	)
	for _, f := range callFacts {
		factByPosition[db.ToolCallPosition{
			MessageOrdinal: f.MessageOrdinal,
			CallIndex:      f.CallIndex,
		}] = f
	}
	for _, u := range m.resultUpdates {
		fact, ok := factByPosition[u.Position]
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

		// Only the events this transaction inserted need scanning:
		// previously stored events already carry findings, and rescanning
		// the call's whole history makes repeated late outputs quadratic.
		// When any event exists the full compute scans events instead of
		// the result_content summary, so the summary-derived finding must
		// go and the inserted events are scanned with their real indexes.
		events := q.InsertedResultEvents(u.Position)
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
	lastTokensOrdinal := nextState.LastValidTokensOrdinal
	appendedHasContextData := false

	// A resumed Codex tail may carry token_count for the final assistant
	// message committed before the checkpoint. That message is already
	// represented in the compact state, but its token-derived contribution
	// is not. Apply the usage update before later appended messages so the
	// compaction detector sees canonical message order without loading the
	// historical transcript.
	for _, usage := range m.messageUsageUpdates {
		if !q.MessageTokenUsageUpdated(usage.Ordinal) ||
			!usage.HasContextTokens {
			continue
		}
		if lastTokens > 0 && usage.Ordinal <= lastTokensOrdinal {
			// A later assistant already contributed a newer measurement
			// than the one this late update targets: applying it here
			// would fold tokens out of chronological order and corrupt
			// compaction detection and the tail token value. Decline and
			// let the caller fall back to a full recompute.
			return nil, nil
		}
		appendedHasContextData = true
		if lastTokens > 0 &&
			float64(usage.ContextTokens) < 0.7*float64(lastTokens) {
			compactionDelta++
		}
		lastTokens = usage.ContextTokens
		lastTokensOrdinal = usage.Ordinal
	}
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
			lastTokensOrdinal = msg.Ordinal
		}
	}
	nextState.LastRole = lastRole
	nextState.LastContent = lastContent
	nextState.MsgIndex = msgIndex
	nextState.ModelCounts = modelCounts
	nextState.ModelFirstSeen = modelFirstSeen
	nextState.LastValidTokens = lastTokens
	nextState.LastValidTokensOrdinal = lastTokensOrdinal

	hasToolCalls := sess.HasToolCalls || len(appendedRows) > 0
	hasContextData := sess.HasContextData || appendedHasContextData
	noCodeContext := sess.NoCodeContextCount
	if noCodeContext > 0 &&
		slices.ContainsFunc(appendedRows, signals.IsContextToolCall) {
		noCodeContext = 0
	}
	// When the session has explicit compact boundaries the full compute
	// derives the compaction count from the boundary count and ignores
	// token-drop compactions; the fold must match by not adding the
	// token-drop delta on top.
	compactionCount := sess.CompactionCount
	if !nextState.HasExplicitBoundaries {
		compactionCount += compactionDelta
	}
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
	out := maps.Clone(in)
	if out == nil {
		out = make(map[string]int)
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
// after a synchronous full-content write so later incremental deltas can fold.
// Callers run under the engine's sync serialization; the conditional database
// write still refuses publication if another writer advances the transcript
// after the revision is captured.
func (e *Engine) seedSignalStateFromFull(
	sessionID string, msgs []db.Message,
) error {
	return e.seedSignalStateFromRows(
		sessionID, msgs, extractToolCallRows(msgs),
	)
}

// seedSignalStateFromFullWithContentFailures is the staged streaming
// variant: the seeded incremental state must see the same pre-computed
// content-failure verdicts the signal pass used, or later deltas fold
// against a different failure history.
func (e *Engine) seedSignalStateFromFullWithContentFailures(
	sessionID string, msgs []db.Message, failures map[string]bool,
) error {
	toolRows := extractToolCallRows(msgs)
	patchToolCallRowsWithContentFailures(toolRows, msgs, failures)
	return e.seedSignalStateFromRows(sessionID, msgs, toolRows)
}

func (e *Engine) seedSignalStateFromRows(
	sessionID string, msgs []db.Message, toolRows []signals.ToolCallRow,
) error {
	rev, err := e.db.TranscriptRevision(sessionID)
	if err != nil {
		return err
	}
	state, err := buildSignalStateFromRows(sessionID, msgs, toolRows, rev)
	if err != nil {
		return err
	}
	_, err = e.db.UpsertSessionSignalStateIfRevision(state)
	return err
}

// buildSignalStateFromRows is the pure full-snapshot counterpart of the
// incremental fold. revision must be captured before the rows represented by
// msgs are loaded; callers that read from the database then publish through
// ReplaceSessionSignalsIfRevision so a concurrent transcript change cannot
// make stale aggregates appear current.
func buildSignalStateFromRows(
	sessionID string,
	msgs []db.Message,
	toolRows []signals.ToolCallRow,
	revision string,
) (db.SessionSignalState, error) {
	lastRole, lastContent := extractLastMessageRole(msgs)
	counts, firstSeen, msgIndex := extractModelCounts(msgs)
	lastTokens, lastTokensOrdinal := 0, 0
	for _, m := range slices.Backward(msgs) {
		if m.Role == "assistant" && m.HasContextTokens {
			lastTokens = m.ContextTokens
			lastTokensOrdinal = m.Ordinal
			break
		}
	}
	state := signals.SeedIncrementalState(
		toolRows,
		extractCompactBoundaryOrdinals(msgs),
		lastRole, lastContent,
		counts, firstSeen, msgIndex, lastTokens, lastTokensOrdinal,
	)
	blob, err := state.MarshalBinary()
	if err != nil {
		return db.SessionSignalState{}, fmt.Errorf(
			"encoding signal state %s: %w", sessionID, err,
		)
	}
	return db.SessionSignalState{
		SessionID:          sessionID,
		State:              blob,
		TranscriptRevision: revision,
		SignalVersion:      db.CurrentQualitySignalVersion,
	}, nil
}
