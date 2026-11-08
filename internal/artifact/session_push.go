package artifact

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

// MaxSessionPushBytes bounds the complete serialized single-session envelope.
const MaxSessionPushBytes int64 = 64 << 20

// SessionPushEnvelope carries exactly one normalized transcript. Manifests and
// segments use the existing artifact format; raw sources and checkpoints are
// deliberately absent. Raw JSON defers allocation until collection preflights.
type SessionPushEnvelope struct {
	Version   int            `json:"v"`
	SessionID string         `json:"session_id"`
	Manifest  jsontext.Value `json:"manifest"`
	Segments  jsontext.Value `json:"segments"`
}

type sessionPushSegment struct {
	SHA256 string `json:"sha256"`
	NDJSON string `json:"ndjson"`
}

type sessionPushCapture struct {
	manifest []byte
	segments []sessionPushSegment
	bytes    int64
}

func (s *sessionPushCapture) Create(_ context.Context, ref Ref, identity Identity, _ string, r io.Reader) (CreateResult, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxSessionPushBytes-s.bytes+1))
	if err != nil {
		return CreateResult{}, err
	}
	s.bytes += int64(len(data))
	if s.bytes > MaxSessionPushBytes {
		return CreateResult{}, errors.New("session push exceeds 64 MiB")
	}
	switch ref.Kind {
	case KindManifests:
		s.manifest = data
	case KindSegments:
		s.segments = append(s.segments, sessionPushSegment{SHA256: identity.SHA256, NDJSON: string(data)})
	default:
		return CreateResult{}, errors.New("unexpected session push dependency")
	}
	return CreateResult{Created: true}, nil
}

// EncodeSessionPush reads exactly id from one SQLite snapshot. It never syncs
// sources, resolves partial IDs, or traverses parent/subagent references.
func EncodeSessionPush(ctx context.Context, database *db.DB, id, expectedAgent string) ([]byte, error) {
	if id == "" || strings.Contains(id, "~") {
		return nil, errors.New("push requires an exact native archive session ID")
	}
	if database.ArchiveContent().UsageOnly() {
		return nil, errors.New("usage-only archive cannot supply a transcript")
	}
	session, data, err := database.LoadSessionPushData(ctx, id, artifactExportLoadLimits(productionArtifactLimits()))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("cannot read the selected session within transfer limits")
	}
	if session == nil {
		return nil, errors.New("session is not in the local archive")
	}
	if expectedAgent != "" && session.Agent != expectedAgent {
		return nil, errors.New("session agent does not match --agent")
	}
	archiveID, err := database.GetArchiveID(ctx)
	if err != nil || archiveID == "" {
		return nil, errors.New("source archive identity is unavailable")
	}
	origin := "push-" + hashHex([]byte(archiveID))[:32]
	return encodeSessionPushData(ctx, origin, *session, data)
}

