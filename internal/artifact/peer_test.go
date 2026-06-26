package artifact

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestPeerArtifactWriteReadManifestDuplicate(t *testing.T) {
	root := t.TempDir()
	origin := "peer-a1b2c3"
	startedAt := "2026-06-14T01:02:03Z"
	segmentData, err := encodeSegment([]db.Message{{
		Ordinal: 0,
		Role:    "user",
		Content: "hello",
	}})
	require.NoError(t, err)
	segmentHash := hashHex(segmentData)
	manifestData, err := canonicalJSON(manifest{
		Version:         formatVersion,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session: db.Session{
			ID:                "sess-1",
			Machine:           origin,
			Agent:             "claude",
			Project:           "alpha",
			StartedAt:         &startedAt,
			CreatedAt:         "2026-06-14T01:02:03Z",
			MessageCount:      1,
			UserMessageCount:  1,
			TotalOutputTokens: 0,
			PeakContextTokens: 0,
		},
		Segments:    []string{segmentHash},
		DataVersion: 1,
		Generation:  1,
	})
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	compressed := compressPeerTestData(t, manifestData)

	res, err := WriteArtifact(root, origin, "manifest", manifestHash, compressed)
	require.NoError(t, err)
	assert.False(t, res.Duplicate)
	assert.Equal(t, KindManifests, res.Kind)
	assert.Equal(t, manifestHash+manifestExtension, res.Name)

	res, err = WriteArtifact(root, origin, "manifests", manifestHash+manifestExtension, compressed)
	require.NoError(t, err)
	assert.True(t, res.Duplicate)

	got, err := ReadArtifact(root, origin, "manifests", manifestHash)
	require.NoError(t, err)
	assert.Equal(t, "application/zstd", got.ContentType)
	assert.Equal(t, compressed, got.Data)
}

func TestPeerArtifactRejectsHashMismatch(t *testing.T) {
	root := t.TempDir()
	origin := "peer-a1b2c3"
	data := compressPeerTestData(t, []byte("not the named hash\n"))

	_, err := WriteArtifact(root, origin, "segments", strings64("0"), data)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestPeerArtifactCheckpointDuplicateAndConflict(t *testing.T) {
	root := t.TempDir()
	origin := "peer-a1b2c3"
	first := []byte(`{"origin":"peer-a1b2c3","seq":1,"sessions":{},"v":1}` + "\n")
	second := []byte(`{"origin":"peer-a1b2c3","seq":1,"sessions":{"peer-a1b2c3~sess-1":"` + strings64("a") + `"},"v":1}` + "\n")

	res, err := WriteArtifact(root, origin, "checkpoints", "cp-0000000001", first)
	require.NoError(t, err)
	assert.False(t, res.Duplicate)

	res, err = WriteArtifact(root, origin, "checkpoints", "cp-0000000001.json", first)
	require.NoError(t, err)
	assert.True(t, res.Duplicate)

	_, err = WriteArtifact(root, origin, "checkpoints", "cp-0000000001", second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactConflict)
}

func TestPeerArtifactMetadataMustMatchOrigin(t *testing.T) {
	root := t.TempDir()
	origin := "peer-a1b2c3"
	body := []byte(`{"hlc":"2026-06-14T010203.000000001Z-other-b2c3d4","op":"rename","origin":"other-b2c3d4","session_gid":"other-b2c3d4~sess-1","v":1,"value":{"display_name":"Remote"}}` + "\n")
	hash := hashHex(body)
	name := "2026-06-14T010203.000000001Z-other-b2c3d4-" + hash

	_, err := WriteArtifact(root, origin, "meta", name, body)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestPeerArtifactStoresFutureVersionArtifacts(t *testing.T) {
	root := t.TempDir()
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
	res, err := WriteArtifact(root, origin, KindCheckpoints, "cp-0000000001", checkpointData)
	require.NoError(t, err)
	assert.False(t, res.Duplicate)

	got, err := ReadArtifact(root, origin, KindCheckpoints, "cp-0000000001")
	require.NoError(t, err)
	assert.Equal(t, checkpointData, got.Data)

	segmentData, err := canonicalJSON(segmentMessage{
		Version: formatVersion + 1,
		Ordinal: 0,
		Role:    "user",
		Content: "future segment",
	})
	require.NoError(t, err)
	compressedSegment := compressPeerTestData(t, segmentData)
	segmentHash := hashHex(segmentData)
	res, err = WriteArtifact(root, origin, KindSegments, segmentHash, compressedSegment)
	require.NoError(t, err)
	assert.Equal(t, segmentHash+segmentExtension, res.Name)
	got, err = ReadArtifact(root, origin, KindSegments, segmentHash)
	require.NoError(t, err)
	assert.Equal(t, compressedSegment, got.Data)

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
	res, err = WriteArtifact(root, origin, KindManifests, manifestHash, compressedManifest)
	require.NoError(t, err)
	assert.Equal(t, manifestHash+manifestExtension, res.Name)
	got, err = ReadArtifact(root, origin, KindManifests, manifestHash)
	require.NoError(t, err)
	assert.Equal(t, compressedManifest, got.Data)

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
	res, err = WriteArtifact(root, origin, KindMeta, metadataName, metadataData)
	require.NoError(t, err)
	assert.Equal(t, metadataName+metadataEventExtension, res.Name)
	got, err = ReadArtifact(root, origin, KindMeta, metadataName)
	require.NoError(t, err)
	assert.Equal(t, metadataData, got.Data)
}

func TestPeerArtifactRejectsMixedFutureSegmentVersions(t *testing.T) {
	root := t.TempDir()
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

	_, err = WriteArtifact(root, origin, KindSegments, hash, compressed)
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
