package rawderive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

// ErrParityGraphVersion marks a stored graph that cannot be compared under the
// run's preparation versions. It does not classify invalid run bindings.
var ErrParityGraphVersion = errors.New("unsupported parity graph version")

const (
	paritySessionBit uint16 = 1 << iota
	parityMessagesBit
	parityToolsBit
	parityUsageBit
	paritySignalsBit
	parityFindingsBit
	parityLinksBit
	parityExclusionsBit
)

func parityDigest(domain string, encode func(*parityEncoder)) (ParityDigest, error) {
	encoder := newParityEncoder(domain)
	encode(encoder)
	value, err := encoder.result()
	if err != nil {
		return ParityDigest{}, err
	}
	return ParityDigest(sha256.Sum256(value)), nil
}

// DigestParityBinding hashes the complete private run binding.
func DigestParityBinding(binding ParityBinding) (ParityDigest, error) {
	if err := validateParityBinding(binding); err != nil {
		return ParityDigest{}, err
	}
	return parityDigest("parity-binding", func(e *parityEncoder) {
		e.string(binding.Request.RunID)
		e.string(binding.Request.RuntimeID)
		e.string(binding.Request.BaselineProfile)
		e.string(binding.Request.Cohort.DeviceID)
		e.string(string(binding.Request.Cohort.Provider))
		e.string(binding.Request.Cohort.RootID)
		e.string(binding.BaselineID)
		e.digest(binding.BaselineConfig)
		e.string(binding.Tenant)
		e.digest(binding.Versions.ParserBuild)
		e.integer(binding.Versions.Data)
		e.string(binding.Versions.Preparation)
		e.string(binding.Versions.Projection)
		e.integer(binding.Versions.Comparison)
		e.integer(binding.Versions.Quality)
		e.string(binding.Versions.SecretRules)
		e.digest(binding.Versions.Policy)
		e.string(binding.ObservedAt.UTC().Format(time.RFC3339Nano))
	})
}

func validateParityBinding(binding ParityBinding) error {
	if binding.ObservedAt.IsZero() {
		return fmt.Errorf("invalid parity binding observation time")
	}
	if binding.Versions.Preparation != ParityPreparationVersion ||
		binding.Versions.Projection != ParityProjectionVersion ||
		binding.Versions.Comparison != ParitySchemaVersion {
		return fmt.Errorf("unsupported parity binding version")
	}
	return nil
}

func validateParityGraphBinding(binding ParityBinding, prepared ingest.PreparedSession) error {
	if prepared.Session.DataVersion != binding.Versions.Data {
		return fmt.Errorf("%w: data version mismatch", ErrParityGraphVersion)
	}
	if prepared.Signals.QualitySignals.Version != binding.Versions.Quality ||
		prepared.Session.QualitySignalVersion != binding.Versions.Quality {
		return fmt.Errorf("%w: quality version mismatch", ErrParityGraphVersion)
	}
	if prepared.Signals.SecretsRulesVersion != binding.Versions.SecretRules ||
		prepared.Session.SecretsRulesVersion != binding.Versions.SecretRules {
		return fmt.Errorf("%w: secret rules version mismatch", ErrParityGraphVersion)
	}
	for _, finding := range prepared.Findings {
		if finding.RulesVersion != binding.Versions.SecretRules {
			return fmt.Errorf("%w: finding rules version mismatch", ErrParityGraphVersion)
		}
	}
	return nil
}

