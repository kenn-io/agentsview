package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
)

func TestCheckpointFloorBootstrapsFromLiveAndQuarantinedNodes(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

func TestExportCheckpointBootstrapReadsLargeSessionMap(t *testing.T) {
	t.Parallel()

	sessions := make(map[string]string, 2000)
	for i := range 2000 {
		sessions[fmt.Sprintf("%s~session-%04d", contractOrigin, i)] = strings64("a")
	}
	body, err := canonicalJSON(checkpoint{
		Version: checkpointFormatVersion, Origin: contractOrigin, Sequence: 42, Sessions: sessions,
	})
	require.NoError(t, err)
	head, err := decodeCanonicalCheckpointHead(strings.NewReader(string(body)), contractOrigin,
		"cp-0000000042.json", identityForBytes(t, body))
	require.NoError(t, err)
	mapBytes, err := canonicalJSON(sessions)
	require.NoError(t, err)
	assert.Equal(t, hashHex(mapBytes), head.SessionMapSHA256)
}

func TestExportCheckpointBootstrapSkipsNoncanonicalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "whitespace",
			body: `{ "origin":"contract-a1b2c3","seq":1,"sessions":{},"v":1}` + "\n",
		},
		{
			name: "escaped field name",
			body: `{"orig\u0069n":"contract-a1b2c3","seq":1,"sessions":{},"v":1}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := testExportDB(t)
			store := newTestArtifactStore(t)
			createCheckpointBody(t, store, 1, []byte(tt.body))

			result, err := ExportToStore(
				t.Context(), database, store,
				ExportOptions{Origin: contractOrigin},
			)
			require.NoError(t, err)
			assert.True(t, result.CheckpointCreated)
			assert.Equal(t, 2, result.CheckpointSequence)
			head, ok, err := database.GetArtifactCheckpointHead(
				t.Context(), contractOrigin,
			)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, 2, head.Sequence)
		})
	}
}

func TestExportCheckpointBootstrapSkipsMalformedCheckpointBeforeEOF(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	store := newTestArtifactStore(t)
	body := append([]byte(`{"unexpected":`), deterministicDocbankBytes(1<<20)...)
	createCheckpointBody(t, store, 1, body)

	result, err := ExportToStore(
		t.Context(), database, store,
		ExportOptions{Origin: contractOrigin},
	)
	require.NoError(t, err)
	assert.True(t, result.CheckpointCreated)
	assert.Equal(t, 2, result.CheckpointSequence)
	head, ok, err := database.GetArtifactCheckpointHead(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, head.Sequence)
}

func TestExportCheckpointBootstrapDefersOnlyValidFutureCheckpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		corrupt    bool
		wantFuture bool
	}{
		{
			name:       "valid future checkpoint",
			body:       `{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":2}` + "\n",
			wantFuture: true,
		},
		{
			name: "valid future checkpoint with added canonical field",
			body: `{"metadata":{"codec":"v2"},"origin":"contract-a1b2c3",` +
				`"seq":1,"sessions":{},"v":2}` + "\n",
			wantFuture: true,
		},
		{
			name: "valid future checkpoint with changed sessions representation",
			body: `{"origin":"contract-a1b2c3","seq":1,"sessions":[` +
				`{"gid":"contract-a1b2c3~session","hash":"sha512:future"}],"v":2}` + "\n",
			wantFuture: true,
		},
		{
			name: "current checkpoint with added canonical field",
			body: `{"metadata":{"codec":"v1"},"origin":"contract-a1b2c3",` +
				`"seq":1,"sessions":{},"v":1}` + "\n",
		},
		{
			name: "current checkpoint with changed sessions representation",
			body: `{"origin":"contract-a1b2c3","seq":1,"sessions":[` +
				`{"gid":"contract-a1b2c3~session","hash":"sha512:future"}],"v":1}` + "\n",
		},
		{
			name: "incomplete future checkpoint",
			body: `{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":2`,
		},
		{
			name: "future checkpoint with trailing JSON",
			body: `{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":2}` + "\n{}",
		},
		{
			name: "future checkpoint with mismatched filename sequence",
			body: `{"origin":"contract-a1b2c3","seq":9,"sessions":{},"v":2}` + "\n",
		},
		{
			name: "noncanonical future checkpoint",
			body: `{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":2}`,
		},
		{
			name:    "corrupt future checkpoint",
			body:    `{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":2}` + "\n",
			corrupt: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := testExportDB(t)
			baseStore := newTestArtifactStore(t)
			createCheckpointBody(t, baseStore, 1, []byte(tt.body))
			var store ArtifactStore = baseStore
			if tt.corrupt {
				store = &checkpointVerifyErrorStore{
					ArtifactStore: baseStore,
					err:           ErrArtifactCorrupt,
				}
			}

			result, err := ExportToStore(
				t.Context(), database, store,
				ExportOptions{Origin: contractOrigin},
			)
			if tt.wantFuture {
				require.ErrorIs(t, err, errFutureArtifactVersion)
				assert.False(t, result.CheckpointCreated)
				page, listErr := firstStoreEntryPage(
					t.Context(), baseStore, contractOrigin, KindCheckpoints, 10,
				)
				require.NoError(t, listErr)
				assert.Len(t, page.Items, 1)
				return
			}

			require.NoError(t, err)
			assert.True(t, result.CheckpointCreated)
			assert.Equal(t, 2, result.CheckpointSequence)
			head, ok, headErr := database.GetArtifactCheckpointHead(
				t.Context(), contractOrigin,
			)
			require.NoError(t, headErr)
			require.True(t, ok)
			assert.Equal(t, 2, head.Sequence)
		})
	}
}

type verifyErrorReader struct {
	VerifiedReader
	err error
}

func (r *verifyErrorReader) Verify() error {
	return errors.Join(r.VerifiedReader.Verify(), r.err)
}

type checkpointVerifyErrorStore struct {
	ArtifactStore
	err error
}

func (s *checkpointVerifyErrorStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	entry, reader, err := s.ArtifactStore.Open(ctx, ref)
	if err != nil || ref.Kind != KindCheckpoints {
		return entry, reader, err
	}
	return entry, &verifyErrorReader{VerifiedReader: reader, err: s.err}, nil
}

// strings64 builds a 64-character stand-in for a sha256 hex digest.
func strings64(ch string) string {
	return strings.Repeat(ch, 64)
}

func TestDecodeSegmentRejectsAggregateNestedLimitsWithSmallLimits(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
		records[ordinal].Version = messageSegmentFormatVersion
		records[ordinal].Ordinal = ordinal
		records[ordinal].Role = "assistant"
		encoded, err := canonicalJSON(records[ordinal])
		require.NoError(t, err)
		_, err = data.Write(encoded)
		require.NoError(t, err)
	}
	return data.Bytes()
}
