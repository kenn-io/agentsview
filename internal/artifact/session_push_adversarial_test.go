package artifact

import (
	"encoding/json/v2"
	"fmt"
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
	valid := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", PRLinks: links[:256]}, nil)
	_, _, err := DecodeSessionPush(valid)
	require.NoError(t, err)
	data := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", PRLinks: links}, nil)
	_, _, err = DecodeSessionPush(data)
	require.ErrorContains(t, err, "session push PR link limit exceeded")
}

func TestSessionPushRejectsEmbeddedResultCollectionAmplification(t *testing.T) {
	array := "[" + strings.Repeat("{},", 4096) + "{}]"
	for name, content := range map[string]string{
		"array":              array,
		"section":            "worker:\n" + array,
		"nul-array":          "\x00" + array,
		"controls-array":     "\x01\x7f\u0085" + array,
		"normalized-section": "\x00worker:\n" + array,
	} {
		for _, event := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/event=%t", name, event), func(t *testing.T) {
				call := db.ToolCall{ToolName: "Read", ResultContent: "safe", ResultEvents: []db.ToolResultEvent{{Content: "safe"}}}
				data := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1}, []db.Message{{Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{call}}})
				_, _, err := DecodeSessionPush(data)
				require.NoError(t, err)
				var envelope SessionPushEnvelope
				require.NoError(t, json.Unmarshal(data, &envelope))
				var segments []sessionPushSegment
				require.NoError(t, json.Unmarshal(envelope.Segments, &segments))
				var record segmentMessage
				require.NoError(t, json.Unmarshal([]byte(segments[0].NDJSON), &record))
				if event {
					record.ToolCalls[0].ResultEvents[0].Content = content
				} else {
					record.ToolCalls[0].ResultContent = content
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
				require.ErrorContains(t, err, "result collection exceeds limit")
			})
		}
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