// FingerprintParity builds the eight normalized category digests for one graph.
func FingerprintParity(
	ctx context.Context,
	binding ParityBinding,
	graph ParityGraph,
) (ParityFingerprint, error) {
	if err := validateParityBinding(binding); err != nil {
		return ParityFingerprint{}, err
	}
	if err := validateParityGraphBounds(graph); err != nil {
		return ParityFingerprint{}, err
	}
	if err := validateParityGraphBinding(binding, graph.Prepared); err != nil {
		return ParityFingerprint{}, err
	}
	if err := ctx.Err(); err != nil {
		return ParityFingerprint{}, err
	}
	prepared := cloneParityPrepared(graph.Prepared)
	prepared.Signals = ingest.RefreshSignalRecencyAt(
		prepared.Session, prepared.Messages, prepared.Signals, binding.ObservedAt,
	)
	ingest.ApplySignalFields(&prepared.Session, prepared.Signals)
	db.RestoreMessageResultContent(prepared.Messages)

	messageRows, toolRows, coordinates, err := encodeParityMessagesAndTools(ctx, prepared.Messages)
	if err != nil {
		return ParityFingerprint{}, err
	}
	if err = validateParityReferences(prepared, graph.Links, coordinates); err != nil {
		return ParityFingerprint{}, err
	}

	fingerprint := ParityFingerprint{}
	fingerprint.Session, err = parityDigest("parity-session", func(e *parityEncoder) {
		encodeParitySession(e, prepared.Session)
		e.boolean(graph.PromptEvidenceDiscarded)
	})
	if err != nil {
		return ParityFingerprint{}, err
	}
	fingerprint.Messages, err = parityDigest("parity-messages", func(e *parityEncoder) { e.records(messageRows) })
	if err != nil {
		return ParityFingerprint{}, err
	}
	fingerprint.Tools, err = parityDigest("parity-tools", func(e *parityEncoder) { e.records(toolRows) })
	if err != nil {
		return ParityFingerprint{}, err
	}
	fingerprint.Usage, err = encodeParityUsage(ctx, prepared.UsageEvents)
	if err != nil {
		return ParityFingerprint{}, err
	}
	fingerprint.Signals, err = parityDigest("parity-signals", func(e *parityEncoder) {
		encodeParitySignals(e, prepared.Signals)
	})
	if err != nil {
		return ParityFingerprint{}, err
	}
	fingerprint.Findings, err = encodeParityFindings(ctx, prepared.Findings)
	if err != nil {
		return ParityFingerprint{}, err
	}
	fingerprint.Links, err = encodeParityLinks(ctx, graph.Links)
	if err != nil {
		return ParityFingerprint{}, err
	}
	fingerprint.Exclusions, err = parityDigest("parity-exclusions", func(e *parityEncoder) {
		e.boolean(graph.Overlay.Excluded)
		e.boolean(graph.Overlay.SourceDeleted)
		e.boolean(graph.Overlay.ProviderExcluded)
	})
	if err != nil {
		return ParityFingerprint{}, err
	}
	fingerprint.Semantic, err = parityDigest("parity-semantic", func(e *parityEncoder) {
		e.digest(fingerprint.Session)
		e.digest(fingerprint.Messages)
		e.digest(fingerprint.Tools)
		e.digest(fingerprint.Usage)
		e.digest(fingerprint.Signals)
		e.digest(fingerprint.Findings)
		e.digest(fingerprint.Links)
		e.digest(fingerprint.Exclusions)
	})
	if err != nil {
		return ParityFingerprint{}, err
	}
	return fingerprint, ctx.Err()
}

// CompareParity compares category digests and reports their declaration-order bits.
func CompareParity(expected, candidate ParityFingerprint) ParityComparison {
	pairs := [][2]ParityDigest{
		{expected.Session, candidate.Session},
		{expected.Messages, candidate.Messages},
		{expected.Tools, candidate.Tools},
		{expected.Usage, candidate.Usage},
		{expected.Signals, candidate.Signals},
		{expected.Findings, candidate.Findings},
		{expected.Links, candidate.Links},
		{expected.Exclusions, candidate.Exclusions},
	}
	comparison := ParityComparison{Verdict: ParityMatched}
	for index, pair := range pairs {
		if pair[0] != pair[1] {
			comparison.Different |= 1 << index
		}
	}
	if comparison.Different != 0 || expected.Semantic != candidate.Semantic {
		comparison.Verdict = ParityMismatched
	}
	return comparison
}

func encodeParitySession(e *parityEncoder, session db.Session) {
	e.string(session.Project)
	e.string(session.Agent)
	e.string(session.AgentLabel)
	e.string(session.Entrypoint)
	e.string(session.SessionKind)
	e.nullableString(session.FirstMessage)
	e.nullableString(session.SessionName)
	e.nullableTimestamp(session.StartedAt)
	e.nullableTimestamp(session.EndedAt)
	e.integer(session.MessageCount)
	e.integer(session.UserMessageCount)
	e.nullableString(session.ParserParentSessionID)
	e.string(session.RelationshipType)
	e.integer(session.TotalOutputTokens)
	e.integer(session.PeakContextTokens)
	e.boolean(session.HasTotalOutputTokens)
	e.boolean(session.HasPeakContextTokens)
	e.boolean(session.IsAutomated)
	e.string(session.Cwd)
	e.string(session.GitBranch)
	e.string(session.SourceSessionID)
	e.string(session.SourceVersion)
	e.string(session.TranscriptFidelity)
	e.integer(session.ParserMalformedLines)
	e.boolean(session.IsTruncated)
	e.nullableString(session.TerminationStatus)
}

