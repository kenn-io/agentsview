package artifact

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func createCompressedTestArtifact(
	t *testing.T, store ArtifactStore, origin string, kind Kind, hash string, encoded []byte,
) (Ref, error) {
	t.Helper()
	name := hash
	switch kind {
	case KindManifests:
		name += ".json"
	case KindSegments:
		name += ".ndjson"
	case KindMeta:
		if !strings.HasSuffix(name, metadataEventExtension) {
			name += metadataEventExtension
		}
	}
	ref, err := NewRef(origin, kind, name)
	require.NoError(t, err)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	_, err = CreateFromWire(
		t.Context(), store, wire, bytes.NewReader(encoded), transportWireLimits(kind),
	)
	return ref, err
}

func assertArtifactNotStored(t *testing.T, store ArtifactStore, ref Ref) {
	t.Helper()
	_, err := store.Stat(t.Context(), ref)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
}

func latestTestStoreManifest(t *testing.T, store ArtifactStore, origin string) manifest {
	t.Helper()
	_, cp, err := latestStoreCheckpointSummary(t.Context(), store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)
	hash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, hash)
	ref, err := NewRef(origin, KindManifests, hash+".json")
	require.NoError(t, err)
	m, err := decodeManifestWithLimits(readContractArtifact(t, store, ref), productionArtifactLimits())
	require.NoError(t, err)
	return m
}

func testStoreManifestMessages(t *testing.T, store ArtifactStore, origin string, m manifest) []db.Message {
	t.Helper()
	var messages []db.Message
	for _, hash := range m.Segments {
		ref, err := NewRef(origin, KindSegments, hash+".ndjson")
		require.NoError(t, err)
		segment, err := decodeSegment(readContractArtifact(t, store, ref))
		require.NoError(t, err)
		messages = append(messages, segment...)
	}
	return messages
}

func assertNoPublishedArtifacts(t *testing.T, store ArtifactStore, origin string) {
	t.Helper()
	for _, kind := range transportKinds {
		page, err := store.List(t.Context(), origin, kind, "", maxArtifactListPageSize)
		require.NoError(t, err)
		assert.Empty(t, page.Items, "unexpected published %s artifact", kind)
		require.Empty(t, page.Next)
	}
}

func assertNoPublishedAuthority(t *testing.T, store ArtifactStore, origin string) {
	t.Helper()
	for _, kind := range []Kind{KindManifests, KindCheckpoints} {
		page, err := store.List(t.Context(), origin, kind, "", maxArtifactListPageSize)
		require.NoError(t, err)
		assert.Empty(t, page.Items, "unexpected published %s artifact", kind)
		require.Empty(t, page.Next)
	}
}

func TestWriteArtifactRejectsManifestStructuralAmplification(t *testing.T) {
	origin := "peer-a1b2c3"
	validHash := strings64("a")
	tests := []struct {
		name      string
		manifest  manifest
		wantError string
	}{
		{
			name: "duplicate segment references",
			manifest: manifest{
				Segments: []string{validHash, validHash},
			},
			wantError: "duplicate segment reference",
		},
		{
			name: "too many segment references",
			manifest: manifest{
				Segments: syntheticSegmentHashes(17),
			},
			wantError: "segment reference limit",
		},
		{
			name: "too many usage events",
			manifest: manifest{
				Segments:    []string{validHash},
				UsageEvents: make([]artifactUsageEvent, 32_769),
			},
			wantError: "usage event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestArtifactStore(t)
			m := tt.manifest
			m.Version = formatVersion
			m.Origin = origin
			m.NativeSessionID = "sess-1"
			m.Session = manifestSession{ID: "sess-1", Machine: origin}
			data, err := canonicalJSON(m)
			require.NoError(t, err)
			hash := hashHex(data)

			ref, err := createCompressedTestArtifact(
				t, store, origin, KindManifests, hash, compressPeerTestData(t, data),
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
			assert.Contains(t, err.Error(), tt.wantError)
			assertArtifactNotStored(t, store, ref)
		})
	}
}

