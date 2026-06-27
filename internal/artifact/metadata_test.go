package artifact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetadataRecorderAppendWritesCanonicalEvent(t *testing.T) {
	database := testDB(t)
	dataDir := t.TempDir()
	now := fixedHLCTime()
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		DataDir: dataDir,
		Origin:  "laptop-a1b2c3",
		Now:     func() time.Time { return now },
	})

	value := json.RawMessage(`{"display_name":"Renamed session"}`)
	record, err := recorder.Append(context.Background(), MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpRename,
		Value:     value,
	})
	require.NoError(t, err)

	assert.Equal(t, "2026-06-14T010203.000000001Z-00000000000000000000", record.HLC)
	assert.Equal(t, "laptop-a1b2c3", record.Origin)
	assert.Equal(t, "laptop-a1b2c3~sess-1", record.SessionGID)
	assert.Equal(t, MetadataOpRename, record.Op)

	data, err := os.ReadFile(record.Path)
	require.NoError(t, err)
	assert.Equal(t,
		"{\"hlc\":\"2026-06-14T010203.000000001Z-00000000000000000000\",\"op\":\"rename\",\"origin\":\"laptop-a1b2c3\",\"session_gid\":\"laptop-a1b2c3~sess-1\",\"v\":1,\"value\":{\"display_name\":\"Renamed session\"}}\n",
		string(data),
	)
	assert.Equal(t, hashHex(data), record.Hash)
	assert.Equal(t,
		filepath.Join(dataDir, "artifacts", "laptop-a1b2c3", "meta", record.HLC+"-"+record.Hash+".json"),
		record.Path,
	)
	// The metadata event filename must be safe on every supported OS,
	// including Windows, which forbids these characters in path components.
	assert.NotContains(t, filepath.Base(record.Path), ":",
		"metadata filename must not contain ':' (invalid on Windows)")
	for _, c := range `<>:"/\|?*` {
		assert.NotContainsf(t, record.HLC, string(c),
			"HLC %q must not contain %q (invalid in Windows filenames)", record.HLC, string(c))
	}
}

func TestImportObservesRemoteHLCForLaterLocalEdits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataDir := t.TempDir()
	localOrigin := "desktop-d4e5f6"
	peerOrigin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	peerGID := localOrigin + "~sess-1"

	recorderNow := fixedHLCTime()
	// A peer event whose wall time is ahead of the recorder's local clock but
	// within the drift bound.
	remoteStamp := HLCTimestamp{WallTime: recorderNow.Add(2 * time.Minute), Logical: 5}
	writeMetadataArtifact(t, filepath.Join(root, peerOrigin),
		replayRenameEvent(t, peerOrigin, peerGID, remoteStamp.String(), "Peer name"))

	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		DataDir: dataDir,
		Origin:  localOrigin,
		Now:     func() time.Time { return recorderNow },
	})

	imported, err := recorder.Import(ctx, root)
	require.NoError(t, err)
	assert.Equal(t, 1, imported.Metadata)

	// The next local edit must receive an HLC strictly after the observed
	// remote HLC even though the local wall clock is behind it.
	rec, err := recorder.Append(ctx, MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpStar,
	})
	require.NoError(t, err)
	localStamp, err := ParseHLCTimestamp(rec.HLC)
	require.NoError(t, err)
	assert.Equal(t, 1, localStamp.Compare(remoteStamp),
		"local HLC %s must be after observed remote HLC %s", rec.HLC, remoteStamp.String())
}

func TestMetadataSessionGID(t *testing.T) {
	assert.Equal(t, "desk-a1b2c3~sess-1", MetadataSessionGID("desk-a1b2c3", "sess-1"))
	assert.Equal(t, "laptop-d4e5f6~sess-1", MetadataSessionGID("desk-a1b2c3", "laptop-d4e5f6~sess-1"))
}