type parityCoordinates struct {
	messages map[int]struct{}
	calls    map[[2]int]struct{}
	events   map[[3]int]struct{}
}

func encodeParityMessagesAndTools(
	ctx context.Context,
	messages []db.Message,
) ([][]byte, [][]byte, parityCoordinates, error) {
	ordered := append([]db.Message(nil), messages...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Ordinal < ordered[j].Ordinal })
	coordinates := parityCoordinates{messages: make(map[int]struct{}), calls: make(map[[2]int]struct{}), events: make(map[[3]int]struct{})}
	messageRows := make([][]byte, 0, len(ordered))
	toolRows := make([][]byte, 0)
	for _, message := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, nil, coordinates, err
		}
		if _, duplicate := coordinates.messages[message.Ordinal]; duplicate {
			return nil, nil, coordinates, fmt.Errorf("duplicate parity message coordinate")
		}
		coordinates.messages[message.Ordinal] = struct{}{}
		record := newParityRecord()
		record.integer(message.Ordinal)
		record.string(message.Role)
		record.string(message.Content)
		record.string(message.ThinkingText)
		record.timestamp(message.Timestamp)
		record.boolean(message.HasThinking)
		record.boolean(message.HasToolUse)
		record.integer(message.ContentLength)
		record.boolean(message.IsSystem)
		record.string(message.Model)
		record.string(message.ReasoningEffort)
		record.string(message.ProviderID)
		record.json(ctx, message.TokenUsage)
		record.integer(message.ContextTokens)
		record.integer(message.OutputTokens)
		record.boolean(message.HasContextTokens)
		record.boolean(message.HasOutputTokens)
		record.string(message.ClaudeMessageID)
		record.string(message.ClaudeRequestID)
		record.string(message.SourceType)
		record.string(message.SourceSubtype)
		record.string(message.PromptSource)
		record.string(message.SourceUUID)
		record.string(message.SourceParentUUID)
		record.boolean(message.IsSidechain)
		record.boolean(message.IsCompactBoundary)
		encoded, err := record.result()
		if err != nil {
			return nil, nil, coordinates, err
		}
		messageRows = append(messageRows, encoded)

		calls := append([]db.ToolCall(nil), message.ToolCalls...)
		sort.Slice(calls, func(i, j int) bool { return calls[i].CallIndex < calls[j].CallIndex })
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				return nil, nil, coordinates, err
			}
			coordinate := [2]int{message.Ordinal, call.CallIndex}
			if _, duplicate := coordinates.calls[coordinate]; duplicate {
				return nil, nil, coordinates, fmt.Errorf("duplicate parity call coordinate")
			}
			coordinates.calls[coordinate] = struct{}{}
			callRecord := newParityRecord()
			callRecord.integer(message.Ordinal)
			callRecord.integer(call.CallIndex)
			callRecord.string(call.ToolName)
			callRecord.string(call.Category)
			callRecord.string(call.ToolUseID)
			callRecord.json(ctx, []byte(call.InputJSON))
			callRecord.string(call.FilePath)
			callRecord.string(call.SkillName)
			callRecord.integer(call.ResultContentLength)
			callRecord.string(call.ResultContent)

			events := append([]db.ToolResultEvent(nil), call.ResultEvents...)
			sort.Slice(events, func(i, j int) bool { return events[i].EventIndex < events[j].EventIndex })
			callRecord.rawUint32(len(events))
			for _, event := range events {
				if err := ctx.Err(); err != nil {
					return nil, nil, coordinates, err
				}
				eventCoordinate := [3]int{message.Ordinal, call.CallIndex, event.EventIndex}
				if _, duplicate := coordinates.events[eventCoordinate]; duplicate {
					return nil, nil, coordinates, fmt.Errorf("duplicate parity event coordinate")
				}
				coordinates.events[eventCoordinate] = struct{}{}
				eventRecord := newParityRecord()
				eventRecord.integer(event.EventIndex)
				eventRecord.string(event.ToolUseID)
				eventRecord.string(event.AgentID)
				eventRecord.string(event.Source)
				eventRecord.string(event.Status)
				eventRecord.string(event.Content)
				eventRecord.integer(event.ContentLength)
				eventRecord.timestamp(event.Timestamp)
				encodedEvent, err := eventRecord.result()
				if err != nil {
					return nil, nil, coordinates, err
				}
				callRecord.record(encodedEvent)
			}
			encodedCall, err := callRecord.result()
			if err != nil {
				return nil, nil, coordinates, err
			}
			toolRows = append(toolRows, encodedCall)
		}
	}
	return messageRows, toolRows, coordinates, nil
}