func encodeSessionPushData(ctx context.Context, origin string, session db.Session, data db.ArtifactExportData) ([]byte, error) {
	// Revisions are local bookkeeping, not part of this immutable content key.
	session.TranscriptRevision = nil
	sink := &sessionPushCapture{}
	_, _, err := exportLoadedSessionToStore(ctx, sink, origin, &session, canonicalMessages(data.Messages), data.UsageEvents, data.PlanUsage, productionArtifactLimits())
	if err != nil {
		return nil, err
	}
	segments, err := json.Marshal(sink.segments)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(SessionPushEnvelope{Version: 1, SessionID: session.ID, Manifest: sink.manifest, Segments: segments})
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > MaxSessionPushBytes {
		return nil, errors.New("session push exceeds 64 MiB")
	}
	// Validate the same closure that the receiver will accept before networking.
	if _, _, err := DecodeSessionPush(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

// DecodeSessionPush validates a complete bounded closure before returning any
// write intent. The caller must atomically persist it with the receipt.
func DecodeSessionPush(data []byte) (db.SessionPushReceipt, db.SessionBatchWrite, error) {
	receipt, write, err := decodeSessionPush(data)
	if err != nil {
		return db.SessionPushReceipt{}, db.SessionBatchWrite{}, fmt.Errorf("invalid session push: %w", err)
	}
	return receipt, write, nil
}

func decodeSessionPush(data []byte) (db.SessionPushReceipt, db.SessionBatchWrite, error) {
	var zero db.SessionPushReceipt
	var noWrite db.SessionBatchWrite
	if int64(len(data)) > MaxSessionPushBytes {
		return zero, noWrite, errors.New("body exceeds 64 MiB")
	}
	var envelope SessionPushEnvelope
	if err := json.Unmarshal(data, &envelope, json.RejectUnknownMembers(true)); err != nil {
		return zero, noWrite, err
	}
	if envelope.Version != 1 || envelope.SessionID == "" || len(envelope.SessionID) > 1024 || strings.Contains(envelope.SessionID, "~") {
		return zero, noWrite, errors.New("unsupported version or session ID")
	}
	limits := productionArtifactLimits()
	if int64(len(envelope.Manifest)) > manifestDecodedLimit {
		return zero, noWrite, errors.New("manifest exceeds limit")
	}
	// PR links are session metadata, outside the artifact graph preflights.
	var metadata struct {
		Session struct {
			PRLinks jsontext.Value `json:"pr_links"`
		} `json:"session"`
	}
	if err := json.Unmarshal(envelope.Manifest, &metadata); err != nil {
		return zero, noWrite, err
	}
	if err := preflightJSONArrayCount(metadata.Session.PRLinks, "session push PR link", 256); err != nil {
		return zero, noWrite, err
	}
	m, err := decodeManifestWithLimits(envelope.Manifest, limits)
	if err != nil {
		return zero, noWrite, err
	}
	if err := json.Unmarshal(envelope.Manifest, &m, json.RejectUnknownMembers(true)); err != nil {
		return zero, noWrite, err
	}
	if err := validateOriginID(m.Origin); err != nil {
		return zero, noWrite, err
	}
	if m.NativeSessionID != envelope.SessionID || m.RawSource != nil || m.Session.Agent == "" || m.Session.DeletedAt != nil {
		return zero, noWrite, errors.New("invalid selected session")
	}
	if err := validateImportedManifest(m, m.Origin, m.Origin+"~"+envelope.SessionID, limits); err != nil {
		return zero, noWrite, err
	}
	if err := preflightJSONArrayCount(envelope.Segments, "segments", limits.manifestSegments); err != nil {
		return zero, noWrite, err
	}
	var segments []sessionPushSegment
	if err := json.Unmarshal(envelope.Segments, &segments, json.RejectUnknownMembers(true)); err != nil {
		return zero, noWrite, err
	}
	if len(segments) != len(m.Segments) {
		return zero, noWrite, errors.New("segment closure mismatch")
	}
	var messages []db.Message
	var nested nestedCollectionCounts
	var decodedBytes int64
	for i, segment := range segments {
		if segment.SHA256 != m.Segments[i] || hashHex([]byte(segment.NDJSON)) != segment.SHA256 {
			return zero, noWrite, errors.New("segment hash mismatch")
		}
		preflight, err := preflightSegmentData([]byte(segment.NDJSON), limits)
		if err != nil {
			return zero, noWrite, err
		}
		if int64(len(segment.NDJSON)) > limits.sessionDecodedBytes-decodedBytes || exceedsCollectionLimit(len(messages), len(preflight.records), limits.sessionMessages) || exceedsCollectionLimit(nested.toolCalls, preflight.nested.toolCalls, limits.sessionToolCalls) || exceedsCollectionLimit(nested.resultEvents, preflight.nested.resultEvents, limits.sessionResultEvents) {
			return zero, noWrite, errors.New("session aggregate limit exceeded")
		}
		for _, record := range preflight.records {
			var message segmentMessage
			if err := json.Unmarshal(record, &message, json.RejectUnknownMembers(true)); err != nil {
				return zero, noWrite, err
			}
			if message.Ordinal != len(messages) {
				return zero, noWrite, errors.New("message ordinals must be contiguous from zero")
			}
			for callIndex, call := range message.ToolCalls {
				if call.CallIndex != callIndex {
					return zero, noWrite, errors.New("invalid tool call coordinate")
				}
				if err := db.ValidateSessionPushResultCollections(call.ResultContent); err != nil {
					return zero, noWrite, err
				}
				for eventIndex, event := range call.ResultEvents {
					if event.EventIndex != eventIndex {
						return zero, noWrite, errors.New("invalid result event coordinate")
					}
					if err := db.ValidateSessionPushResultCollections(event.Content); err != nil {
						return zero, noWrite, err
					}
				}
			}
			converted := message.dbMessage()
			// Normalize before asset downgrade and signal extraction, matching the
			// text just preflighted. Receipt hashing still uses the wire manifest
			// and segment digests, independently of these storage projections.
			for i := range converted.ToolCalls {
				db.SanitizeToolCall(&converted.ToolCalls[i])
			}
			messages = append(messages, converted)
		}
		nested.toolCalls += preflight.nested.toolCalls
		nested.resultEvents += preflight.nested.resultEvents
		decodedBytes += int64(len(segment.NDJSON))
	}
	if err := validateImportedClosure(m, messages); err != nil {
		return zero, noWrite, err
	}
	for _, event := range m.UsageEvents {
		if event.MessageOrdinal != nil && (*event.MessageOrdinal < 0 || *event.MessageOrdinal >= len(messages)) {
			return zero, noWrite, errors.New("usage references missing message")
		}
	}
	// Hash the canonical validated manifest and ordered dependency digests, never
	// sender-supplied receipt material or outer transport whitespace.
	normalizeManifestSessionLocalState(&m.Session)
	m.Session.TranscriptRevision = nil
	canonical, err := canonicalJSON(struct {
		Version  int      `json:"v"`
		Manifest manifest `json:"manifest"`
	}{1, m})
	if err != nil {
		return zero, noWrite, err
	}
	receipt := db.SessionPushReceipt{SessionID: envelope.SessionID, SourceOrigin: m.Origin, PayloadHash: hashHex(canonical)}
	session := m.Session.dbSession()
	clearImportedSessionSourceState(&session)
	session.HasToolCalls = m.SessionHasToolCalls
	session.HasContextData = m.SessionHasContextData
	session.SessionName = m.SessionName
	session.ApplyQualitySignals(m.SessionQualitySignals.dbQualitySignals())
	session.SecretLeakCount = 0
	session.SecretsRulesVersion = ""
	for i := range messages {
		messages[i].SessionID = session.ID
		for j := range messages[i].ToolCalls {
			messages[i].ToolCalls[j].SessionID = session.ID
		}
	}
	downgradeImportedAssetReferences(messages)
	signals := signalsFromImportedSession(session)
	signals.ToolObservations = ingest.ComputeToolObservations(session, ingest.ExtractToolCallRows(messages))
	planUsage := importedPlanUsage(m)
	if planUsage != nil && m.PlanUsage != nil {
		planUsage.CountedUnder = m.PlanUsage.CountedUnder
	}
	return receipt, db.SessionBatchWrite{Session: session, Messages: messages, UsageEvents: importedUsageEvents(m.UsageEvents, session.ID), PlanUsage: planUsage, Signals: signals, DataVersion: m.DataVersion, ReplaceMessages: true}, nil
}
