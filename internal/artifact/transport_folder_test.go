package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testFolderPublishOrigin = "local-a1b2c3"

func TestOpenFolderTransportInitializesMissingAndEmptyTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		create bool
	}{
		{name: "missing target"},
		{name: "empty target", create: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := filepath.Join(t.TempDir(), "share")
			if tt.create {
				require.NoError(t, os.Mkdir(target, 0o755))
			}

			transport, err := OpenFolderTransport(target, FolderTransportOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, transport.Close()) })

			body, err := os.ReadFile(filepath.Join(target, ".agentsview-artifacts.json"))
			require.NoError(t, err)
			var marker folderMarker
			require.NoError(t, json.Unmarshal(body, &marker))
			assert.Equal(t, folderFormatName, marker.Format)
			assert.Equal(t, folderFormatVersion, marker.Version)
			assert.Len(t, marker.NamespaceID, 32)
		})
	}
}

func TestOpenFolderTransportReopensMarkedTarget(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	initialized, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	require.NoError(t, initialized.Close())

	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	require.NoError(t, transport.Prepare(t.Context(), nil))
}

func TestOpenFolderTransportRecoversInterruptedMarkerTemporary(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	temporary := filepath.Join(
		target,
		folderMarkerTempPrefix+"0123456789abcdef",
	)
	require.NoError(t, os.WriteFile(temporary, []byte(`{"format":`), 0o600))

	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	assert.NoFileExists(t, temporary)
	marker, err := os.ReadFile(filepath.Join(target, folderMarkerName))
	require.NoError(t, err)
	assert.Contains(t, string(marker), folderFormatName)
}

func TestOpenFolderTransportRecoversPartialFinalWithCompleteTemporary(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	temporary := filepath.Join(
		target,
		folderMarkerTempPrefix+"0123456789abcdef",
	)
	body, err := json.Marshal(folderMarker{
		Format:      folderFormatName,
		NamespaceID: "0123456789abcdef0123456789abcdef",
		Version:     folderFormatVersion,
	})
	require.NoError(t, err)
	body = append(body, '\n')
	require.NoError(t, os.WriteFile(temporary, body, 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(target, folderMarkerName),
		[]byte(`{"format":`),
		0o600,
	))

	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	assert.NoFileExists(t, temporary)
	root, err := os.OpenRoot(target)
	require.NoError(t, err)
	_, err = readFolderMarker(root)
	require.NoError(t, err)
	require.NoError(t, root.Close())
}

func TestOpenFolderTransportRejectsMarkerTempLookalike(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	lookalike := filepath.Join(target, folderMarkerTempPrefix+"not-generated")
	require.NoError(t, os.WriteFile(lookalike, []byte("keep"), 0o600))

	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.Error(t, err)
	assert.Nil(t, transport)
	assert.ErrorContains(t, err, "not an agentsview artifact target")
	assert.FileExists(t, lookalike)
	assert.NoFileExists(t, filepath.Join(target, folderMarkerName))
}

func TestCreateFolderMarkerExclusiveRemovesPartialFinal(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	root, err := os.OpenRoot(target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	body := []byte(`{"format":"agentsview-normalized-artifacts"}`)
	partialWrite := errors.New("partial marker write")

	err = createFolderMarkerExclusiveWithWriter(
		root,
		body,
		func(file *os.File, body []byte) (int, error) {
			written, writeErr := file.Write(body[:8])
			require.NoError(t, writeErr)
			return written, partialWrite
		},
	)

	require.ErrorIs(t, err, partialWrite)
	assert.NoFileExists(t, filepath.Join(target, folderMarkerName))
}

func TestOpenFolderTransportRefusesUnmarkedNonemptyTarget(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	unrelated := filepath.Join(target, "keep.txt")
	require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0o600))

	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.Error(t, err)
	assert.Nil(t, transport)
	assert.ErrorContains(t, err, "not an agentsview artifact target")
	assert.FileExists(t, unrelated)
	assert.NoFileExists(t, filepath.Join(target, ".agentsview-artifacts.json"))
}

func TestOpenFolderTransportRejectsInvalidMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "older version", body: `{"format":"agentsview-normalized-artifacts","version":1}`},
		{name: "future version", body: `{"format":"agentsview-normalized-artifacts","version":4}`},
		{name: "wrong format", body: `{"format":"other","version":1}`},
		{name: "unknown field", body: `{"format":"agentsview-normalized-artifacts","version":1,"extra":true}`},
		{name: "trailing value", body: `{"format":"agentsview-normalized-artifacts","version":1}{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := t.TempDir()
			require.NoError(t, os.WriteFile(
				filepath.Join(target, ".agentsview-artifacts.json"),
				[]byte(tt.body),
				0o600,
			))

			transport, err := OpenFolderTransport(target, FolderTransportOptions{})
			require.Error(t, err)
			assert.Nil(t, transport)
			assert.ErrorContains(t, err, "invalid agentsview artifact target marker")
		})
	}
}

func TestOpenFolderTransportRejectsProtectedRootOverlap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		target    func(string) string
		protected func(string) string
	}{
		{
			name:      "same path",
			target:    func(root string) string { return filepath.Join(root, "shared") },
			protected: func(root string) string { return filepath.Join(root, "shared") },
		},
		{
			name:      "target below protected root",
			target:    func(root string) string { return filepath.Join(root, "provider", "share") },
			protected: func(root string) string { return filepath.Join(root, "provider") },
		},
		{
			name:      "protected root below target",
			target:    func(root string) string { return filepath.Join(root, "share") },
			protected: func(root string) string { return filepath.Join(root, "share", "provider") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			target := tt.target(root)
			protected := tt.protected(root)
			require.NoError(t, os.MkdirAll(target, 0o755))
			require.NoError(t, os.MkdirAll(protected, 0o755))

			transport, err := OpenFolderTransport(target, FolderTransportOptions{
				ForbiddenRoots: []string{protected},
			})
			require.Error(t, err)
			assert.Nil(t, transport)
			assert.ErrorContains(t, err, "overlaps a protected root")
			assert.NoFileExists(t, filepath.Join(target, ".agentsview-artifacts.json"))
		})
	}
}

func TestOpenFolderTransportRejectsMixedRelativeAndAbsoluteOverlap(
	t *testing.T,
) {
	t.Parallel()

	workingDirectory, err := os.Getwd()
	require.NoError(t, err)
	root, err := os.MkdirTemp(
		workingDirectory,
		".artifact-overlap-",
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	protected := filepath.Join(root, "provider")
	target := filepath.Join(protected, "share")
	require.NoError(t, os.MkdirAll(target, 0o755))
	relativeTarget, err := filepath.Rel(workingDirectory, target)
	require.NoError(t, err)
	relativeProtected, err := filepath.Rel(workingDirectory, protected)
	require.NoError(t, err)

	tests := []struct {
		name      string
		target    string
		protected string
	}{
		{
			name:      "relative target",
			target:    relativeTarget,
			protected: protected,
		},
		{
			name:      "relative protected root",
			target:    target,
			protected: relativeProtected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport, err := OpenFolderTransport(
				tt.target,
				FolderTransportOptions{
					ForbiddenRoots: []string{tt.protected},
				},
			)
			require.Error(t, err)
			assert.Nil(t, transport)
			assert.ErrorContains(t, err, "overlaps a protected root")
		})
	}
	assert.NoFileExists(t, filepath.Join(target, folderMarkerName))
}

func TestPathsOverlapRejectsCaseAliasesOnEveryPlatform(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	overlap, err := pathsOverlap(
		filepath.Join(root, "Provider", "artifacts"),
		filepath.Join(root, "provider"),
	)

	require.NoError(t, err)
	assert.True(t, overlap)
}

func TestOpenFolderTransportRejectsCaseAliasOverlap(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	protected := filepath.Join(root, "Protected")
	target := filepath.Join(protected, "share")
	require.NoError(t, os.MkdirAll(target, 0o755))
	alias := filepath.Join(root, strings.ToLower(filepath.Base(protected)))
	aliasInfo, err := os.Stat(alias)
	if err != nil {
		t.Skip("test filesystem is case-sensitive")
	}
	protectedInfo, err := os.Stat(protected)
	require.NoError(t, err)
	if !os.SameFile(aliasInfo, protectedInfo) {
		t.Skip("test filesystem does not resolve case aliases")
	}

	transport, err := OpenFolderTransport(target, FolderTransportOptions{
		ForbiddenRoots: []string{alias},
	})
	require.Error(t, err)
	assert.Nil(t, transport)
	assert.ErrorContains(t, err, "overlaps a protected root")
	assert.NoFileExists(t, filepath.Join(target, folderMarkerName))
}

func TestOpenFolderTransportResolvesFinalSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "actual")
	require.NoError(t, os.Mkdir(target, 0o755))
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(target, alias))

	transport, err := OpenFolderTransport(alias, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	assert.FileExists(t, filepath.Join(target, ".agentsview-artifacts.json"))
}

func TestFolderTransportPrepareRejectsSwappedTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "share")
	require.NoError(t, os.Mkdir(target, 0o755))
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	if runtime.GOOS == "windows" {
		t.Skip("Windows prevents replacing a directory held open by os.Root")
	}

	moved := filepath.Join(root, "moved")
	require.NoError(t, os.Rename(target, moved))
	require.NoError(t, os.Mkdir(target, 0o755))
	markerBody, err := os.ReadFile(filepath.Join(moved, folderMarkerName))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(target, folderMarkerName), markerBody, 0o600,
	))

	err = transport.Prepare(t.Context(), nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "changed while open")
}

func TestFolderTransportPullsDependenciesBeforeCheckpoint(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	origin := "peer-a1b2c3"
	segmentBody := []byte("{\"content\":\"hello\"}\n")
	manifestBody := []byte(`{"v":2}`)
	checkpointBody := []byte(`{"origin":"peer-a1b2c3","sessions":{},"v":1}`)
	segment := testContentRef(t, origin, KindSegments, segmentBody, ".ndjson")
	manifest := testContentRef(t, origin, KindManifests, manifestBody, ".json")
	checkpoint, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)

	writeFolderWire(t, target, segment, segmentBody)
	writeFolderWire(t, target, manifest, manifestBody)
	writeFolderWire(t, target, checkpoint, checkpointBody)

	store := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	result, err := transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Received: 3}, result)
	require.Len(t, store.changed, 3)
	assert.Equal(t, []Kind{
		KindSegments,
		KindManifests,
		KindCheckpoints,
	}, []Kind{
		store.changed[0].Ref.Kind,
		store.changed[1].Ref.Kind,
		store.changed[2].Ref.Kind,
	})

	assertArtifactBody(t, store, segment, segmentBody)
	assertArtifactBody(t, store, manifest, manifestBody)
	assertArtifactBody(t, store, checkpoint, checkpointBody)

	replay, err := transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{}, replay)
	require.Len(t, store.changed, 3)
}

func TestFolderTransportPullsOnlyNormalizedSessionKinds(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	origin := "peer-a1b2c3"
	segmentBody := []byte("{\"content\":\"normalized session\"}\n")
	segmentRef := testContentRef(
		t,
		origin,
		KindSegments,
		segmentBody,
		".ndjson",
	)
	rawBody := []byte("provider-owned source")
	rawRef := testContentRef(t, origin, KindRaw, rawBody, "")
	metadataBody := []byte(`{"title":"mutable user metadata"}`)
	metadataRef := testMetadataRef(t, origin, metadataBody)
	for ref, body := range map[Ref][]byte{
		segmentRef:  segmentBody,
		rawRef:      rawBody,
		metadataRef: metadataBody,
	} {
		writeFolderWire(t, target, ref, body)
	}

	store := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	result, err := transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)

	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Received: 1}, result)
	assertArtifactBody(t, store, segmentRef, segmentBody)
	for _, ref := range []Ref{rawRef, metadataRef} {
		_, err := store.Stat(t.Context(), ref)
		assert.ErrorIs(t, err, ErrArtifactNotFound)
	}
}

func TestFolderTransportPublishesOnlyNormalizedSessionKinds(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	origin := testFolderPublishOrigin
	segmentBody := []byte("{\"content\":\"normalized session\"}\n")
	segmentRef := testContentRef(
		t,
		origin,
		KindSegments,
		segmentBody,
		".ndjson",
	)
	rawBody := []byte("provider-owned source")
	rawRef := testContentRef(t, origin, KindRaw, rawBody, "")
	metadataBody := []byte(`{"title":"mutable user metadata"}`)
	metadataRef := testMetadataRef(t, origin, metadataBody)
	store := newTestArtifactStore(t)
	for ref, body := range map[Ref][]byte{
		segmentRef:  segmentBody,
		rawRef:      rawBody,
		metadataRef: metadataBody,
	} {
		createTestStoreArtifact(t, store, ref, body)
	}

	result, err := transport.Exchange(t.Context(), store, origin)

	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Published: 1}, result)
	assertFolderWireBody(t, target, segmentRef, segmentBody)
	for _, ref := range []Ref{rawRef, metadataRef} {
		wire, err := ToWireRef(ref)
		require.NoError(t, err)
		assert.NoFileExists(t, filepath.Join(
			target,
			wire.Origin,
			string(wire.Kind),
			wire.Name,
		))
	}
}

func TestFolderTransportQuarantinesCompleteCorruptWireAndContinues(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	origin := "peer-a1b2c3"
	corruptRef := testContentRef(
		t,
		origin,
		KindManifests,
		[]byte(`{"v":2}`),
		".json",
	)
	corruptWire, err := ToWireRef(corruptRef)
	require.NoError(t, err)
	wireDirectory := filepath.Join(target, origin, string(KindManifests))
	require.NoError(t, os.MkdirAll(wireDirectory, 0o755))
	corruptPath := filepath.Join(wireDirectory, corruptWire.Name)
	require.NoError(t, os.WriteFile(corruptPath, []byte("not zstd"), 0o600))
	appendFolderJournalTestEntry(t, target, Entry{
		Ref:      corruptRef,
		Identity: identityForBytes(t, []byte(`{"v":2}`)),
	})

	validBody := []byte("{\"content\":\"still imported\"}\n")
	validRef := testContentRef(t, origin, KindSegments, validBody, ".ndjson")
	writeFolderWire(t, target, validRef, validBody)

	store := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	result, err := transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Received: 1, Quarantined: 1}, result)
	assert.NoFileExists(t, corruptPath)
	quarantined, err := filepath.Glob(corruptPath + folderCorruptSeparator + "*")
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
	assertArtifactBody(t, store, validRef, validBody)

	require.NoError(t, transport.Close())
	reopened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	replayStore := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	replay, err := reopened.Exchange(
		t.Context(), replayStore, testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Received: 1}, replay)
	assertArtifactBody(t, replayStore, validRef, validBody)
}

func TestFolderTransportQuarantineLetsFreshConsumerAdvanceWhenArtifactExistsLocally(
	t *testing.T,
) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	origin := "peer-a1b2c3"
	localBody := []byte(`{"v":2}`)
	ref := testContentRef(t, origin, KindManifests, localBody, ".json")
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	directory := filepath.Join(target, origin, string(KindManifests))
	require.NoError(t, os.MkdirAll(directory, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(directory, wire.Name), []byte("not zstd"), 0o600,
	))
	appendFolderJournalTestEntry(t, target, Entry{
		Ref:      ref,
		Identity: identityForBytes(t, localBody),
	})
	laterBody := []byte("{\"content\":\"later event\"}\n")
	laterRef := testContentRef(t, origin, KindSegments, laterBody, ".ndjson")
	writeFolderWire(t, target, laterRef, laterBody)

	local := newTestArtifactStore(t)
	createTestStoreArtifact(t, local, ref, localBody)
	store := &transportRecordingStore{ArtifactStore: local}
	result, err := transport.Exchange(
		t.Context(), store, testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Received: 1, Quarantined: 1}, result)
	assertArtifactBody(t, store, ref, localBody)
	require.NoError(t, transport.Close())

	reopened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	replayStore := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	replay, err := reopened.Exchange(
		t.Context(), replayStore, testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Received: 1}, replay)
	assertArtifactBody(t, replayStore, laterRef, laterBody)
}

func TestFolderTransportWritesRejectionBeforeQuarantiningWire(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	origin := "peer-a1b2c3"
	expectedBody := []byte(`{"v":2}`)
	ref := testContentRef(t, origin, KindManifests, expectedBody, ".json")
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	directory := filepath.Join(target, origin, string(KindManifests))
	require.NoError(t, os.MkdirAll(directory, 0o755))
	wirePath := filepath.Join(directory, wire.Name)
	require.NoError(t, os.WriteFile(wirePath, []byte("not zstd"), 0o600))
	identity := identityForBytes(t, expectedBody)
	appendFolderJournalTestEntry(t, target, Entry{
		Ref:      ref,
		Identity: identity,
	})

	interrupted := errors.New("interrupt quarantine")
	transport.quarantineEntry = func(*os.Root, string) error {
		return interrupted
	}
	store := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	_, err = transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.Error(t, err)
	assert.ErrorIs(t, err, interrupted)
	assert.FileExists(t, wirePath)
	kindRoot, err := os.OpenRoot(directory)
	require.NoError(t, err)
	require.NoError(t, validateFolderJournalRejection(
		kindRoot,
		wire.Name,
		identity,
	))
	require.NoError(t, kindRoot.Close())

	transport.quarantineEntry = nil
	result, err := transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Quarantined: 1}, result)
	assert.NoFileExists(t, wirePath)
	quarantined, err := filepath.Glob(wirePath + folderCorruptSeparator + "*")
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
}

func TestFolderTransportRequiresChangeRecorderBeforeAcceptingArtifact(
	t *testing.T,
) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	body := []byte("unrecorded")
	ref := testContentRef(
		t,
		"peer-a1b2c3",
		KindSegments,
		body,
		".ndjson",
	)
	writeFolderWire(t, target, ref, body)
	store := newTestArtifactStore(t)

	_, err = transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, "change recorder is required")
	_, err = store.Stat(t.Context(), ref)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
}

func TestFolderTransportRejectsCheckpointIdentityConflict(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	ref, err := NewRef(
		"peer-a1b2c3",
		KindCheckpoints,
		"cp-0000000001.json",
	)
	require.NoError(t, err)
	localBody := []byte(`{"origin":"peer-a1b2c3","sessions":{},"v":1}`)
	remoteBody := []byte(`{"origin":"peer-a1b2c3","sessions":{"x":"y"},"v":1}`)
	store := newTestArtifactStore(t)
	_, err = store.Create(
		t.Context(),
		ref,
		identityForBytes(t, localBody),
		canonicalArtifactMediaType(ref.Kind),
		bytes.NewReader(localBody),
	)
	require.NoError(t, err)
	writeFolderWire(t, target, ref, remoteBody)

	_, err = transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactConflict)
	assertArtifactBody(t, store, ref, localBody)
}

func TestFolderTransportValidatesJournalIdentityBeforeCheckpointPersistence(
	t *testing.T,
) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	ref, err := NewRef(
		"peer-a1b2c3",
		KindCheckpoints,
		"cp-0000000001.json",
	)
	require.NoError(t, err)
	expectedBody := []byte(`{"origin":"peer-a1b2c3","sessions":{},"v":1}`)
	unexpectedBody := []byte(
		`{"origin":"peer-a1b2c3","sessions":{"peer-a1b2c3~other":"` +
			strings.Repeat("a", 64) + `"},"v":1}`,
	)
	writeFolderWireFile(t, target, ref, unexpectedBody)
	appendFolderJournalTestEntry(t, target, Entry{
		Ref:      ref,
		Identity: identityForBytes(t, expectedBody),
	})
	store := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}

	_, err = transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactConflict)
	_, statErr := store.Stat(t.Context(), ref)
	assert.ErrorIs(t, statErr, ErrArtifactNotFound)
	assert.Empty(t, store.changed)

	writeFolderWireFile(t, target, ref, expectedBody)
	result, err := transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Received: 1}, result)
	assertArtifactBody(t, store, ref, expectedBody)
}

