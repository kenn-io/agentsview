// Package ledger is the append-only event ledger adapted from jilog's
// ledger-core (MIT, 9e8e094). Events, segments and their CRC are
// byte-compatible with jilog segment files. The package is pure: no
// database, no network, no filesystem.
package ledger

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"go.kenn.io/agentsview/internal/serdejson"
)

// EventClass is the event taxonomy. The string values are serde's
// snake_case forms (jilog event.rs:16-29), used in segments, storage and
// JSON output.
type EventClass string

const (
	ClassIngest      EventClass = "ingest"
	ClassRoute       EventClass = "route"
	ClassDecision    EventClass = "decision"
	ClassStateChange EventClass = "state_change"
	ClassClaim       EventClass = "claim"
	ClassDelivery    EventClass = "delivery"
	ClassProjection  EventClass = "projection"
	ClassHealth      EventClass = "health"
	ClassApproval    EventClass = "approval"
	ClassNoteMeta    EventClass = "note_meta"
)

// AllClasses lists the classes in jilog's declaration order.
var AllClasses = []EventClass{
	ClassIngest, ClassRoute, ClassDecision, ClassStateChange, ClassClaim,
	ClassDelivery, ClassProjection, ClassHealth, ClassApproval, ClassNoteMeta,
}

// Valid reports whether c is one of the ten serde forms.
func (c EventClass) Valid() bool {
	return slices.Contains(AllClasses, c)
}

// DebugLower is jilog's `format!("{:?}", class).to_lowercase()` form used
// by `jilog query` text output: state_change -> statechange.
func (c EventClass) DebugLower() string {
	return strings.ReplaceAll(string(c), "_", "")
}

// ParseClass ports jilog query.rs:260-277: case-insensitive, and accepts
// the Debug, kebab and snake forms of the two multi-word classes.
func ParseClass(s string) (EventClass, error) {
	switch strings.ToLower(s) {
	case "ingest":
		return ClassIngest, nil
	case "route":
		return ClassRoute, nil
	case "decision":
		return ClassDecision, nil
	case "statechange", "state-change", "state_change":
		return ClassStateChange, nil
	case "claim":
		return ClassClaim, nil
	case "delivery":
		return ClassDelivery, nil
	case "projection":
		return ClassProjection, nil
	case "health":
		return ClassHealth, nil
	case "approval":
		return ClassApproval, nil
	case "notemeta", "note-meta", "note_meta":
		return ClassNoteMeta, nil
	}
	return "", fmt.Errorf(
		"unknown event class '%s' (valid: ingest, route, decision, state-change, claim, delivery, projection, health, approval, note-meta)",
		strings.ToLower(s),
	)
}

// PayloadTier is the payload confidentiality tier (jilog event.rs:32-41).
type PayloadTier string

const (
	TierMetadataOnly PayloadTier = "metadata_only"
	TierStructured   PayloadTier = "structured"
	TierConfidential PayloadTier = "confidential"
)

// AllTiers lists the tiers in jilog's declaration order.
var AllTiers = []PayloadTier{TierMetadataOnly, TierStructured, TierConfidential}

// Valid reports whether t is one of the three serde forms.
func (t PayloadTier) Valid() bool {
	return t == TierMetadataOnly || t == TierStructured || t == TierConfidential
}

// DebugLower is the Debug-lowercase form (metadata_only -> metadataonly).
func (t PayloadTier) DebugLower() string {
	return strings.ReplaceAll(string(t), "_", "")
}

// ParseTier accepts the serde, Debug-lowercase and kebab forms,
// case-insensitively.
func ParseTier(s string) (PayloadTier, error) {
	switch strings.ToLower(s) {
	case "metadata_only", "metadataonly", "metadata-only":
		return TierMetadataOnly, nil
	case "structured":
		return TierStructured, nil
	case "confidential":
		return TierConfidential, nil
	}
	return "", fmt.Errorf(
		"unknown payload tier '%s' (valid: metadata-only, structured, confidential)",
		strings.ToLower(s),
	)
}