func TestWriteArtifactRejectsSegmentRecordAmplification(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "peer-a1b2c3"
	data := syntheticSegmentRecords(t, 4_097)
	hash := hashHex(data)

	ref, err := createCompressedTestArtifact(
		t, store, origin, KindSegments, hash, compressPeerTestData(t, data),
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.Contains(t, err.Error(), "message record limit")
	assertArtifactNotStored(t, store, ref)
}

func TestWriteArtifactRejectsSegmentNestedAmplificationWithoutWriting(t *testing.T) {
	origin := "peer-a1b2c3"
	tests := []struct {
		name      string
		record    segmentMessage
		wantError string
	}{
		{
			name: "too many tool calls in one message",
			record: segmentMessage{
				ToolCalls: make([]segmentToolCall, 257),
			},
			wantError: "tool call limit",
		},
		{
			name: "too many result events in one tool call",
			record: segmentMessage{
				ToolCalls: []segmentToolCall{{
					ResultEvents: make([]segmentResultEvent, 1_025),
				}},
			},
			wantError: "result event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestArtifactStore(t)
			record := tt.record
			record.Version = formatVersion
			record.Ordinal = 7
			record.Role = "assistant"
			data, err := canonicalJSON(record)
			require.NoError(t, err)
			hash := hashHex(data)

			ref, err := createCompressedTestArtifact(
				t, store, origin, KindSegments, hash, compressPeerTestData(t, data),
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
			assert.Contains(t, err.Error(), tt.wantError)
			assertArtifactNotStored(t, store, ref)
		})
	}
}

func TestWriteArtifactRejectsBlankSegmentRecordsWithoutWriting(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "peer-a1b2c3"
	data := bytes.Repeat([]byte("\n"), 4_097)
	hash := hashHex(data)

	ref, err := createCompressedTestArtifact(
		t, store, origin, KindSegments, hash, compressPeerTestData(t, data),
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.Contains(t, err.Error(), "blank message record")
	assertArtifactNotStored(t, store, ref)
}

func TestExportChunksOnMessageRecordLimit(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	msgs := make([]db.Message, 4_097)
	for i := range msgs {
		msgs[i] = db.Message{SessionID: "sess-1", Ordinal: i, Role: "user"}
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	m := latestTestStoreManifest(t, store, origin)
	require.Len(t, m.Segments, 2)
	got := testStoreManifestMessages(t, store, origin, m)
	assert.Len(t, got, 4_097)
}

func TestExportRejectsOversizedGeneratedManifestBeforePublication(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha", func(sess *db.Session) {
		first := strings.Repeat("x", int(manifestDecodedLimit))
		sess.FirstMessage = &first
	})

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generated manifest exceeds")
	assertNoPublishedAuthority(t, store, origin)
}

func TestExportRejectsSessionMessageAmplificationBeforePublication(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	msgs := make([]db.Message, 32_769)
	for i := range msgs {
		msgs[i] = db.Message{SessionID: "sess-1", Ordinal: i, Role: "user"}
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session message limit")
	assertNoPublishedArtifacts(t, store, origin)
}

func TestExportRejectsUsageEventAmplificationBeforePublication(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	events := make([]db.UsageEvent, 32_769)
	for i := range events {
		events[i] = db.UsageEvent{
			SessionID: "sess-1",
			Source:    "fixture",
			DedupKey:  fmt.Sprintf("usage-%d", i),
		}
	}
	require.NoError(t, database.ReplaceSessionUsageEvents("sess-1", events))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage event limit")
	assertNoPublishedArtifacts(t, store, origin)
}

func TestExportSessionRejectsAggregateLimitsBeforeWritingWithSmallLimits(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "decoded bytes",
			configure: func(limits *artifactLimits) {
				limits.sessionDecodedBytes = 1
			},
			wantError: "session decoded byte limit",
		},
		{
			name: "segment references",
			configure: func(limits *artifactLimits) {
				limits.segmentMessages = 1
				limits.manifestSegments = 1
			},
			wantError: "segment reference limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			limits := productionArtifactLimits()
			tt.configure(&limits)

			sess, err := database.GetSessionFull(ctx, "sess-1")
			require.NoError(t, err)
			require.NotNil(t, sess)
			messages, err := database.GetAllMessages(ctx, "sess-1")
			require.NoError(t, err)
			_, _, err = exportLoadedSessionToStore(
				ctx, store, origin, sess, messages, nil, limits,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
			assertNoPublishedAuthority(t, store, origin)
		})
	}
}