func TestFolderTransportRejectsSymlinkedWireEntry(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	body := []byte("segment body")
	ref := testContentRef(
		t,
		"peer-a1b2c3",
		KindSegments,
		body,
		".ndjson",
	)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	directory := filepath.Join(target, wire.Origin, string(wire.Kind))
	require.NoError(t, os.MkdirAll(directory, 0o755))
	sourceDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(sourceDirectory, "source"),
		body,
		0o600,
	))
	require.NoError(t, os.Symlink(
		filepath.Join(sourceDirectory, "source"),
		filepath.Join(directory, wire.Name),
	))
	appendFolderJournalTestEntry(t, target, Entry{
		Ref:      ref,
		Identity: identityForBytes(t, body),
	})

	_, err = transport.Exchange(
		t.Context(),
		newTestArtifactStore(t),
		testFolderPublishOrigin,
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, "not a regular file")
}

func TestFolderTransportPullExchangeWorkStaysBounded(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	const objectCount = folderExchangeMaxObjects + 1
	origin := "peer-a1b2c3"
	for index := range objectCount {
		body := fmt.Appendf(nil, "segment-%04d", index)
		ref := testContentRef(t, origin, KindSegments, body, ".ndjson")
		writeFolderWire(t, target, ref, body)
	}

	store := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	var received int
	var rounds int
	for {
		result, exchangeErr := transport.Exchange(
			t.Context(), store, testFolderPublishOrigin,
		)
		require.NoError(t, exchangeErr)
		assert.LessOrEqual(t, result.Received, folderExchangeMaxObjects)
		received += result.Received
		rounds++
		if !result.More {
			break
		}
	}
	assert.Equal(t, objectCount, received)
	assert.Greater(t, rounds, 1)
}

