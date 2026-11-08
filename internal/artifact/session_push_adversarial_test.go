package artifact

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// Build through the existing artifact encoder, independently of push validation,
// so malformed-coordinate tests remain able to reach the receiver boundary.
func adversarialSessionPushPacket(t *testing.T, session db.Session, messages []db.Message) []byte {
	t.Helper()
	sink := &sessionPushCapture{}
	_, _, err := exportLoadedSessionToStore(t.Context(), sink, "source", &session, messages, nil, nil, productionArtifactLimits())
	require.NoError(t, err)
	segments, err := json.Marshal(sink.segments)
	require.NoError(t, err)
	data, err := json.Marshal(SessionPushEnvelope{Version: 1, SessionID: session.ID, Manifest: sink.manifest, Segments: segments})
	require.NoError(t, err)
	return data
}

func TestSessionPushRejectsPRLinkAmplification(t *testing.T) {
	links := make([]db.PRLink, 4097)
	for i := range links {
		links[i] = db.PRLink{URL: "https://example.test/project/pull/1", Host: "example.test", Repository: "project", Number: 1}
	}
	data := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", PRLinks: links}, nil)
	_, _, err := DecodeSessionPush(data)
	require.Error(t, err)
}

func TestSessionPushRejectsEmbeddedResultCollectionAmplification(t *testing.T) {
	for _, section := range []string{"", "worker:\n"} {
		t.Run(section, func(t *testing.T) {
			content := section + "[" + strings.Repeat("{},", 4096) + "{}]"
			data := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1}, []db.Message{{Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}})
			_, _, err := DecodeSessionPush(data)
			require.Error(t, err)
		})
	}
}

func TestSessionPushRejectsNestedCoordinates(t *testing.T) {
	for _, coordinate := range []string{"call", "event"} {
		t.Run(coordinate, func(t *testing.T) {
			data := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1}, []db.Message{{Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Content: "first"}, {EventIndex: 1, Content: "second"}}}}}})
			_, _, err := DecodeSessionPush(data)
			require.NoError(t, err, "the unmodified graph must be valid")
			var envelope SessionPushEnvelope
			require.NoError(t, json.Unmarshal(data, &envelope))
			var segments []sessionPushSegment
			require.NoError(t, json.Unmarshal(envelope.Segments, &segments))
			var record segmentMessage
			require.NoError(t, json.Unmarshal([]byte(segments[0].NDJSON), &record))
			if coordinate == "call" {
				record.ToolCalls[0].CallIndex = -1
			} else {
				record.ToolCalls[0].ResultEvents[1].EventIndex = 0
			}
			body, err := canonicalJSON(record)
			require.NoError(t, err)
			segments[0].NDJSON, segments[0].SHA256 = string(body), hashHex(body)
			var m manifest
			require.NoError(t, json.Unmarshal(envelope.Manifest, &m))
			m.Segments[0] = segments[0].SHA256
			envelope.Manifest, err = json.Marshal(m)
			require.NoError(t, err)
			envelope.Segments, err = json.Marshal(segments)
			require.NoError(t, err)
			data, err = json.Marshal(envelope)
			require.NoError(t, err)
			_, _, err = DecodeSessionPush(data)
			require.Error(t, err)
		})
	}
}
