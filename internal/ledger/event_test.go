package ledger

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/serdejson"
)

// testEvent mirrors jilog's segment.rs test_event helper with a fixed ID
// and time so failures are reproducible.
func testEvent(zone string, seq uint64) Event {
	actor := "person:test-user"
	return Event{
		EventID:     uuid.MustParse("0190f5a2-7c3e-7000-8000-00000000aa01"),
		Zone:        zone,
		Source:      "test",
		SourceSeq:   seq,
		Timestamp:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ActorRef:    &actor,
		EventClass:  ClassHealth,
		PayloadTier: TierMetadataOnly,
	}
}

func TestEventClassForms(t *testing.T) {
	tests := []struct {
		class EventClass
		serde string
		debug string
	}{
		{ClassIngest, "ingest", "ingest"},
		{ClassRoute, "route", "route"},
		{ClassDecision, "decision", "decision"},
		{ClassStateChange, "state_change", "statechange"},
		{ClassClaim, "claim", "claim"},
		{ClassDelivery, "delivery", "delivery"},
		{ClassProjection, "projection", "projection"},
		{ClassHealth, "health", "health"},
		{ClassApproval, "approval", "approval"},
		{ClassNoteMeta, "note_meta", "notemeta"},
	}
	require.Len(t, AllClasses, len(tests))
	for i, tt := range tests {
		t.Run(tt.serde, func(t *testing.T) {
			assert.Equal(t, tt.class, AllClasses[i], "declaration order")
			assert.Equal(t, tt.serde, string(tt.class))
			assert.Equal(t, tt.debug, tt.class.DebugLower())
			assert.True(t, tt.class.Valid())
			fromSerde, err := ParseClass(tt.serde)
			require.NoError(t, err)
			assert.Equal(t, tt.class, fromSerde)
			fromDebug, err := ParseClass(tt.debug)
			require.NoError(t, err)
			assert.Equal(t, tt.class, fromDebug)
		})
	}
	assert.False(t, EventClass("review").Valid(), "README-only classes are not added (D21)")
	assert.False(t, EventClass("learning").Valid())
}

func TestParseClass(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    EventClass
		wantErr string
	}{
		// jilog query.rs:714 parse_class_known_values
		{name: "parse_class_known_values/state-change", in: "state-change", want: ClassStateChange},
		{name: "parse_class_known_values/StateChange", in: "StateChange", want: ClassStateChange},
		{name: "parse_class_known_values/approval", in: "approval", want: ClassApproval},
		{
			name: "parse_class_known_values/nonsense", in: "nonsense",
			wantErr: "unknown event class 'nonsense' (valid: ingest, route, decision, state-change, claim, delivery, projection, health, approval, note-meta)",
		},
		{name: "note-meta", in: "Note-Meta", want: ClassNoteMeta},
		{name: "note_meta", in: "NOTE_META", want: ClassNoteMeta},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseClass(tt.in)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPayloadTierForms(t *testing.T) {
	tests := []struct {
		tier  PayloadTier
		serde string
		debug string
	}{
		{TierMetadataOnly, "metadata_only", "metadataonly"},
		{TierStructured, "structured", "structured"},
		{TierConfidential, "confidential", "confidential"},
	}
	require.Len(t, AllTiers, len(tests))
	for i, tt := range tests {
		t.Run(tt.serde, func(t *testing.T) {
			assert.Equal(t, tt.tier, AllTiers[i])
			assert.Equal(t, tt.serde, string(tt.tier))
			assert.Equal(t, tt.debug, tt.tier.DebugLower())
			for _, in := range []string{tt.serde, tt.debug} {
				got, err := ParseTier(in)
				require.NoError(t, err)
				assert.Equal(t, tt.tier, got)
			}
		})
	}
	_, err := ParseTier("secret")
	require.EqualError(t, err,
		"unknown payload tier 'secret' (valid: metadata-only, structured, confidential)")
}

func TestEventMarshalSerde(t *testing.T) {
	corr := uuid.MustParse("0190f5a2-7c3e-7000-8000-0000000000c1")
	obj := "subsystem:x"
	tests := []struct {
		name  string
		event func() Event
		want  string
	}{
		{
			name:  "nulls_written_in_declaration_order",
			event: func() Event { return testEvent("zone-a", 1) },
			want: `{"event_id":"0190f5a2-7c3e-7000-8000-00000000aa01","zone":"zone-a","source":"test","source_seq":1,` +
				`"timestamp":"2026-01-02T03:04:05Z","correlation_id":null,"causation_id":null,` +
				`"actor_ref":"person:test-user","object_ref":null,"event_class":"health",` +
				`"payload_tier":"metadata_only","payload":null}`,
		},
		{
			name: "payload_keys_sorted_and_html_unescaped",
			event: func() Event {
				e := testEvent("z", 2)
				e.CorrelationID = &corr
				e.ObjectRef = &obj
				e.EventClass = ClassStateChange
				e.PayloadTier = TierStructured
				e.Timestamp = time.Date(2026, 1, 2, 3, 4, 5, 100_000_000, time.UTC)
				e.Payload = map[string]any{"z": serdejson.Number("1"), "a": "<b>&</b>"}
				return e
			},
			want: `{"event_id":"0190f5a2-7c3e-7000-8000-00000000aa01","zone":"z","source":"test","source_seq":2,` +
				`"timestamp":"2026-01-02T03:04:05.100Z","correlation_id":"0190f5a2-7c3e-7000-8000-0000000000c1",` +
				`"causation_id":null,"actor_ref":"person:test-user","object_ref":"subsystem:x",` +
				`"event_class":"state_change","payload_tier":"structured","payload":{"a":"<b>&</b>","z":1}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.event().MarshalSerde()
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestParseEventsJSONErrors(t *testing.T) {
	good := `{"event_id":"0190f5a2-7c3e-7000-8000-00000000aa01","zone":"z","source":"s","source_seq":1,` +
		`"timestamp":"2026-01-02T03:04:05Z","event_class":"health","payload_tier":"structured"}`
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "missing_options_are_none", in: "[" + good + "]"},
		{name: "not_an_array", in: `{}`, wantErr: "events is not an array"},
		{name: "missing_required", in: `[{"zone":"z"}]`, wantErr: "missing field `event_id`"},
		{name: "unknown_class", in: `[` + replace(good, `"health"`, `"review"`) + `]`, wantErr: "unknown variant `review`"},
		{name: "debug_class_is_not_serde", in: `[` + replace(good, `"health"`, `"statechange"`) + `]`, wantErr: "unknown variant `statechange`"},
		{name: "float_seq", in: `[` + replace(good, `"source_seq":1`, `"source_seq":1.0`) + `]`, wantErr: "invalid value for `source_seq`"},
		{name: "bad_uuid", in: `[` + replace(good, `0190f5a2-7c3e-7000-8000-00000000aa01`, `nope`) + `]`, wantErr: "event_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := ParseEventsJSON([]byte(tt.in))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, events, 1)
			assert.Nil(t, events[0].CorrelationID)
			assert.Nil(t, events[0].ActorRef)
			assert.Nil(t, events[0].Payload)
		})
	}
}

func replace(s, old, repl string) string { return strings.Replace(s, old, repl, 1) }

func TestEventTimestampIsWrittenInUTC(t *testing.T) {
	e := testEvent("z", 1)
	e.Timestamp = time.Date(2026, 1, 2, 4, 4, 5, 0, time.FixedZone("plus1", 3600))
	b, err := e.MarshalSerde()
	require.NoError(t, err)
	assert.Contains(t, string(b), `"timestamp":"2026-01-02T03:04:05Z"`)
}
