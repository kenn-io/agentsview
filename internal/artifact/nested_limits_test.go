package artifact

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func exportSessionToTestStoreWithLimits(
	t *testing.T,
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	origin string,
	limits artifactLimits,
) (string, bool, error) {
	t.Helper()
	sess, err := database.GetSessionFull(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	messages, err := database.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	usage, err := database.GetUsageEvents(ctx, "sess-1")
	require.NoError(t, err)
	return exportLoadedSessionToStore(ctx, store, origin, sess, messages, usage, limits)
}

func TestDecodeSegmentRejectsAggregateNestedLimitsWithSmallLimits(t *testing.T) {
	tests := []struct {
		name      string
		records   []segmentMessage
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "tool calls per segment",
			records: []segmentMessage{
				{ToolCalls: []segmentToolCall{{}}},
				{ToolCalls: []segmentToolCall{{}}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentToolCalls = 1
			},
			wantError: "segment tool call limit",
		},
		{
			name: "result events per segment",
			records: []segmentMessage{
				{ToolCalls: []segmentToolCall{{ResultEvents: []segmentResultEvent{{}}}}},
				{ToolCalls: []segmentToolCall{{ResultEvents: []segmentResultEvent{{}}}}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentResultEvents = 1
			},
			wantError: "segment result event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := productionArtifactLimits()
			tt.configure(&limits)
			data := nestedSegmentData(t, tt.records...)

			_, err := decodeSegmentWithLimits(data, limits)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
		})
	}
}

func TestPeerArtifactDefersFutureNestedSchema(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "peer-a1b2c3"
	data := []byte(`{"v":2,"ordinal":{"future_shape":true},"tool_calls":{"future_shape":true}}` + "\n")
	hash := hashHex(data)
	compressed := compressPeerTestData(t, data)

	ref, err := createCompressedTestArtifact(t, store, origin, KindSegments, hash, compressed)
	require.NoError(t, err)
	assert.Equal(t, hash+".ndjson", ref.Name)
	var stored bytes.Buffer
	require.NoError(t, EncodeWire(t.Context(), ref,
		bytes.NewReader(readContractArtifact(t, store, ref)), &stored))
	assert.Equal(t, compressed, stored.Bytes())
}

func TestDecodeSegmentAcceptsCanonicalTrailingNewlineAndEmptySession(t *testing.T) {
	record := nestedSegmentData(t, segmentMessage{})
	tests := []struct {
		name string
		data []byte
		want int
	}{
		{name: "canonical trailing newline", data: record, want: 1},
		{name: "zero byte empty segment", data: nil, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, err := decodeSegment(tt.data)
			require.NoError(t, err)
			assert.Len(t, msgs, tt.want)
		})
	}
}

func TestImportQuarantinesNestedAmplificationWithoutAdvancingState(t *testing.T) {
	tests := []struct {
		name   string
		record segmentMessage
	}{
		{
			name: "too many tool calls",
			record: segmentMessage{
				ToolCalls: make([]segmentToolCall, 257),
			},
		},
		{
			name: "too many result events",
			record: segmentMessage{
				ToolCalls: []segmentToolCall{{
					ResultEvents: make([]segmentResultEvent, 1_025),
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			localOrigin := "desktop-d4e5f6"
			importDB := testDB(t)
			gid, segmentRef := writeNestedImportFixture(
				t, store, origin, tt.record,
			)

			res, err := importResultFromTestStore(ctx, importDB, store, localOrigin)
			require.NoError(t, err)
			assert.False(t, res.Changed())
			state, err := importDB.GetSyncState(importStateKey(origin, gid))
			require.NoError(t, err)
			assert.Empty(t, state)
			got, err := importDB.GetSessionFull(ctx, gid)
			require.NoError(t, err)
			assert.Nil(t, got)
			_, err = store.Stat(ctx, segmentRef)
			assert.ErrorIs(t, err, ErrArtifactNotFound)
		})
	}
}

func TestExportRejectsNestedAmplificationBeforePublication(t *testing.T) {
	tests := []struct {
		name      string
		message   db.Message
		wantError string
	}{
		{
			name: "too many tool calls in one message",
			message: db.Message{
				ToolCalls: make([]db.ToolCall, 257),
			},
			wantError: "tool call limit exceeded for message ordinal 0",
		},
		{
			name: "too many result events in one tool call",
			message: db.Message{
				ToolCalls: []db.ToolCall{{
					ResultEvents: make([]db.ToolResultEvent, 1_025),
				}},
			},
			wantError: "result event limit exceeded for tool call 0 in message ordinal 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			message := tt.message
			message.SessionID = "sess-1"
			message.Ordinal = 0
			message.Role = "assistant"
			require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{message}))

			_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
			assertNoPublishedArtifacts(t, store, origin)
		})
	}
}

