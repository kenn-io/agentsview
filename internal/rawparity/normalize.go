// Package rawparity compares complete stored session content without reading a
// database. Callers supply physical rows and a match map for relationship IDs.
//
// Field matrix (all fields outside the compared set are masked):
//
//	Part           Compared fields
//	metadata       Agent, project, cwd, first message, start/end, message counts
//	messages       All persisted message fields, including model and usage flags
//	tool_calls     Calls (including input, target path and call index) and results
//	usage          Every accounting field, including nullable cost/message links
//	findings       Secret and friction findings, including natural coordinates
//	relationships  Parent set/type and call/result subagent targets via MatchIDs
//
// Normalized: JSON object key order, UTC microsecond timestamps, nil/empty row
// sets, row order (ordinal/call/event coordinates remain content), physical IDs.
// Masked: machine, custody file coordinates, publication/import clocks and
// versions, provenance/raw columns, deletion/rollout state, names, stars, pins,
// derived session/tool signals, dialogue text and transient parser/tool state.
// Tool target paths are content, unlike the session's custody file path.
// Friction titles and fingerprints embed physical session IDs and are masked;
// their semantic text, labels, evidence, detector and coordinates are compared.
//
// The transport masks and JSON key normalization follow rawContentRevision in
// internal/postgres/raw_projection_content.go. Numeric JSON spellings stay
// exact, including large integers. This narrower stored-content matrix does not
// change that versioned parser-payload digest. Timestamps use the same precision
// as PostgreSQL push fingerprints. db.Session has no model; messages carry it.
package rawparity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// Snapshot is one session's complete rows. MatchIDs maps physical or public
// relationship targets to common identities on each side. Unmapped IDs remain
// literal; callers must not invent matches for ambiguous or source-local IDs.
// A session's own ID is ignored: matching the two sessions is the caller's job.
type Snapshot struct {
	Session  db.Session
	Messages []db.Message
	Usage    []db.UsageEvent
	Secrets  []db.SecretFinding
	Friction []db.FrictionFinding
	MatchIDs map[string]string
}

// Comparison exposes only digests and part names, never transcript content.
// Differing is in field-matrix order; empty means every compared part is equal.
type Comparison struct {
	Left, Right map[string]string
	Differing   []string
}

var parts = [...]string{"metadata", "messages", "tool_calls", "usage", "findings", "relationships"}

// Compare normalizes both inputs without changing them and hashes each part.
// Rows may arrive in any order; explicit coordinates preserve semantic order.
func Compare(a, b Snapshot) (Comparison, error) {
	left, err := digests(a)
	if err != nil {
		return Comparison{}, err
	}
	right, err := digests(b)
	if err != nil {
		return Comparison{}, err
	}
	result := Comparison{Left: left, Right: right}
	for _, part := range parts {
		if left[part] != right[part] {
			result.Differing = append(result.Differing, part)
		}
	}
	return result, nil
}