func encodeParityUsage(ctx context.Context, events []db.UsageEvent) (ParityDigest, error) {
	rows := make([][]byte, 0, len(events))
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return ParityDigest{}, err
		}
		record := newParityRecord()
		record.nullableInt(event.MessageOrdinal)
		record.string(event.Source)
		record.string(event.Model)
		record.string(event.ProviderID)
		record.integer(event.InputTokens)
		record.integer(event.OutputTokens)
		record.integer(event.CacheCreationInputTokens)
		record.integer(event.CacheReadInputTokens)
		record.integer(event.ReasoningTokens)
		var cost *int64
		if event.Cost != nil {
			cost = &event.Cost.Microdollars
		}
		record.nullableMoney(cost)
		record.string(event.CostStatus)
		record.string(event.CostSource)
		record.timestamp(event.OccurredAt)
		record.string(event.DedupKey)
		encoded, err := record.result()
		if err != nil {
			return ParityDigest{}, err
		}
		rows = append(rows, encoded)
	}
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i], rows[j]) < 0 })
	return parityDigest("parity-usage", func(e *parityEncoder) { e.records(rows) })
}

func encodeParitySignals(e *parityEncoder, signals db.SessionSignalUpdate) {
	e.integer(signals.ToolFailureSignalCount)
	e.integer(signals.ToolRetryCount)
	e.integer(signals.EditChurnCount)
	e.integer(signals.ConsecutiveFailureMax)
	e.string(signals.Outcome)
	e.string(signals.OutcomeConfidence)
	e.string(signals.EndedWithRole)
	e.integer(signals.FinalFailureStreak)
	e.integer(signals.CompactionCount)
	e.integer(signals.MidTaskCompactionCount)
	e.nullableFloat(signals.ContextPressureMax)
	e.nullableInt(signals.HealthScore)
	e.nullableString(signals.HealthGrade)
	e.boolean(signals.HasToolCalls)
	e.boolean(signals.HasContextData)
	e.integer(signals.SecretLeakCount)
	e.integer(signals.QualitySignals.ShortPromptCount)
	e.boolean(signals.QualitySignals.UnstructuredStart)
	e.integer(signals.QualitySignals.MissingSuccessCriteriaCount)
	e.integer(signals.QualitySignals.MissingVerificationCount)
	e.integer(signals.QualitySignals.DuplicatePromptCount)
	e.integer(signals.QualitySignals.NoCodeContextCount)
	e.integer(signals.QualitySignals.RunawayToolLoopCount)
}

func encodeParityFindings(ctx context.Context, findings []db.SecretFinding) (ParityDigest, error) {
	rows := make([][]byte, 0, len(findings))
	for _, finding := range findings {
		if err := ctx.Err(); err != nil {
			return ParityDigest{}, err
		}
		record := newParityRecord()
		record.string(finding.RuleName)
		record.string(finding.Confidence)
		record.string(finding.LocationKind)
		record.integer(finding.MessageOrdinal)
		record.nullableInt(finding.CallIndex)
		record.nullableInt(finding.EventIndex)
		record.integer(finding.MatchStart)
		record.integer(finding.MatchEnd)
		record.integer(finding.MatchIndex)
		record.string(finding.RedactedMatch)
		encoded, err := record.result()
		if err != nil {
			return ParityDigest{}, err
		}
		rows = append(rows, encoded)
	}
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i], rows[j]) < 0 })
	return parityDigest("parity-findings", func(e *parityEncoder) { e.records(rows) })
}