// Event is one ledger event. Field order is jilog's declaration order
// (event.rs:50-89) and is the serialization order.
type Event struct {
	EventID       uuid.UUID
	Zone          string
	Source        string
	SourceSeq     uint64
	Timestamp     time.Time
	CorrelationID *uuid.UUID
	CausationID   *uuid.UUID
	ActorRef      *string
	ObjectRef     *string
	EventClass    EventClass
	PayloadTier   PayloadTier
	Payload       any // serdejson value model; nil => null
}

// MarshalSerde returns serde_json::to_vec(&event): declaration order,
// every None written as null.
func (e Event) MarshalSerde() ([]byte, error) {
	return e.appendSerde(nil)
}

func (e Event) appendSerde(b []byte) ([]byte, error) {
	if !e.EventClass.Valid() {
		return nil, fmt.Errorf("serialization error: unknown event class %q", e.EventClass)
	}
	if !e.PayloadTier.Valid() {
		return nil, fmt.Errorf("serialization error: unknown payload tier %q", e.PayloadTier)
	}
	payload, err := serdejson.Compact(e.Payload)
	if err != nil {
		return nil, fmt.Errorf("serialization error: %w", err)
	}
	b = append(b, `{"event_id":`...)
	b = appendString(b, e.EventID.String())
	b = append(b, `,"zone":`...)
	b = appendString(b, e.Zone)
	b = append(b, `,"source":`...)
	b = appendString(b, e.Source)
	b = append(b, `,"source_seq":`...)
	b = strconv.AppendUint(b, e.SourceSeq, 10)
	b = append(b, `,"timestamp":`...)
	b = appendString(b, serdejson.FormatTimestamp(e.Timestamp))
	b = append(b, `,"correlation_id":`...)
	b = appendUUIDPtr(b, e.CorrelationID)
	b = append(b, `,"causation_id":`...)
	b = appendUUIDPtr(b, e.CausationID)
	b = append(b, `,"actor_ref":`...)
	b = appendStringPtr(b, e.ActorRef)
	b = append(b, `,"object_ref":`...)
	b = appendStringPtr(b, e.ObjectRef)
	b = append(b, `,"event_class":`...)
	b = appendString(b, string(e.EventClass))
	b = append(b, `,"payload_tier":`...)
	b = appendString(b, string(e.PayloadTier))
	b = append(b, `,"payload":`...)
	b = append(b, payload...)
	return append(b, '}'), nil
}

func appendString(b []byte, s string) []byte {
	return append(b, serdejson.CompactString(s)...)
}

func appendStringPtr(b []byte, s *string) []byte {
	if s == nil {
		return append(b, "null"...)
	}
	return appendString(b, *s)
}

func appendUUIDPtr(b []byte, u *uuid.UUID) []byte {
	if u == nil {
		return append(b, "null"...)
	}
	return appendString(b, u.String())
}

// ParseEventsJSON parses a serde events array (a segment's `events`, or
// the ledger_segments.events_json column) into events.
func ParseEventsJSON(b []byte) ([]Event, error) {
	v, err := serdejson.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("serialization error: %w", err)
	}
	return eventsFromValue(v)
}

// ParseEventJSON parses one serde event object, such as the
// ledger_events.event_json column.
func ParseEventJSON(b []byte) (Event, error) {
	v, err := serdejson.Decode(b)
	if err != nil {
		return Event{}, fmt.Errorf("serialization error: %w", err)
	}
	e, err := eventFromValue(v)
	if err != nil {
		return Event{}, fmt.Errorf("serialization error: %w", err)
	}
	return e, nil
}

func eventsFromValue(v any) ([]Event, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, errors.New("serialization error: events is not an array")
	}
	events := make([]Event, 0, len(arr))
	for i, raw := range arr {
		e, err := eventFromValue(raw)
		if err != nil {
			return nil, fmt.Errorf("serialization error: events[%d]: %w", i, err)
		}
		events = append(events, e)
	}
	return events, nil
}

