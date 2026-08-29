package artifact

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type importClosureOutcome uint8

const (
	importClosureComplete importClosureOutcome = iota
	importClosureDeferred
	importClosureInvalid
)

func loadImportedSession(
	ctx context.Context,
	_ *db.DB,
	store ArtifactStore,
	origin string,
	gid string,
	manifestHash string,
	limits artifactLimits,
) (db.SessionBatchWrite, importClosureOutcome, error) {
	if !strings.HasPrefix(gid, origin+"~") || len(gid) == len(origin)+1 {
		return db.SessionBatchWrite{}, importClosureInvalid, nil
	}
	if err := validateHashHex(manifestHash); err != nil {
		return db.SessionBatchWrite{}, importClosureInvalid, nil
	}
	manifestRef, err := NewRef(
		origin, KindManifests, manifestHash+".json",
	)
	if err != nil {
		return db.SessionBatchWrite{}, importClosureInvalid, nil
	}
	manifestEntry, found, err := statImportDependency(ctx, store, manifestRef)
	if err != nil {
		if isInvalidImportDependencyError(err) {
			return quarantineImportDependency(
				ctx, store, manifestRef, "invalid import manifest",
			)
		}
		return db.SessionBatchWrite{}, importClosureDeferred, err
	}
	if !found {
		return db.SessionBatchWrite{}, importClosureDeferred, nil
	}
	manifestData, err := readVerifiedImportArtifact(
		ctx, store, manifestEntry, manifestDecodedLimit,
	)
	if err != nil {
		if isInvalidImportDependencyError(err) {
			return quarantineImportDependency(
				ctx, store, manifestRef, "invalid import manifest",
			)
		}
		return db.SessionBatchWrite{}, importClosureDeferred, err
	}
	m, err := decodeManifestWithLimits(manifestData, limits)
	if err != nil {
		if errors.Is(err, errFutureArtifactVersion) {
			return db.SessionBatchWrite{}, importClosureDeferred, err
		}
		return quarantineImportDependency(
			ctx, store, manifestRef, "invalid import manifest",
		)
	}
	if err := validateImportedManifest(m, origin, gid, limits); err != nil {
		return quarantineImportDependency(
			ctx, store, manifestRef, "invalid import manifest",
		)
	}

	messages := make([]db.Message, 0, min(limits.sessionMessages, 64))
	var decodedBytes int64
	nested := nestedCollectionCounts{}
	for _, segmentHash := range m.Segments {
		segmentRef, err := NewRef(
			origin, KindSegments, segmentHash+".ndjson",
		)
		if err != nil {
			return quarantineImportDependency(
				ctx, store, manifestRef, "invalid import segment reference",
			)
		}
		segmentEntry, found, err := statImportDependency(ctx, store, segmentRef)
		if err != nil {
			if isInvalidImportDependencyError(err) {
				return quarantineImportDependency(
					ctx, store, segmentRef, "invalid import segment",
				)
			}
			return db.SessionBatchWrite{}, importClosureDeferred, err
		}
		if !found {
			return db.SessionBatchWrite{}, importClosureDeferred, nil
		}
		segmentData, err := readVerifiedImportArtifact(
			ctx, store, segmentEntry, segmentDecodedLimit,
		)
		if err != nil {
			if isInvalidImportDependencyError(err) {
				return quarantineImportDependency(
					ctx, store, segmentRef, "invalid import segment",
				)
			}
			return db.SessionBatchWrite{}, importClosureDeferred, err
		}
		preflight, err := preflightSegmentData(segmentData, limits)
		if err != nil {
			if errors.Is(err, errFutureArtifactVersion) {
				return db.SessionBatchWrite{}, importClosureDeferred, err
			}
			return quarantineImportDependency(
				ctx, store, segmentRef, "invalid import segment",
			)
		}
		if int64(len(segmentData)) > limits.sessionDecodedBytes-decodedBytes ||
			exceedsCollectionLimit(
				len(messages), len(preflight.records), limits.sessionMessages,
			) ||
			exceedsCollectionLimit(
				nested.toolCalls,
				preflight.nested.toolCalls,
				limits.sessionToolCalls,
			) ||
			exceedsCollectionLimit(
				nested.resultEvents,
				preflight.nested.resultEvents,
				limits.sessionResultEvents,
			) {
			return quarantineImportDependency(
				ctx, store, manifestRef, "import session aggregate exceeds limits",
			)
		}
		segmentMessages, err := decodePreflightedSegment(preflight)
		if err != nil {
			return quarantineImportDependency(
				ctx, store, segmentRef, "invalid import segment",
			)
		}
		messages = append(messages, segmentMessages...)
		decodedBytes += int64(len(segmentData))
		nested.toolCalls += preflight.nested.toolCalls
		nested.resultEvents += preflight.nested.resultEvents
	}
	if err := validateImportedClosure(m, messages); err != nil {
		return quarantineImportDependency(
			ctx, store, manifestRef, "invalid import session closure",
		)
	}
	return rewriteManifestForImport(m, messages), importClosureComplete, nil
}

