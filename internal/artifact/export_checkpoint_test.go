package artifact

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
)

func TestCheckpointFloorBootstrapsFromLiveAndQuarantinedNodes(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	database := testDB(t)
	origin := contractOrigin
	for sequence := 1; sequence <= checkpointFloorPageSize+2; sequence++ {
		body := fmt.Appendf(nil, `{"v":1,"origin":%q,"sequence":%d,"sessions":{}}`, origin, sequence)
		createCheckpointBody(t, store, sequence, body)
	}
	quarantinedName := fmt.Sprintf("cp-%010d.json", checkpointFloorPageSize+2)
	require.NoError(t, store.Quarantine(t.Context(),
		requireContractRef(t, origin, KindCheckpoints, quarantinedName),
		"test quarantine"))

	sequence, err := reserveCheckpointSequenceFromStore(
		t.Context(), database, store, origin,
	)
	require.NoError(t, err)
	assert.Equal(t, 131, sequence)

	// A fresh vault may report no sequence after reset or quarantine expiry, but
	// the SQLite floor remains authoritative and may never be lowered.
	_, emptyStore := newTestDocbankStore(t, docbank.Config{})
	sequence, err = reserveCheckpointSequenceFromStore(
		t.Context(), database, emptyStore, origin,
	)
	require.NoError(t, err)
	assert.Equal(t, 132, sequence)

	// If both SQLite and the vault are lost simultaneously, local prevention is
	// impossible; peer common-checkpoint conflict handling is the final backstop.
}

func TestCheckpointFloorTraversesStoreOnlyBeforeBootstrap(t *testing.T) {
	database := testDB(t)
	store := &countingCheckpointFloorStore{floor: 40}

	sequence, err := reserveCheckpointSequenceFromStore(
		t.Context(), database, store, contractOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, 41, sequence)
	sequence, err = reserveCheckpointSequenceFromStore(
		t.Context(), database, store, contractOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, 42, sequence)
	assert.Equal(t, 1, store.calls, "durable floor avoids repeated vault traversal")
}

type countingCheckpointFloorStore struct {
	ArtifactStore
	floor int
	calls int
}

func (s *countingCheckpointFloorStore) checkpointFloor(context.Context, string) (int, error) {
	s.calls++
	return s.floor, nil
}

func TestExportCheckpointBootstrapStreamsLargeSessionMap(t *testing.T) {
	sessions := make(map[string]string, 2000)
	for i := range 2000 {
		sessions[fmt.Sprintf("%s~session-%04d", contractOrigin, i)] = strings64("a")
	}
	body, err := canonicalJSON(checkpoint{
		Version: formatVersion, Origin: contractOrigin, Sequence: 42, Sessions: sessions,
	})
	require.NoError(t, err)
	reader := &maxReadReader{reader: strings.NewReader(string(body))}
	head, err := decodeCanonicalCheckpointHead(reader, contractOrigin,
		"cp-0000000042.json", identityForBytes(t, body))
	require.NoError(t, err)
	mapBytes, err := canonicalJSON(sessions)
	require.NoError(t, err)
	assert.Equal(t, hashHex(mapBytes), head.SessionMapSHA256)
	assert.Less(t, reader.max, len(body)/4,
		"bootstrap must tokenize the checkpoint instead of reading its full body")
}

type maxReadReader struct {
	reader io.Reader
	max    int
}

func (r *maxReadReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		r.max = len(p)
	}
	return r.reader.Read(p)
}

// strings64 builds a 64-character stand-in for a sha256 hex digest.
func strings64(ch string) string {
	return strings.Repeat(ch, 64)
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