func TestFolderTransportPullResumesWithinObjectBudget(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	state := &testFolderTransportStateStore{}
	transport, err := OpenFolderTransport(target, FolderTransportOptions{
		MaxObjects: 2,
		MaxBytes:   1 << 20,
		StateStore: state,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	origin := "peer-a1b2c3"
	refs := make([]Ref, 0, 5)
	for index := range 5 {
		body := fmt.Appendf(nil, "peer-segment-%04d", index)
		ref := testContentRef(t, origin, KindSegments, body, ".ndjson")
		writeFolderWire(t, target, ref, body)
		refs = append(refs, ref)
	}

	store := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	var received int
	var rounds int
	for {
		result, exchangeErr := transport.Exchange(
			t.Context(), store, testFolderPublishOrigin,
		)
		require.NoError(t, exchangeErr)
		assert.LessOrEqual(t, result.Received, 2)
		received += result.Received
		rounds++
		if !result.More {
			break
		}
		require.Less(t, rounds, 10)
	}

	assert.Equal(t, 5, received)
	assert.Equal(t, 3, rounds)
	for _, ref := range refs {
		_, statErr := store.Stat(t.Context(), ref)
		assert.NoError(t, statErr)
	}
}

type unknownTypeFolderDirEntry struct {
	name string
}

func (e unknownTypeFolderDirEntry) Name() string               { return e.name }
func (e unknownTypeFolderDirEntry) IsDir() bool                { return false }
func (e unknownTypeFolderDirEntry) Type() fs.FileMode          { return 0 }
func (e unknownTypeFolderDirEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrInvalid }

func TestFolderTransportUsesLstatForUnknownOriginEntryType(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	body := []byte("network-filesystem-entry")
	ref := testContentRef(
		t,
		"peer-a1b2c3",
		KindSegments,
		body,
		".ndjson",
	)
	writeFolderWire(t, target, ref, body)
	store := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	var result ExchangeResult

	err = transport.pullRootEntryLocked(
		t.Context(),
		store,
		unknownTypeFolderDirEntry{name: ref.Origin},
		&result,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Received: 1}, result)
	assertArtifactBody(t, store, ref, body)
}

func TestFolderTransportPushesWireObjectsBeforeCheckpointAndRetriesUnchanged(
	t *testing.T,
) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	store := newTestArtifactStore(t)
	origin := "local-a1b2c3"
	segmentBody := []byte("{\"content\":\"hello\"}\n")
	manifestBody := []byte(`{"v":2}`)
	checkpointBody := []byte(`{"origin":"local-a1b2c3","sessions":{},"v":1}`)
	segment := testContentRef(t, origin, KindSegments, segmentBody, ".ndjson")
	manifest := testContentRef(t, origin, KindManifests, manifestBody, ".json")
	checkpoint, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	createTestStoreArtifact(t, store, checkpoint, checkpointBody)
	createTestStoreArtifact(t, store, manifest, manifestBody)
	createTestStoreArtifact(t, store, segment, segmentBody)

	result, err := transport.Exchange(t.Context(), store, origin)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Published: 3}, result)
	assertFolderWireBody(t, target, segment, segmentBody)
	assertFolderWireBody(t, target, manifest, manifestBody)
	assertFolderWireBody(t, target, checkpoint, checkpointBody)

	manifestWire, err := ToWireRef(manifest)
	require.NoError(t, err)
	manifestPath := filepath.Join(
		target,
		manifestWire.Origin,
		string(manifestWire.Kind),
		manifestWire.Name,
	)
	before, err := os.Stat(manifestPath)
	require.NoError(t, err)

	replay, err := transport.Exchange(t.Context(), store, origin)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{}, replay)
	after, err := os.Stat(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime())

	temps, err := filepath.Glob(
		filepath.Join(target, "*", "*", ".agentsview-artifact-publish-*"),
	)
	require.NoError(t, err)
	assert.Empty(t, temps)
}