func validateImportedManifest(
	m manifest, origin, gid string, limits artifactLimits,
) error {
	if m.Version < manifestMinDecodeVersion || m.Version > manifestFormatVersion {
		return errors.New("manifest version is unsupported")
	}
	if m.Origin != origin {
		return errors.New("manifest origin is invalid")
	}
	if m.NativeSessionID == "" || strings.Contains(m.NativeSessionID, "~") {
		return errors.New("manifest native session ID is invalid")
	}
	if gid != origin+"~"+m.NativeSessionID {
		return errors.New("manifest global session ID is invalid")
	}
	if m.Session.ID != m.NativeSessionID {
		return errors.New("manifest session ID is invalid")
	}
	if m.Session.Machine != origin {
		return errors.New("manifest session machine is invalid")
	}
	if len(m.Segments) == 0 || len(m.Segments) > limits.manifestSegments {
		return errors.New("manifest segment references are invalid")
	}
	seen := make(map[string]struct{}, len(m.Segments))
	for _, segmentHash := range m.Segments {
		if err := validateHashHex(segmentHash); err != nil {
			return err
		}
		if _, exists := seen[segmentHash]; exists {
			return errors.New("manifest has duplicate segment reference")
		}
		seen[segmentHash] = struct{}{}
	}
	if len(m.UsageEvents) > limits.manifestUsageEvents {
		return errors.New("manifest usage event limit exceeded")
	}
	return ValidateRawSource(m.RawSource)
}

func validateImportedClosure(m manifest, messages []db.Message) error {
	if m.Session.MessageCount != len(messages) {
		return errors.New("manifest message count does not match segment messages")
	}
	if m.Session.UserMessageCount < 0 ||
		m.Session.UserMessageCount > m.Session.MessageCount {
		return errors.New("manifest user message count is invalid")
	}
	ordinals := make(map[int]struct{}, len(messages))
	userMessageCount := 0
	for _, message := range messages {
		if _, exists := ordinals[message.Ordinal]; exists {
			return errors.New("session closure has duplicate message ordinal")
		}
		ordinals[message.Ordinal] = struct{}{}
		if message.Role == "user" && !message.IsSystem {
			userMessageCount++
		}
	}
	if m.Session.UserMessageCount != userMessageCount {
		return errors.New(
			"manifest user message count does not match segment messages",
		)
	}
	usageKeys := make(map[string]struct{}, len(m.UsageEvents))
	for _, event := range m.UsageEvents {
		if event.DedupKey == "" {
			continue
		}
		key := event.Source + "\x00" + event.DedupKey
		if _, exists := usageKeys[key]; exists {
			return errors.New("session closure has duplicate usage event key")
		}
		usageKeys[key] = struct{}{}
	}
	return nil
}

func statImportDependency(
	ctx context.Context, store ArtifactStore, ref Ref,
) (Entry, bool, error) {
	entry, err := store.Stat(ctx, ref)
	if errors.Is(err, ErrArtifactNotFound) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	return entry, true, nil
}

func isInvalidImportDependencyError(err error) bool {
	return errors.Is(err, ErrArtifactInvalid) ||
		errors.Is(err, ErrArtifactCorrupt)
}

func quarantineImportDependency(
	ctx context.Context, store ArtifactStore, ref Ref, reason string,
) (db.SessionBatchWrite, importClosureOutcome, error) {
	if err := store.Quarantine(ctx, ref, reason); err != nil {
		return db.SessionBatchWrite{}, importClosureDeferred, err
	}
	return db.SessionBatchWrite{}, importClosureInvalid, nil
}