func TestExportChunksOnAggregateNestedLimitsWithSmallLimits(t *testing.T) {
	tests := []struct {
		name         string
		resultEvents []db.ToolResultEvent
		configure    func(*artifactLimits)
	}{
		{
			name: "tool calls per segment",
			configure: func(limits *artifactLimits) {
				limits.segmentToolCalls = 2
			},
		},
		{
			name:         "result events per segment",
			resultEvents: []db.ToolResultEvent{{EventIndex: 0}},
			configure: func(limits *artifactLimits) {
				limits.segmentResultEvents = 2
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			msgs := make([]db.Message, 3)
			for ordinal := range msgs {
				msgs[ordinal] = db.Message{
					SessionID: "sess-1",
					Ordinal:   ordinal,
					Role:      "assistant",
					ToolCalls: []db.ToolCall{{
						ResultEvents: tt.resultEvents,
					}},
				}
			}
			require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))
			limits := productionArtifactLimits()
			tt.configure(&limits)

			manifestHash, changed, err := exportSessionToTestStoreWithLimits(
				t, ctx, database, store, origin, limits,
			)
			require.NoError(t, err)
			assert.True(t, changed)
			manifestRef, err := NewRef(origin, KindManifests, manifestHash+".json")
			require.NoError(t, err)
			m, err := decodeManifestWithLimits(
				readContractArtifact(t, store, manifestRef), productionArtifactLimits(),
			)
			require.NoError(t, err)
			require.Len(t, m.Segments, 2)
			got := testStoreManifestMessages(t, store, origin, m)
			require.Len(t, got, 3)
			for ordinal := range got {
				assert.Equal(t, ordinal, got[ordinal].Ordinal)
				require.Len(t, got[ordinal].ToolCalls, 1)
				assert.Len(t, got[ordinal].ToolCalls[0].ResultEvents, len(tt.resultEvents))
			}
		})
	}
}

func TestExportRejectsMessageThatCannotFitNestedSegmentLimits(t *testing.T) {
	tests := []struct {
		name      string
		message   db.Message
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "tool calls cannot split across segments",
			message: db.Message{
				ToolCalls: []db.ToolCall{{}, {}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentToolCalls = 1
			},
			wantError: "2 tool calls",
		},
		{
			name: "one tool result history cannot split across segments",
			message: db.Message{
				ToolCalls: []db.ToolCall{{
					ResultEvents: []db.ToolResultEvent{{EventIndex: 0}, {EventIndex: 1}},
				}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentResultEvents = 1
			},
			wantError: "2 result events",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			message := tt.message
			message.SessionID = "sess-1"
			message.Ordinal = 0
			message.Role = "assistant"
			require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{message}))
			limits := productionArtifactLimits()
			tt.configure(&limits)

			_, _, err := exportSessionToTestStoreWithLimits(
				t, ctx, database, store, origin, limits,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot fit in one segment")
			assert.Contains(t, err.Error(), tt.wantError)
			assertNoPublishedArtifacts(t, store, origin)
		})
	}
}

func TestExportRejectsSessionNestedLimitsBeforeWritingWithSmallLimits(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "tool calls per session",
			configure: func(limits *artifactLimits) {
				limits.sessionToolCalls = 1
			},
			wantError: "session tool call limit",
		},
		{
			name: "result events per session",
			configure: func(limits *artifactLimits) {
				limits.sessionResultEvents = 1
			},
			wantError: "session result event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			msgs := make([]db.Message, 2)
			for ordinal := range msgs {
				msgs[ordinal] = db.Message{
					SessionID: "sess-1",
					Ordinal:   ordinal,
					Role:      "assistant",
					ToolCalls: []db.ToolCall{{
						ResultEvents: []db.ToolResultEvent{{EventIndex: 0}},
					}},
				}
			}
			require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))
			limits := productionArtifactLimits()
			tt.configure(&limits)

			_, _, err := exportSessionToTestStoreWithLimits(
				t, ctx, database, store, origin, limits,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
			assertNoPublishedArtifacts(t, store, origin)
		})
	}
}

func nestedSegmentData(t *testing.T, records ...segmentMessage) []byte {
	t.Helper()
	var data bytes.Buffer
	for ordinal := range records {
		records[ordinal].Version = formatVersion
		records[ordinal].Ordinal = ordinal
		records[ordinal].Role = "assistant"
		encoded, err := canonicalJSON(records[ordinal])
		require.NoError(t, err)
		_, err = data.Write(encoded)
		require.NoError(t, err)
	}
	return data.Bytes()
}

func writeNestedImportFixture(
	t *testing.T,
	store ArtifactStore,
	origin string,
	record segmentMessage,
) (string, Ref) {
	t.Helper()
	data := nestedSegmentData(t, record)
	segmentHash := hashHex(data)
	segmentRef, err := NewRef(origin, KindSegments, segmentHash+".ndjson")
	require.NoError(t, err)
	createContractArtifact(t, store, segmentRef, data)

	gid := origin + "~sess-1"
	m := manifest{
		Version:         formatVersion,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session: manifestSession{
			ID:      "sess-1",
			Machine: origin,
		},
		Segments: []string{segmentHash},
	}
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	manifestRef, err := NewRef(origin, KindManifests, manifestHash+".json")
	require.NoError(t, err)
	createContractArtifact(t, store, manifestRef, manifestData)
	cpData, err := canonicalJSON(checkpoint{
		Version:  formatVersion,
		Origin:   origin,
		Sequence: 1,
		Sessions: map[string]string{gid: manifestHash},
	})
	require.NoError(t, err)
	cpRef, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	createContractArtifact(t, store, cpRef, cpData)
	return gid, segmentRef
}