func TestFolderTransportDirectorySyncFailureStopsPublicationAuthority(
	t *testing.T,
) {
	t.Parallel()

	body := []byte("directory-sync-segment")
	ref := testContentRef(
		t,
		testFolderPublishOrigin,
		KindSegments,
		body,
		".ndjson",
	)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)

	tests := []struct {
		name      string
		failCall  int
		wantHead  bool
		lostEntry func(string) string
	}{
		{
			name:     "object entry",
			failCall: 3,
			lostEntry: func(target string) string {
				return filepath.Join(
					target,
					testFolderPublishOrigin,
					string(KindSegments),
					wire.Name,
				)
			},
		},
		{
			name:     "journal event",
			failCall: 5,
			lostEntry: func(target string) string {
				return filepath.Join(
					target,
					folderJournalDirectory,
					folderJournalEventName(1),
				)
			},
		},
		{
			name:     "journal head",
			failCall: 6,
			wantHead: true,
			lostEntry: func(target string) string {
				return filepath.Join(
					target,
					folderJournalDirectory,
					folderJournalHeadName,
				)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := t.TempDir()
			opened, openErr := OpenFolderTransport(
				target,
				FolderTransportOptions{},
			)
			require.NoError(t, openErr)
			transport := opened.(*folderTransport)
			t.Cleanup(func() { require.NoError(t, transport.Close()) })
			require.NoError(t, os.MkdirAll(
				filepath.Join(
					target,
					testFolderPublishOrigin,
					string(KindSegments),
				),
				0o755,
			))
			require.NoError(t, os.MkdirAll(
				filepath.Join(target, folderJournalDirectory),
				0o755,
			))

			store := newTestArtifactStore(t)
			createTestStoreArtifact(t, store, ref, body)
			interrupted := errors.New("directory sync interrupted")
			calls := 0
			transport.syncDirectory = func(root *os.Root) error {
				calls++
				if calls == tt.failCall {
					return interrupted
				}
				return syncFolderDirectory(root)
			}

			_, exchangeErr := transport.Exchange(
				t.Context(),
				store,
				testFolderPublishOrigin,
			)
			require.Error(t, exchangeErr)
			assert.ErrorIs(t, exchangeErr, interrupted)
			assert.Equal(t, tt.failCall, calls)
			headPath := filepath.Join(
				target,
				folderJournalDirectory,
				folderJournalHeadName,
			)
			if tt.wantHead {
				assert.FileExists(t, headPath)
			} else {
				assert.NoFileExists(t, headPath)
			}

			require.NoError(t, os.Remove(tt.lostEntry(target)))
			transport.syncDirectory = nil
			_, exchangeErr = transport.Exchange(
				t.Context(),
				store,
				testFolderPublishOrigin,
			)
			require.NoError(t, exchangeErr)
			assertFolderWireBody(t, target, ref, body)
			journal, journalErr := os.OpenRoot(
				filepath.Join(target, folderJournalDirectory),
			)
			require.NoError(t, journalErr)
			head, headErr := readFolderJournalHead(journal)
			require.NoError(t, headErr)
			require.NoError(t, journal.Close())
			assert.Equal(t, int64(1), head.Sequence)
		})
	}
}

func TestFolderTransportRetrySyncsVisibleObjectBeforeJournal(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	body := []byte("visible-but-not-yet-durable")
	ref := testContentRef(
		t,
		testFolderPublishOrigin,
		KindSegments,
		body,
		".ndjson",
	)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	store := newTestArtifactStore(t)
	createTestStoreArtifact(t, store, ref, body)
	objectSyncInterrupted := errors.New("object directory sync interrupted")
	failedObjectSync := false
	transport.syncDirectory = func(root *os.Root) error {
		if !failedObjectSync {
			if _, statErr := root.Lstat(wire.Name); statErr == nil {
				failedObjectSync = true
				return objectSyncInterrupted
			}
		}
		return syncFolderDirectory(root)
	}

	_, err = transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.ErrorIs(t, err, objectSyncInterrupted)
	assert.True(t, failedObjectSync)
	assertFolderWireBody(t, target, ref, body)
	assert.NoFileExists(t, filepath.Join(
		target,
		folderJournalDirectory,
		folderJournalHeadName,
	))

	retrySyncInterrupted := errors.New("retry object sync interrupted")
	transport.syncDirectory = func(root *os.Root) error {
		if _, statErr := root.Lstat(wire.Name); statErr == nil {
			return retrySyncInterrupted
		}
		return syncFolderDirectory(root)
	}
	_, err = transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.ErrorIs(t, err, retrySyncInterrupted)
	assert.NoFileExists(t, filepath.Join(
		target,
		folderJournalDirectory,
		folderJournalHeadName,
	))

	transport.syncDirectory = nil
	_, err = transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.NoError(t, err)
	journal, err := os.OpenRoot(filepath.Join(target, folderJournalDirectory))
	require.NoError(t, err)
	head, err := readFolderJournalHead(journal)
	require.NoError(t, err)
	require.NoError(t, journal.Close())
	assert.Equal(t, int64(1), head.Sequence)
}