func rewriteManifestForImport(
	m manifest, messages []db.Message,
) db.SessionBatchWrite {
	importedID := m.Origin + "~" + m.NativeSessionID
	session := m.Session.dbSession()
	session.ID = importedID
	session.Machine = m.Origin
	session.SessionName = m.SessionName
	session.DeletedAt = nil
	session.DeletionCause = nil
	clearImportedSessionSourceState(&session)
	session.HasToolCalls = m.SessionHasToolCalls
	session.HasContextData = m.SessionHasContextData
	session.ApplyQualitySignals(m.SessionQualitySignals.dbQualitySignals())
	session.SecretLeakCount = 0
	session.SecretsRulesVersion = ""
	session.SourceSessionID = prefixImportedSessionID(
		m.Origin, session.SourceSessionID,
	)
	if session.ParentSessionID != nil {
		parent := prefixImportedSessionID(m.Origin, *session.ParentSessionID)
		session.ParentSessionID = &parent
	}
	for i := range messages {
		messages[i].ID = 0
		messages[i].SessionID = importedID
		for j := range messages[i].ToolCalls {
			call := &messages[i].ToolCalls[j]
			call.MessageID = 0
			call.SessionID = importedID
			call.SubagentSessionID = prefixImportedSessionID(
				m.Origin, call.SubagentSessionID,
			)
			for k := range call.ResultEvents {
				event := &call.ResultEvents[k]
				event.SubagentSessionID = prefixImportedSessionID(
					m.Origin, event.SubagentSessionID,
				)
			}
		}
	}
	return db.SessionBatchWrite{
		Session:         session,
		Messages:        messages,
		UsageEvents:     importedUsageEvents(m.UsageEvents, importedID),
		Signals:         signalsFromImportedSession(session),
		DataVersion:     m.DataVersion,
		ReplaceMessages: true,
	}
}

func clearImportedSessionSourceState(session *db.Session) {
	session.FilePath = nil
	session.FileSize = nil
	session.FileMtime = nil
	session.NextOrdinal = 0
	session.LastEntryUUID = nil
	session.FileInode = nil
	session.FileDevice = nil
	session.FileHash = nil
}

func prefixImportedSessionID(origin, id string) string {
	if id == "" || strings.Contains(id, "~") {
		return id
	}
	return origin + "~" + id
}

func importedUsageEvents(
	events []artifactUsageEvent, sessionID string,
) []db.UsageEvent {
	out := make([]db.UsageEvent, len(events))
	for i, event := range events {
		out[i] = db.UsageEvent{
			SessionID: sessionID, MessageOrdinal: event.MessageOrdinal,
			Source: event.Source, Model: event.Model,
			ProviderID:  event.ProviderID,
			InputTokens: event.InputTokens, OutputTokens: event.OutputTokens,
			CacheCreationInputTokens: event.CacheCreationInputTokens,
			CacheReadInputTokens:     event.CacheReadInputTokens,
			ReasoningTokens:          event.ReasoningTokens,
			Cost:                     event.Cost, CostStatus: event.CostStatus,
			CostSource: event.CostSource, OccurredAt: event.OccurredAt,
			DedupKey: event.DedupKey,
		}
	}
	return out
}

func signalsFromImportedSession(session db.Session) db.SessionSignalUpdate {
	signals := db.SessionSignalUpdate{
		ToolFailureSignalCount: session.ToolFailureSignalCount,
		ToolRetryCount:         session.ToolRetryCount,
		EditChurnCount:         session.EditChurnCount,
		ConsecutiveFailureMax:  session.ConsecutiveFailureMax,
		Outcome:                session.Outcome,
		OutcomeConfidence:      session.OutcomeConfidence,
		EndedWithRole:          session.EndedWithRole,
		FinalFailureStreak:     session.FinalFailureStreak,
		SignalsPendingSince:    session.SignalsPendingSince,
		CompactionCount:        session.CompactionCount,
		MidTaskCompactionCount: session.MidTaskCompactionCount,
		ContextPressureMax:     session.ContextPressureMax,
		HealthScore:            session.HealthScore,
		HealthGrade:            session.HealthGrade,
		HasToolCalls:           session.HasToolCalls,
		HasContextData:         session.HasContextData,
	}
	if quality := session.StoredQualitySignals(); quality != nil {
		signals.QualitySignals = *quality
	}
	return signals
}
