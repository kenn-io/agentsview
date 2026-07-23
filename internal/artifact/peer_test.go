package artifact

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPeerArtifactRejectsInvalidReferences(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "peer-a1b2c3"

	checkpointData := []byte(`{"origin":"peer-a1b2c3","seq":1,"sessions":{"peer-a1b2c3~sess-1":"../outside"},"v":1}` + "\n")
	_, err := createCompressedTestArtifact(
		t, store, origin, KindCheckpoints, "cp-0000000001.json", checkpointData,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)

	manifestData, err := canonicalJSON(manifest{
		Version:         formatVersion,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session: manifestSession{
			ID:        "sess-1",
			Machine:   origin,
			Agent:     "claude",
			Project:   "alpha",
			CreatedAt: "2026-06-14T01:02:03Z",
		},
		Segments: []string{"../outside"},
	})
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	_, err = createCompressedTestArtifact(
		t, store, origin, KindManifests, manifestHash, compressPeerTestData(t, manifestData),
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestPeerArtifactMetadataMustMatchOrigin(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "peer-a1b2c3"
	body := []byte(`{"hlc":"2026-06-14T010203.000000001Z-other-b2c3d4","op":"rename","origin":"other-b2c3d4","session_gid":"other-b2c3d4~sess-1","v":1,"value":{"display_name":"Remote"}}` + "\n")
	hash := hashHex(body)
	name := "2026-06-14T010203.000000001Z-other-b2c3d4-" + hash

	_, err := createCompressedTestArtifact(t, store, origin, KindMeta, name, body)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestPeerArtifactMetadataRejectsMalformedKnownOpPayload(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "peer-a1b2c3"
	hlc := "2026-06-14T010203.000000001Z-peer-a1b2c3"

	tests := []struct {
		name  string
		op    string
		value json.RawMessage
	}{
		{name: "pin missing payload", op: MetadataOpPin},
		{name: "unpin missing payload", op: MetadataOpUnpin},
		{name: "rename non-object value", op: MetadataOpRename, value: json.RawMessage(`[1,2]`)},
		{name: "rename missing value", op: MetadataOpRename},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := canonicalJSON(metadataEvent{
				Version:    formatVersion,
				HLC:        hlc,
				Origin:     origin,
				SessionGID: origin + "~sess-1",
				Op:         tt.op,
				Value:      tt.value,
			})
			require.NoError(t, err)
			name := hlc + "-" + hashHex(data)
			_, err = createCompressedTestArtifact(t, store, origin, KindMeta, name, data)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
		})
	}
}

func TestPeerArtifactStoresFutureVersionArtifacts(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "peer-a1b2c3"

	checkpointData, err := canonicalJSON(checkpoint{
		Version:  formatVersion + 1,
		Origin:   origin,
		Sequence: 1,
		Sessions: map[string]string{
			origin + "~sess-1": strings64("a"),
		},
	})
	require.NoError(t, err)
	checkpointRef, err := createCompressedTestArtifact(
		t, store, origin, KindCheckpoints, "cp-0000000001.json", checkpointData,
	)
	require.NoError(t, err)
	assert.Equal(t, checkpointData, readContractArtifact(t, store, checkpointRef))

	segmentData, err := canonicalJSON(segmentMessage{
		Version: formatVersion + 1,
		Ordinal: 0,
		Role:    "user",
		Content: "future segment",
	})
	require.NoError(t, err)
	compressedSegment := compressPeerTestData(t, segmentData)
	segmentHash := hashHex(segmentData)
	segmentRef, err := createCompressedTestArtifact(
		t, store, origin, KindSegments, segmentHash, compressedSegment,
	)
	require.NoError(t, err)
	assert.Equal(t, segmentHash+".ndjson", segmentRef.Name)
	assert.Equal(t, segmentData, readContractArtifact(t, store, segmentRef))

	futureManifestData, err := canonicalJSON(struct {
		Version int    `json:"v"`
		Origin  string `json:"origin"`
		Future  string `json:"future"`
	}{
		Version: formatVersion + 1,
		Origin:  origin,
		Future:  "schema-owned-by-newer-peer",
	})
	require.NoError(t, err)
	compressedManifest := compressPeerTestData(t, futureManifestData)
	manifestHash := hashHex(futureManifestData)
	manifestRef, err := createCompressedTestArtifact(
		t, store, origin, KindManifests, manifestHash, compressedManifest,
	)
	require.NoError(t, err)
	assert.Equal(t, manifestHash+".json", manifestRef.Name)
	assert.Equal(t, futureManifestData, readContractArtifact(t, store, manifestRef))

	hlc := "2026-06-14T010203.000000001Z-peer-a1b2c3"
	metadataData, err := canonicalJSON(metadataEvent{
		Version:    formatVersion + 1,
		HLC:        hlc,
		Origin:     origin,
		SessionGID: origin + "~sess-1",
		Op:         "future_op",
		Value:      json.RawMessage(`{"future":true}`),
	})
	require.NoError(t, err)
	metadataHash := hashHex(metadataData)
	metadataName := hlc + "-" + metadataHash
	metadataRef, err := createCompressedTestArtifact(
		t, store, origin, KindMeta, metadataName, metadataData,
	)
	require.NoError(t, err)
	assert.Equal(t, metadataName+metadataEventExtension, metadataRef.Name)
	assert.Equal(t, metadataData, readContractArtifact(t, store, metadataRef))
}

func TestPeerArtifactStoresFutureMetadataWithoutCurrentEnvelope(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "peer-a1b2c3"
	data, err := canonicalJSON(struct {
		Version     int    `json:"v"`
		FutureClock string `json:"future_clock"`
		FutureKey   string `json:"future_key"`
	}{
		Version:     formatVersion + 1,
		FutureClock: "schema-owned-by-newer-peer",
		FutureKey:   origin + "~sess-1",
	})
	require.NoError(t, err)
	hash := hashHex(data)
	name := "2026-06-14T010203.000000001Z-peer-a1b2c3-" + hash

	ref, err := createCompressedTestArtifact(t, store, origin, KindMeta, name, data)
	require.NoError(t, err)
	assert.Equal(t, data, readContractArtifact(t, store, ref))
}

func TestPeerArtifactRejectsMixedFutureSegmentVersions(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "peer-a1b2c3"
	futureLine, err := canonicalJSON(segmentMessage{
		Version: formatVersion + 1,
		Ordinal: 0,
		Role:    "user",
		Content: "future",
	})
	require.NoError(t, err)
	currentLine, err := canonicalJSON(segmentMessage{
		Version: formatVersion,
		Ordinal: 1,
		Role:    "assistant",
		Content: "current",
	})
	require.NoError(t, err)
	segmentData := append(futureLine, currentLine...)
	compressed := compressPeerTestData(t, segmentData)
	hash := hashHex(segmentData)

	_, err = createCompressedTestArtifact(t, store, origin, KindSegments, hash, compressed)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func compressPeerTestData(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	require.NoError(t, err)
	_, err = enc.Write(data)
	require.NoError(t, err)
	require.NoError(t, enc.Close())
	return buf.Bytes()
}

func strings64(ch string) string {
	return strings.Repeat(ch, 64)
}