func TestFolderTransportDirectorySyncFailureStopsSubdirectoryUse(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	body := []byte("subdirectory-sync-segment")
	ref := testContentRef(
		t,
		testFolderPublishOrigin,
		KindSegments,
		body,
		".ndjson",
	)
	store := newTestArtifactStore(t)
	createTestStoreArtifact(t, store, ref, body)
	interrupted := errors.New("subdirectory sync interrupted")
	transport.syncDirectory = func(*os.Root) error {
		return interrupted
	}

	_, err = transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.Error(t, err)
	assert.ErrorIs(t, err, interrupted)
	assert.NoDirExists(t, filepath.Join(
		target,
		testFolderPublishOrigin,
		string(KindSegments),
	))
	assert.NoFileExists(t, filepath.Join(
		target,
		folderJournalDirectory,
		folderJournalHeadName,
	))

	_, err = transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.Error(t, err)
	assert.ErrorIs(t, err, interrupted)
	assert.NoDirExists(t, filepath.Join(
		target,
		testFolderPublishOrigin,
		string(KindSegments),
	))

	transport.syncDirectory = nil
	_, err = transport.Exchange(t.Context(), store, testFolderPublishOrigin)
	require.NoError(t, err)
	assertFolderWireBody(t, target, ref, body)
}

func TestFolderTransportPushFallsBackWithoutHardLinks(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	transport.publishLink = func(*os.Root, string, string) error {
		return errors.New("hard links unsupported")
	}

	body := []byte("fallback body")
	ref := testContentRef(
		t,
		testFolderPublishOrigin,
		KindSegments,
		body,
		".ndjson",
	)
	store := newTestArtifactStore(t)
	createTestStoreArtifact(t, store, ref, body)

	result, err := transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Published: 1}, result)
	assertFolderWireBody(t, target, ref, body)

	replay, err := transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{}, replay)
}

func TestFolderTransportRemovesAbandonedPublishTemp(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	directory := filepath.Join(
		target,
		testFolderPublishOrigin,
		string(KindSegments),
	)
	require.NoError(t, os.MkdirAll(directory, 0o755))
	abandoned := filepath.Join(
		directory,
		".agentsview-artifact-publish-crash",
	)
	require.NoError(t, os.WriteFile(abandoned, []byte("partial"), 0o600))

	body := []byte("complete")
	ref := testContentRef(
		t,
		testFolderPublishOrigin,
		KindSegments,
		body,
		".ndjson",
	)
	store := newTestArtifactStore(t)
	createTestStoreArtifact(t, store, ref, body)

	result, err := transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{Published: 1}, result)
	assert.NoFileExists(t, abandoned)
	assertFolderWireBody(t, target, ref, body)
}

func TestFolderTransportExchangeLockProtectsActivePublishTemp(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	directory := filepath.Join(
		target,
		testFolderPublishOrigin,
		string(KindSegments),
	)
	require.NoError(t, os.MkdirAll(directory, 0o755))
	active := filepath.Join(directory, folderPublishTempPrefix+"active")
	require.NoError(t, os.WriteFile(active, []byte("in progress"), 0o600))
	held := flock.New(filepath.Join(target, folderExchangeLockName))
	locked, err := held.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	t.Cleanup(func() { require.NoError(t, held.Unlock()) })

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = transport.Exchange(
		ctx,
		&transportRecordingStore{ArtifactStore: newTestArtifactStore(t)},
		testFolderPublishOrigin,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.FileExists(t, active)
}

func TestFolderTransportExchangeLockRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	escaped := filepath.Join(t.TempDir(), "escaped.lock")
	lockPath := filepath.Join(target, folderExchangeLockName)
	if err := os.Symlink(escaped, lockPath); err != nil {
		t.Skipf("creating lock symlink: %v", err)
	}

	_, err = transport.Exchange(
		t.Context(),
		&transportRecordingStore{ArtifactStore: newTestArtifactStore(t)},
		testFolderPublishOrigin,
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, "exchange lock is not a regular file")
	assert.NoFileExists(t, escaped)
}

func TestFolderTransportRejectsOversizedStoreEntryBeforePublication(
	t *testing.T,
) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	body := bytes.Repeat([]byte("x"), int(manifestDecodedLimit+1))
	ref := testContentRef(t, "local-a1b2c3", KindManifests, body, ".json")
	store := newTestArtifactStore(t)
	createTestStoreArtifact(t, store, ref, body)

	_, err = transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.ErrorContains(t, err, "decoded size limit")
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(
		target,
		wire.Origin,
		string(wire.Kind),
		wire.Name,
	))
}

func TestFolderTransportQuarantinePreservesDifferentReplacementIdentity(
	t *testing.T,
) {
	t.Parallel()

	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	ref, err := NewRef(
		"peer-a1b2c3",
		KindCheckpoints,
		"cp-0000000001.json",
	)
	require.NoError(t, err)
	invalidBody := []byte(`{"v":1}`)
	replacementBody := []byte(
		`{"origin":"peer-a1b2c3","seq":1,"sessions":{},"v":1}`,
	)
	writeFolderWire(t, target, ref, replacementBody)

	err = transport.QuarantineTransportArtifact(
		t.Context(),
		ref,
		identityForBytes(t, invalidBody),
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactConflict)
	assertFolderWireBody(t, target, ref, replacementBody)
}

func TestFolderTransportPushExchangeWorkStaysBounded(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	var pageSizes []int
	transport.observeStorePage = func(size int) {
		pageSizes = append(pageSizes, size)
	}
	const objectCount = folderExchangeMaxObjects + 1
	origin := "local-a1b2c3"
	store := newTestArtifactStore(t)
	for index := range objectCount {
		body := fmt.Appendf(nil, "local-segment-%04d", index)
		ref := testContentRef(t, origin, KindSegments, body, ".ndjson")
		createTestStoreArtifact(t, store, ref, body)
	}

	var published int
	var rounds int
	for {
		result, exchangeErr := transport.Exchange(t.Context(), store, origin)
		require.NoError(t, exchangeErr)
		assert.LessOrEqual(t, result.Published, folderExchangeMaxObjects)
		published += result.Published
		rounds++
		if !result.More {
			break
		}
	}
	assert.Equal(t, objectCount, published)
	assert.Greater(t, rounds, 1)
	require.NotEmpty(t, pageSizes)
	assert.LessOrEqual(t, maxInt(pageSizes), transportStorePageSize)
	assert.GreaterOrEqual(t, len(pageSizes), 2,
		"the two object-store pages must be observed")
}

