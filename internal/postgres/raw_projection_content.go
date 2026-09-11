package postgres

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/rawsync"
)

func rawDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func rawSourceID(m rawsync.CanonicalManifest) string {
	return rawDigest("source-v1", m.Identity.TenantID, m.Identity.DeviceID, string(m.Manifest.Provider), m.Manifest.ConfiguredRootID, m.Manifest.SourceKey)
}
func rawGroupID(m rawsync.CanonicalManifest, s db.Session) (string, string) {
	key := s.SourceSessionID
	if key == "" {
		key = rawDigest("source-local-v1", rawSourceID(m), s.ID)
	}
	return rawDigest("group-v1", m.Identity.TenantID, s.Agent, key), key
}
func encodeRawPayload(p ingest.PreparedSession) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(p)
	return b.Bytes(), err
}
func decodeRawPayload(b []byte) (ingest.PreparedSession, error) {
	var p ingest.PreparedSession
	err := gob.NewDecoder(bytes.NewReader(b)).Decode(&p)
	return p, err
}

// rawContentRevision hashes a versioned structural representation of every
// normalized field, including fields omitted from public JSON. Only the explicit
// non-content fields below are removed. JSON-valued
// fields are canonical structures, not their formatting or object key order.
func rawContentRevision(p ingest.PreparedSession) (string, error) {
	s := &p.Session
	s.ID = ""
	s.Machine = ""
	s.DisplayName = nil
	s.CreatedAt = ""
	s.LocalModifiedAt = nil
	s.TranscriptRevision = nil
	s.DeletedAt = nil
	s.DeletionCause = nil
	s.SourceMissingAt = nil
	s.FilePath = nil
	s.FileSize = nil
	s.FileMtime = nil
	s.FileInode = nil
	s.FileDevice = nil
	s.FileHash = nil
	s.NextOrdinal = 0
	s.LastEntryUUID = nil
	s.ClaudeLinearParse = nil
	s.LastWriteIncremental = false
	s.PreserveSessionName = false
	s.PreserveStoredAutomation = false
	s.DataVersion = 0
	s.SecretsRulesVersion = ""
	s.QualitySignalVersion = 0
	// Recency-derived state is published separately from immutable content.
	s.SignalsPendingSince, s.HealthScore, s.HealthGrade = nil, nil, nil
	s.Outcome, s.OutcomeConfidence = "", ""
	if s.QualitySignals != nil {
		copy := *s.QualitySignals
		copy.Version = 0
		s.QualitySignals = &copy
	}
	p.Signals.FullState = nil
	p.Signals.SecretsRulesVersion = ""
	p.Signals.QualitySignals.Version = 0
	p.Signals.SignalsPendingSince, p.Signals.HealthScore, p.Signals.HealthGrade = nil, nil, nil
	p.Signals.Outcome, p.Signals.OutcomeConfidence = "", ""
	p.Validation = db.ValidationStats{}
	// Copy nested slices before removing transport IDs: callers retain their rows.
	var err error
	p.Messages = append([]db.Message(nil), p.Messages...)
	for i := range p.Messages {
		m := &p.Messages[i]
		m.ID = 0
		m.SessionID = ""
		m.ToolResults = nil
		m.ToolCalls = append([]db.ToolCall(nil), m.ToolCalls...)
		for j := range m.ToolCalls {
			tc := &m.ToolCalls[j]
			tc.MessageID = 0
			tc.Rendering = ""
			tc.SessionID = ""
			tc.ResultEvents = append([]db.ToolResultEvent(nil), tc.ResultEvents...)
			for k := range tc.ResultEvents {
				tc.ResultEvents[k].RawContentDigest = nil
				tc.ResultEvents[k].SummaryParticipates = nil
			}
		}
	}
	p.UsageEvents = append([]db.UsageEvent(nil), p.UsageEvents...)
	for i := range p.UsageEvents {
		p.UsageEvents[i].ID = 0
		p.UsageEvents[i].SessionID = ""
	}
	p.Findings = append([]db.SecretFinding(nil), p.Findings...)
	for i := range p.Findings {
		p.Findings[i].SessionID = ""
		p.Findings[i].RulesVersion = ""
	}
	structure, err := rawCanonicalValue(reflect.ValueOf(p), "")
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(structure)
	if err != nil {
		return "", err
	}
	return rawDigest("normalized-content-v1", string(encoded)), nil
}
func rawCanonicalValue(v reflect.Value, field string) (any, error) {
	if !v.IsValid() {
		return nil, nil
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil, nil
		}
		return rawCanonicalValue(v.Elem(), field)
	}
	if field == "TokenUsage" || field == "InputJSON" {
		var b []byte
		if v.Kind() == reflect.String {
			b = []byte(v.String())
		} else if v.Kind() == reflect.Slice {
			b = v.Bytes()
		}
		if len(b) > 0 {
			var value any
			d := json.NewDecoder(bytes.NewReader(b))
			d.UseNumber()
			if err := d.Decode(&value); err == nil {
				return value, nil
			}
		}
	}
	switch v.Kind() {
	case reflect.Struct:
		out := map[string]any{}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			value, err := rawCanonicalValue(v.Field(i), f.Name)
			if err != nil {
				return nil, err
			}
			out[f.Name] = value
		}
		return out, nil
	case reflect.Slice, reflect.Array:
		out := make([]any, v.Len())
		for i := range out {
			value, err := rawCanonicalValue(v.Index(i), "")
			if err != nil {
				return nil, err
			}
			out[i] = value
		}
		return out, nil
	case reflect.Map:
		out := map[string]any{}
		iter := v.MapRange()
		for iter.Next() {
			value, err := rawCanonicalValue(iter.Value(), "")
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(iter.Key().Interface())] = value
		}
		return out, nil
	default:
		return v.Interface(), nil
	}
}

func rawMessageKey(m db.Message) (string, error) {
	// Include ordinal and complete message content. A UUID or ordinal reused for
	// different content must leave an old pin unresolved.
	p := ingest.PreparedSession{Messages: []db.Message{m}}
	return rawContentRevision(p)
}