func TestImportQuarantinesOversizedSegmentWithoutAdvancingState(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	importDB := testDB(t)

	prefix, err := canonicalJSON(segmentMessage{
		Version:       formatVersion,
		Ordinal:       0,
		Role:          "user",
		Content:       "hello",
		ContentLength: 5,
	})
	require.NoError(t, err)
	segmentData := append(prefix, bytes.Repeat([]byte("\n"), 64<<20+1-len(prefix))...)
	segmentHash := hashHex(segmentData)
	segmentRef, err := NewRef(origin, KindSegments, segmentHash+".ndjson")
	require.NoError(t, err)
	createContractArtifact(t, store, segmentRef, segmentData)

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

	res, err := ImportDetailedFromStore(ctx, importDB, store, localOrigin)
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
}

func TestExportChunksLargeMultiMessageSessionInOrder(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	content := strings.Repeat("x", 9<<20)
	msgs := make([]db.Message, 4)
	for i := range msgs {
		msgs[i] = db.Message{
			SessionID:     "sess-1",
			Ordinal:       i,
			Role:          "user",
			Content:       content,
			ContentLength: len(content),
		}
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))

	exportResult, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	assert.Equal(t, 1, exportResult.ExportedSessions)
	m := latestTestStoreManifest(t, store, origin)
	require.Len(t, m.Segments, 2)

	got := testStoreManifestMessages(t, store, origin, m)
	require.Len(t, got, 4)
	for i := range got {
		assert.Equal(t, i, got[i].Ordinal)
		assert.Equal(t, content, got[i].Content)
	}
}

func TestExportRejectsSingleEncodedRecordAboveReadableLimit(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	content := strings.Repeat("x", 64<<20)
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID:     "sess-1",
		Ordinal:       0,
		Role:          "user",
		Content:       content,
		ContentLength: len(content),
	}}))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encoded message record")
	assert.Contains(t, err.Error(), "67108864-byte readable limit")
	assertNoPublishedArtifacts(t, store, origin)
}

func TestExportPreservesSmallSingleSegmentHash(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	msgs, err := database.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	segmentData, err := encodeSegment(canonicalMessages(msgs))
	require.NoError(t, err)
	wantHash := hashHex(segmentData)

	_, err = ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	m := latestTestStoreManifest(t, store, origin)
	assert.Equal(t, []string{wantHash}, m.Segments)
}

type repeatedByteReader byte

func (r repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func syntheticSegmentHashes(count int) []string {
	hashes := make([]string, count)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("%064x", i+1)
	}
	return hashes
}

func syntheticSegmentRecords(t *testing.T, count int) []byte {
	return syntheticSegmentRecordsFrom(t, 0, count)
}

func syntheticSegmentRecordsFrom(t *testing.T, start, count int) []byte {
	t.Helper()
	var data bytes.Buffer
	for i := range count {
		record, err := canonicalJSON(segmentMessage{
			Version: formatVersion,
			Ordinal: start + i,
			Role:    "user",
		})
		require.NoError(t, err)
		_, err = data.Write(record)
		require.NoError(t, err)
	}
	return data.Bytes()
}