func TestFolderTransportPushSharesLimitsAcrossKinds(t *testing.T) {
	t.Parallel()

	segmentBody := []byte("segment-fills-the-exchange-budget")
	tests := []struct {
		name       string
		maxObjects int
		maxBytes   int64
	}{
		{name: "byte budget", maxObjects: 8, maxBytes: int64(len(segmentBody))},
		{name: "object budget", maxObjects: 1, maxBytes: 1 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := t.TempDir()
			origin := "local-a1b2c3"
			transport, err := OpenFolderTransport(target, FolderTransportOptions{
				MaxObjects: tt.maxObjects,
				MaxBytes:   tt.maxBytes,
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, transport.Close()) })

			store := newTestArtifactStore(t)
			segmentRef := testContentRef(t, origin, KindSegments, segmentBody, ".ndjson")
			createTestStoreArtifact(t, store, segmentRef, segmentBody)
			manifestBody, err := canonicalJSON(manifest{
				Version:  manifestFormatVersion,
				Origin:   origin,
				Segments: []string{},
			})
			require.NoError(t, err)
			manifestRef := testContentRef(t, origin, KindManifests, manifestBody, ".json")
			createTestStoreArtifact(t, store, manifestRef, manifestBody)

			first, err := transport.Exchange(t.Context(), store, origin)
			require.NoError(t, err)
			assert.Equal(t, 1, first.Published)
			assert.True(t, first.More)
			assertFolderWireBody(t, target, segmentRef, segmentBody)
			_, err = os.Stat(filepath.Join(
				target, origin, string(KindManifests), manifestRef.Name,
			))
			assert.ErrorIs(t, err, fs.ErrNotExist)

			second, err := transport.Exchange(t.Context(), store, origin)
			require.NoError(t, err)
			assert.Equal(t, 1, second.Published)
			assertFolderWireBody(t, target, manifestRef, manifestBody)
			if second.More {
				settled, settleErr := transport.Exchange(t.Context(), store, origin)
				require.NoError(t, settleErr)
				assert.Zero(t, settled.Published)
				assert.False(t, settled.More)
			}
		})
	}
}

func TestFolderTransportExchangeResumesWithinObjectBudget(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{
		MaxObjects: 2,
		MaxBytes:   1 << 20,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	origin := "local-a1b2c3"
	store := newTestArtifactStore(t)
	refs := make([]Ref, 0, 5)
	for index := range 5 {
		body := fmt.Appendf(nil, "bounded-segment-%04d", index)
		ref := testContentRef(t, origin, KindSegments, body, ".ndjson")
		createTestStoreArtifact(t, store, ref, body)
		refs = append(refs, ref)
	}

	var published int
	var rounds int
	for {
		result, exchangeErr := transport.Exchange(t.Context(), store, origin)
		require.NoError(t, exchangeErr)
		assert.LessOrEqual(t, result.Published, 2)
		published += result.Published
		rounds++
		if !result.More {
			break
		}
		require.Less(t, rounds, 10)
	}

	assert.Equal(t, 5, published)
	assert.Equal(t, 3, rounds)
	for _, ref := range refs {
		entry, reader, openErr := store.Open(t.Context(), ref)
		require.NoError(t, openErr)
		body, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Verify())
		require.NoError(t, reader.Close())
		assertFolderWireBody(t, target, entry.Ref, body)
	}
}