// eventFromValue mirrors serde's derived Deserialize for Event: required
// fields must be present with the right type, Option fields may be
// missing or null, unknown fields are ignored.
func eventFromValue(v any) (Event, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return Event{}, errors.New("event is not an object")
	}
	var e Event
	var err error
	idStr, err := requiredString(obj, "event_id")
	if err != nil {
		return Event{}, err
	}
	if e.EventID, err = uuid.Parse(idStr); err != nil {
		return Event{}, fmt.Errorf("event_id: %w", err)
	}
	if e.Zone, err = requiredString(obj, "zone"); err != nil {
		return Event{}, err
	}
	if e.Source, err = requiredString(obj, "source"); err != nil {
		return Event{}, err
	}
	if e.SourceSeq, err = requiredUint(obj, "source_seq", 64); err != nil {
		return Event{}, err
	}
	ts, err := requiredString(obj, "timestamp")
	if err != nil {
		return Event{}, err
	}
	if e.Timestamp, err = ParseTimestamp(ts); err != nil {
		return Event{}, fmt.Errorf("timestamp: %w", err)
	}
	if e.CorrelationID, err = optionalUUID(obj, "correlation_id"); err != nil {
		return Event{}, err
	}
	if e.CausationID, err = optionalUUID(obj, "causation_id"); err != nil {
		return Event{}, err
	}
	if e.ActorRef, err = optionalString(obj, "actor_ref"); err != nil {
		return Event{}, err
	}
	if e.ObjectRef, err = optionalString(obj, "object_ref"); err != nil {
		return Event{}, err
	}
	class, err := requiredString(obj, "event_class")
	if err != nil {
		return Event{}, err
	}
	if e.EventClass = EventClass(class); !e.EventClass.Valid() {
		return Event{}, fmt.Errorf("unknown variant `%s` for event_class", class)
	}
	tier, err := requiredString(obj, "payload_tier")
	if err != nil {
		return Event{}, err
	}
	if e.PayloadTier = PayloadTier(tier); !e.PayloadTier.Valid() {
		return Event{}, fmt.Errorf("unknown variant `%s` for payload_tier", tier)
	}
	e.Payload = obj["payload"] // absent and null both mean None
	return e, nil
}

func requiredString(obj map[string]any, key string) (string, error) {
	raw, ok := obj[key]
	if !ok {
		return "", fmt.Errorf("missing field `%s`", key)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("invalid type for `%s`: expected a string", key)
	}
	return s, nil
}

func requiredUint(obj map[string]any, key string, bits int) (uint64, error) {
	raw, ok := obj[key]
	if !ok {
		return 0, fmt.Errorf("missing field `%s`", key)
	}
	n, ok := raw.(serdejson.Number)
	if !ok {
		return 0, fmt.Errorf("invalid type for `%s`: expected an unsigned integer", key)
	}
	u, err := strconv.ParseUint(string(n), 10, bits)
	if err != nil {
		return 0, fmt.Errorf("invalid value for `%s`: %s", key, string(n))
	}
	return u, nil
}

func optionalString(obj map[string]any, key string) (*string, error) {
	raw, ok := obj[key]
	if !ok || raw == nil {
		return nil, nil
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("invalid type for `%s`: expected a string", key)
	}
	return &s, nil
}

func optionalUUID(obj map[string]any, key string) (*uuid.UUID, error) {
	s, err := optionalString(obj, key)
	if err != nil || s == nil {
		return nil, err
	}
	u, err := uuid.Parse(*s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return &u, nil
}

// ParseTimestamp accepts what chrono's DateTime<Utc> deserializer accepts
// in practice: RFC3339 with any offset, and a 'T', 't' or ' ' separator.
func ParseTimestamp(s string) (time.Time, error) {
	if len(s) > 10 && (s[10] == ' ' || s[10] == 't') {
		s = s[:10] + "T" + s[11:]
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