func encodeParityLinks(ctx context.Context, links []ParityLink) (ParityDigest, error) {
	rows := make([][]byte, 0, len(links))
	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return ParityDigest{}, err
		}
		record := newParityRecord()
		record.string(link.Kind)
		record.integer(link.Ordinal)
		record.integer(link.CallIndex)
		record.integer(link.EventIndex)
		record.string(link.Target.SourceID)
		record.string(link.Target.LogicalKey)
		record.string(link.Target.Kind)
		record.string(link.Unresolved)
		encoded, err := record.result()
		if err != nil {
			return ParityDigest{}, err
		}
		rows = append(rows, encoded)
	}
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i], rows[j]) < 0 })
	return parityDigest("parity-links", func(e *parityEncoder) { e.records(rows) })
}

func validateParityReferences(
	prepared ingest.PreparedSession,
	links []ParityLink,
	coordinates parityCoordinates,
) error {
	sessionID := prepared.Session.ID
	for _, message := range prepared.Messages {
		if message.SessionID != sessionID {
			return fmt.Errorf("invalid parity message ownership")
		}
		for _, call := range message.ToolCalls {
			if call.SessionID != sessionID {
				return fmt.Errorf("invalid parity call ownership")
			}
		}
	}
	for _, usage := range prepared.UsageEvents {
		if usage.SessionID != sessionID {
			return fmt.Errorf("invalid parity usage ownership")
		}
		if usage.MessageOrdinal != nil {
			if _, ok := coordinates.messages[*usage.MessageOrdinal]; !ok {
				return fmt.Errorf("dangling parity usage coordinate")
			}
		}
	}
	for _, finding := range prepared.Findings {
		if finding.SessionID != sessionID {
			return fmt.Errorf("invalid parity finding ownership")
		}
		if _, ok := coordinates.messages[finding.MessageOrdinal]; !ok {
			return fmt.Errorf("dangling parity finding message coordinate")
		}
		if finding.CallIndex != nil {
			call := [2]int{finding.MessageOrdinal, *finding.CallIndex}
			if _, ok := coordinates.calls[call]; !ok {
				return fmt.Errorf("dangling parity finding call coordinate")
			}
		}
		if finding.EventIndex != nil {
			if finding.CallIndex == nil {
				return fmt.Errorf("dangling parity finding event coordinate")
			}
			event := [3]int{finding.MessageOrdinal, *finding.CallIndex, *finding.EventIndex}
			if _, ok := coordinates.events[event]; !ok {
				return fmt.Errorf("dangling parity finding event coordinate")
			}
		}
	}
	for _, link := range links {
		switch link.Kind {
		case "call":
			if _, ok := coordinates.calls[[2]int{link.Ordinal, link.CallIndex}]; !ok {
				return fmt.Errorf("dangling parity call link")
			}
		case "event":
			if _, ok := coordinates.events[[3]int{link.Ordinal, link.CallIndex, link.EventIndex}]; !ok {
				return fmt.Errorf("dangling parity event link")
			}
		}
	}
	return nil
}

func sharesParityStringStorage(a, b string) bool {
	return len(a) == len(b) && (len(a) == 0 || unsafe.StringData(a) == unsafe.StringData(b))
}