func digests(s Snapshot) (map[string]string, error) {
	rows := make(map[string][]any)
	session := s.Session
	rows["metadata"] = []any{[]any{session.Agent, session.Project, session.Cwd, session.FirstMessage, timePointer(session.StartedAt), timePointer(session.EndedAt), session.MessageCount, session.UserMessageCount}}
	parents := slices.Clone(session.ParentSessionIDs)
	if session.ParentSessionID != nil && *session.ParentSessionID != "" {
		parents = append(parents, *session.ParentSessionID)
	}
	for i := range parents {
		parents[i] = target(s.MatchIDs, parents[i])
	}
	slices.Sort(parents)
	parents = slices.Compact(parents)
	if parents == nil {
		parents = []string{}
	}
	rows["relationships"] = []any{[]any{"parents", session.RelationshipType, parents}}
	for _, message := range s.Messages {
		m := message
		m.ID = 0
		m.SessionID = ""
		m.Timestamp = timestamp(m.Timestamp)
		m.ToolCalls = nil
		m.ToolResults = nil
		m.DialogueText = nil
		usage := canonicalJSON(string(m.TokenUsage))
		m.TokenUsage = nil
		rows["messages"] = append(rows["messages"], []any{m, usage})
		for _, call := range message.ToolCalls {
			c := call
			c.MessageID = 0
			c.SessionID = ""
			c.Rendering = ""
			c.InputJSON = canonicalJSON(c.InputJSON)
			c.ResultEvents = nil
			link := c.SubagentSessionID
			c.SubagentSessionID = ""
			// JSON intentionally omits target paths and call indexes on db.ToolCall.
			// Carry them explicitly, along with the owning message coordinate.
			rows["tool_calls"] = append(rows["tool_calls"], []any{"call", m.Ordinal, c.CallIndex, c.FilePath, c})
			if link != "" {
				rows["relationships"] = append(rows["relationships"], []any{"call", m.Ordinal, c.CallIndex, target(s.MatchIDs, link)})
			}
			for _, event := range call.ResultEvents {
				e := event
				e.Timestamp = timestamp(e.Timestamp)
				e.RawContentDigest = nil
				e.SummaryParticipates = nil
				link = e.SubagentSessionID
				e.SubagentSessionID = ""
				rows["tool_calls"] = append(rows["tool_calls"], []any{"result", m.Ordinal, c.CallIndex, e})
				if link != "" {
					rows["relationships"] = append(rows["relationships"], []any{"result", m.Ordinal, c.CallIndex, e.EventIndex, target(s.MatchIDs, link)})
				}
			}
		}
	}
	for _, e := range s.Usage {
		e.ID = 0
		e.SessionID = ""
		e.OccurredAt = timestamp(e.OccurredAt)
		rows["usage"] = append(rows["usage"], e)
	}
	for _, f := range s.Secrets {
		f.SessionID = ""
		f.RulesVersion = ""
		rows["findings"] = append(rows["findings"], []any{"secret", f})
	}
	for _, f := range s.Friction {
		f.SessionID = ""
		f.RulesVersion = ""
		f.Title, f.Fingerprint = "", ""
		if f.OccurredAt != nil {
			v := f.OccurredAt.UTC().Truncate(time.Microsecond)
			f.OccurredAt = &v
		}
		rows["findings"] = append(rows["findings"], []any{"friction", f})
	}
	out := make(map[string]string, len(parts))
	for _, part := range parts {
		records := make([]string, 0, len(rows[part]))
		for _, row := range rows[part] {
			encoded, err := json.Marshal(row, json.Deterministic(true))
			if err != nil {
				return nil, fmt.Errorf("cannot normalize parity part %s", part)
			}
			records = append(records, string(encoded))
		}
		slices.Sort(records)
		encoded, err := json.Marshal(records)
		if err != nil {
			return nil, fmt.Errorf("cannot encode parity part %s", part)
		}
		hash := sha256.Sum256(append([]byte("raw-parity-v1:"+part+":"), encoded...))
		out[part] = hex.EncodeToString(hash[:])
	}
	return out, nil
}

func target(ids map[string]string, id string) string {
	if matched, ok := ids[id]; ok {
		return matched
	}
	return id
}

func timestamp(s string) string {
	// Mirror the accepted SQLite representations in postgres.ParseSQLiteTimestamp
	// without making this pure package depend on the PostgreSQL reader.
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07", "2006-01-02 15:04:05"} {
		if v, err := time.Parse(layout, s); err == nil {
			return v.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
		}
	}
	return s
}

func timePointer(s *string) *string {
	if s == nil {
		return nil
	}
	return new(timestamp(*s))
}

// Keep malformed JSON verbatim; never turn distinct malformed payloads into an
// empty/default value. Raw number tokens avoid float64 precision loss.
func canonicalJSON(s string) string {
	if s == "" {
		return ""
	}
	numbers := json.UnmarshalFromFunc(func(dec *jsontext.Decoder, value *any) error {
		if dec.PeekKind() != '0' {
			return errors.ErrUnsupported
		}
		raw, err := dec.ReadValue()
		*value = raw.Clone()
		return err
	})
	var v any
	if err := json.Unmarshal([]byte(s), &v, json.WithUnmarshalers(numbers)); err != nil {
		return s
	}
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return s
	}
	return string(b)
}