func TestFolderTransportExchangeCursorSurvivesReopen(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	state := &testFolderTransportStateStore{}
	store := newTestArtifactStore(t)
	origin := "local-a1b2c3"
	for index := range 5 {
		body := fmt.Appendf(nil, "durable-segment-%04d", index)
		ref := testContentRef(t, origin, KindSegments, body, ".ndjson")
		createTestStoreArtifact(t, store, ref, body)
	}
	options := FolderTransportOptions{
		MaxObjects: 2,
		MaxBytes:   1 << 20,
		StateStore: state,
	}

	first, err := OpenFolderTransport(target, options)
	require.NoError(t, err)
	firstResult, err := first.Exchange(t.Context(), store, origin)
	require.NoError(t, err)
	assert.Equal(t, 2, firstResult.Published)
	assert.True(t, firstResult.More)
	require.NoError(t, first.Close())

	second, err := OpenFolderTransport(target, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	secondResult, err := second.Exchange(t.Context(), store, origin)
	require.NoError(t, err)
	assert.Equal(t, 2, secondResult.Published)
	assert.True(t, secondResult.More)
}

func TestFolderTransportRecoversJournalEventBeforeHeadAdvance(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	origin := "local-a1b2c3"
	body := []byte("crash-window-segment")
	ref := testContentRef(t, origin, KindSegments, body, ".ndjson")
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	directory := filepath.Join(target, origin, string(KindSegments))
	require.NoError(t, os.MkdirAll(directory, 0o755))
	var encoded bytes.Buffer
	require.NoError(t, EncodeWire(t.Context(), ref, bytes.NewReader(body), &encoded))
	require.NoError(t, os.WriteFile(
		filepath.Join(directory, wire.Name), encoded.Bytes(), 0o600,
	))
	journalDirectory := filepath.Join(target, folderJournalDirectory)
	require.NoError(t, os.MkdirAll(journalDirectory, 0o755))
	eventBody, err := json.Marshal(folderJournalEvent{
		Kind:     KindSegments,
		Name:     wire.Name,
		Origin:   origin,
		Sequence: 1,
		SHA256:   identityForBytes(t, body).SHA256,
		Size:     int64(len(body)),
	})
	require.NoError(t, err)
	eventBody = append(eventBody, '\n')
	require.NoError(t, os.WriteFile(
		filepath.Join(journalDirectory, folderJournalEventName(1)),
		eventBody,
		0o600,
	))
	store := newTestArtifactStore(t)
	createTestStoreArtifact(t, store, ref, body)

	result, err := transport.Exchange(t.Context(), store, origin)
	require.NoError(t, err)
	assert.Zero(t, result.Published)
	root, err := os.OpenRoot(journalDirectory)
	require.NoError(t, err)
	head, err := readFolderJournalHead(root)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	assert.Equal(t, int64(1), head.Sequence)
}

func TestFolderTransportRecoversTruncatedUncommittedJournalEvent(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	journalDirectory := filepath.Join(target, folderJournalDirectory)
	require.NoError(t, os.MkdirAll(journalDirectory, 0o755))
	eventPath := filepath.Join(journalDirectory, folderJournalEventName(1))
	require.NoError(t, os.WriteFile(eventPath, []byte(`{"kind":`), 0o600))

	body := []byte("recovered-segment")
	ref := testContentRef(
		t,
		testFolderPublishOrigin,
		KindSegments,
		body,
		".ndjson",
	)
	entry := Entry{Ref: ref, Identity: identityForBytes(t, body)}
	require.NoError(t, transport.appendFolderJournalLocked(t.Context(), entry))

	journal, err := os.OpenRoot(journalDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	event, err := readFolderJournalEvent(journal, 1)
	require.NoError(t, err)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	assert.Equal(t, folderJournalEvent{
		Kind:     KindSegments,
		Name:     wire.Name,
		Origin:   testFolderPublishOrigin,
		Sequence: 1,
		SHA256:   entry.Identity.SHA256,
		Size:     entry.Identity.Size,
	}, event)
	head, err := readFolderJournalHead(journal)
	require.NoError(t, err)
	assert.Equal(t, int64(1), head.Sequence)
	quarantined, err := filepath.Glob(eventPath + folderCorruptSeparator + "*")
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
	partial, err := os.ReadFile(quarantined[0])
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"kind":`), partial)
}

func TestFolderTransportRecoversTruncatedJournalRejection(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	body := []byte("rejected-segment")
	ref := testContentRef(
		t,
		"peer-a1b2c3",
		KindSegments,
		body,
		".ndjson",
	)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	identity := identityForBytes(t, body)
	appendFolderJournalTestEntry(t, target, Entry{
		Ref:      ref,
		Identity: identity,
	})
	directory := filepath.Join(target, ref.Origin, string(ref.Kind))
	require.NoError(t, os.MkdirAll(directory, 0o755))
	rejectionPath := filepath.Join(
		directory,
		folderJournalRejectionName(wire.Name),
	)
	require.NoError(t, os.WriteFile(
		rejectionPath,
		[]byte(`{"sha256":`),
		0o600,
	))

	result, err := transport.Exchange(
		t.Context(),
		&transportRecordingStore{ArtifactStore: newTestArtifactStore(t)},
		testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{}, result)
	kindRoot, err := os.OpenRoot(directory)
	require.NoError(t, err)
	require.NoError(t, validateFolderJournalRejection(
		kindRoot,
		wire.Name,
		identity,
	))
	require.NoError(t, kindRoot.Close())
	quarantined, err := filepath.Glob(rejectionPath + folderCorruptSeparator + "*")
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
	partial, err := os.ReadFile(quarantined[0])
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"sha256":`), partial)
}

type testFolderTransportStateStore struct {
	values map[string]string
}

func (s *testFolderTransportStateStore) LoadFolderTransportState(
	_ context.Context,
	namespaceID string,
) (string, error) {
	return s.values[namespaceID], nil
}

func (s *testFolderTransportStateStore) SaveFolderTransportState(
	_ context.Context,
	namespaceID string,
	value string,
) error {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[namespaceID] = value
	return nil
}

type transportRecordingStore struct {
	ArtifactStore
	changed []Entry
}

func (s *transportRecordingStore) RecordTransportChanged(
	_ context.Context,
	entry Entry,
) error {
	s.changed = append(s.changed, entry)
	return nil
}

func testContentRef(
	t *testing.T,
	origin string,
	kind Kind,
	body []byte,
	extension string,
) Ref {
	t.Helper()
	sum := sha256.Sum256(body)
	ref, err := NewRef(origin, kind, hex.EncodeToString(sum[:])+extension)
	require.NoError(t, err)
	return ref
}

func testMetadataRef(t *testing.T, origin string, body []byte) Ref {
	t.Helper()
	sum := sha256.Sum256(body)
	ref, err := NewRef(
		origin,
		KindMeta,
		"event-"+hex.EncodeToString(sum[:])+".json",
	)
	require.NoError(t, err)
	return ref
}

func writeFolderWire(t *testing.T, target string, ref Ref, body []byte) {
	t.Helper()
	writeFolderWireFile(t, target, ref, body)
	appendFolderJournalTestEntry(t, target, Entry{
		Ref:      ref,
		Identity: identityForBytes(t, body),
	})
}

func writeFolderWireFile(t *testing.T, target string, ref Ref, body []byte) {
	t.Helper()
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	directory := filepath.Join(target, wire.Origin, string(wire.Kind))
	require.NoError(t, os.MkdirAll(directory, 0o755))
	var encoded bytes.Buffer
	require.NoError(t, EncodeWire(
		t.Context(),
		ref,
		bytes.NewReader(body),
		&encoded,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(directory, wire.Name),
		encoded.Bytes(),
		0o600,
	))
}

func appendFolderJournalTestEntry(t *testing.T, target string, entry Entry) {
	t.Helper()
	root, err := os.OpenRoot(target)
	require.NoError(t, err)
	transport := &folderTransport{root: root}
	require.NoError(t, transport.appendFolderJournalLocked(t.Context(), entry))
	require.NoError(t, root.Close())
}

func createTestStoreArtifact(
	t *testing.T,
	store ArtifactStore,
	ref Ref,
	body []byte,
) {
	t.Helper()
	_, err := store.Create(
		t.Context(),
		ref,
		identityForBytes(t, body),
		canonicalArtifactMediaType(ref.Kind),
		bytes.NewReader(body),
	)
	require.NoError(t, err)
}

func assertFolderWireBody(
	t *testing.T,
	target string,
	ref Ref,
	want []byte,
) {
	t.Helper()
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	file, err := os.Open(filepath.Join(
		target,
		wire.Origin,
		string(wire.Kind),
		wire.Name,
	))
	require.NoError(t, err)
	defer file.Close()
	var decoded bytes.Buffer
	require.NoError(t, DecodeWire(
		t.Context(),
		wire,
		file,
		&decoded,
		transportWireLimits(ref.Kind),
	))
	assert.Equal(t, want, decoded.Bytes())
}

func assertArtifactBody(
	t *testing.T,
	store ArtifactStore,
	ref Ref,
	want []byte,
) {
	t.Helper()
	_, reader, err := store.Open(t.Context(), ref)
	require.NoError(t, err)
	body, readErr := io.ReadAll(reader)
	verifyErr := reader.Verify()
	closeErr := reader.Close()
	require.NoError(t, errors.Join(readErr, verifyErr, closeErr))
	assert.Equal(t, want, body)
}

func maxInt(values []int) int {
	maximum := 0
	for _, value := range values {
		maximum = max(maximum, value)
	}
	return maximum
}