func validateParityGraphBounds(graph ParityGraph) error {
	rows := len(graph.Prepared.Messages) + len(graph.Prepared.UsageEvents) +
		len(graph.Prepared.Findings) + len(graph.Links)
	bytesRetained := 0
	add := func(values ...string) error {
		for _, value := range values {
			if len(value) > maxParityGraphBytes-bytesRetained {
				return errParityLimit
			}
			bytesRetained += len(value)
		}
		return nil
	}
	s := graph.Prepared.Session
	if err := add(
		graph.Key.SourceID, graph.Key.LogicalKey, graph.Key.Kind,
		s.ID, s.Project, s.Machine, s.Agent, s.AgentLabel, s.Entrypoint,
		s.SessionKind, s.RelationshipType, s.Outcome, s.OutcomeConfidence,
		s.EndedWithRole, s.SecretsRulesVersion, s.Cwd, s.GitBranch,
		s.SourceSessionID, s.SourceVersion, s.TranscriptFidelity, s.CreatedAt,
	); err != nil {
		return err
	}
	for _, parent := range s.ParentSessionIDs {
		if err := add(parent); err != nil {
			return err
		}
	}
	for _, pointer := range []*string{
		s.FirstMessage, s.DisplayName, s.SessionName, s.StartedAt, s.EndedAt,
		s.ParentSessionID, s.ParserParentSessionID, s.SignalsPendingSince,
		s.HealthGrade, s.DeletedAt, s.DeletionCause, s.SourceMissingAt,
		s.TerminationStatus, s.FilePath, s.LastEntryUUID, s.FileHash,
		s.LocalModifiedAt, s.TranscriptRevision,
	} {
		if pointer != nil {
			if err := add(*pointer); err != nil {
				return err
			}
		}
	}
	for _, message := range graph.Prepared.Messages {
		messageSessionID := message.SessionID
		if sharesParityStringStorage(messageSessionID, s.ID) {
			messageSessionID = ""
		}
		if err := add(messageSessionID, message.Role, message.Content, message.ThinkingText, message.Timestamp, message.Model, message.ReasoningEffort, message.ProviderID, string(message.TokenUsage), message.ClaudeMessageID, message.ClaudeRequestID, message.SourceType, message.SourceSubtype, message.PromptSource, message.SourceUUID, message.SourceParentUUID); err != nil {
			return err
		}
		rows += len(message.ToolCalls) + len(message.ToolResults)
		for _, result := range message.ToolResults {
			if err := add(result.ToolUseID, result.ContentRaw); err != nil {
				return err
			}
		}
		for _, call := range message.ToolCalls {
			callSessionID := call.SessionID
			if sharesParityStringStorage(callSessionID, s.ID) {
				callSessionID = ""
			}
			if err := add(callSessionID, call.ToolName, call.Category, call.ToolUseID, call.InputJSON, call.FilePath, call.SkillName, call.ResultContent, call.SubagentSessionID, call.Rendering); err != nil {
				return err
			}
			rows += len(call.ResultEvents)
			for _, event := range call.ResultEvents {
				if err := add(event.ToolUseID, event.AgentID, event.SubagentSessionID, event.Source, event.Status, event.Content, event.Timestamp, string(event.RawContentDigest)); err != nil {
					return err
				}
			}
		}
	}
	for _, event := range graph.Prepared.UsageEvents {
		eventSessionID := event.SessionID
		if sharesParityStringStorage(eventSessionID, s.ID) {
			eventSessionID = ""
		}
		if err := add(eventSessionID, event.Source, event.Model, event.ProviderID, event.CostStatus, event.CostSource, event.OccurredAt, event.DedupKey); err != nil {
			return err
		}
	}
	for _, finding := range graph.Prepared.Findings {
		findingSessionID := finding.SessionID
		if sharesParityStringStorage(findingSessionID, s.ID) {
			findingSessionID = ""
		}
		if err := add(findingSessionID, finding.RuleName, finding.Confidence, finding.LocationKind, finding.RedactedMatch, finding.RulesVersion); err != nil {
			return err
		}
	}
	for _, link := range graph.Links {
		if err := add(link.Kind, link.Target.SourceID, link.Target.LogicalKey, link.Target.Kind, link.Unresolved); err != nil {
			return err
		}
	}
	if graph.Prepared.Signals.FullState != nil {
		fullStateSessionID := graph.Prepared.Signals.FullState.SessionID
		if sharesParityStringStorage(fullStateSessionID, s.ID) {
			fullStateSessionID = ""
		}
		if err := add(
			fullStateSessionID,
			string(graph.Prepared.Signals.FullState.State),
			graph.Prepared.Signals.FullState.TranscriptRevision,
			graph.Prepared.Signals.FullState.UpdatedAt,
		); err != nil {
			return err
		}
	}
	if graph.Prepared.Signals.SignalsPendingSince != nil {
		if err := add(*graph.Prepared.Signals.SignalsPendingSince); err != nil {
			return err
		}
	}
	if graph.Prepared.Signals.HealthGrade != nil {
		if err := add(*graph.Prepared.Signals.HealthGrade); err != nil {
			return err
		}
	}
	if err := add(
		graph.Prepared.Signals.Outcome,
		graph.Prepared.Signals.OutcomeConfidence,
		graph.Prepared.Signals.EndedWithRole,
		graph.Prepared.Signals.SecretsRulesVersion,
	); err != nil {
		return err
	}
	if rows > maxParityChildRows {
		return errParityLimit
	}
	return nil
}
