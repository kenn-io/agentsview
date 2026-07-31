package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const testFolderMarkerBody = `{"format":"agentsview-normalized-artifacts","version":2}
`
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
			assert.Equal(t, testFolderMarkerBody, string(body))
		})
	}
}

func TestOpenFolderTransportReopensMarkedTarget(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(target, ".agentsview-artifacts.json"),
		[]byte(testFolderMarkerBody),
		0o600,
	))

	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	require.NoError(t, transport.Prepare(t.Context(), nil))
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
		{name: "future version", body: `{"format":"agentsview-normalized-artifacts","version":3}`},
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
	require.NoError(t, os.WriteFile(
		filepath.Join(target, ".agentsview-artifacts.json"),
		[]byte(testFolderMarkerBody),
		0o600,
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

	writeFolderWire(t, target, checkpoint, checkpointBody)
	writeFolderWire(t, target, manifest, manifestBody)
	writeFolderWire(t, target, segment, segmentBody)

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
	require.Len(t, store.changed, 4)
	assert.Equal(t, checkpoint, store.changed[3].Ref,
		"an existing checkpoint must close the create-before-signal crash window")
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
	assert.Equal(t, ExchangeResult{Received: 1}, result)
	assert.NoFileExists(t, corruptPath)
	quarantined, err := filepath.Glob(corruptPath + folderCorruptSeparator + "*")
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
	assertArtifactBody(t, store, validRef, validBody)
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

	_, err = transport.Exchange(
		t.Context(),
		newTestArtifactStore(t),
		testFolderPublishOrigin,
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, "not a regular file")
}

func TestFolderTransportPullDirectoryPagesStayBounded(t *testing.T) {
	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	var pageSizes []int
	transport.observeDirectoryPage = func(size int) {
		pageSizes = append(pageSizes, size)
	}
	const objectCount = transportDirectoryPageSize + 1
	origin := "peer-a1b2c3"
	for index := range objectCount {
		body := fmt.Appendf(nil, "segment-%04d", index)
		ref := testContentRef(t, origin, KindSegments, body, ".ndjson")
		writeFolderWire(t, target, ref, body)
	}

	store := &transportRecordingStore{ArtifactStore: newTestArtifactStore(t)}
	result, err := transport.Exchange(
		t.Context(),
		store,
		testFolderPublishOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, objectCount, result.Received)
	require.NotEmpty(t, pageSizes)
	assert.LessOrEqual(t, maxInt(pageSizes), transportDirectoryPageSize)
	assert.GreaterOrEqual(t, len(pageSizes), 3,
		"root plus the two object-directory pages must be observed")
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

func TestFolderTransportPushStorePagesStayBounded(t *testing.T) {
	target := t.TempDir()
	opened, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	transport := opened.(*folderTransport)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	var pageSizes []int
	transport.observeStorePage = func(size int) {
		pageSizes = append(pageSizes, size)
	}
	const objectCount = transportStorePageSize + 1
	origin := "local-a1b2c3"
	store := newTestArtifactStore(t)
	for index := range objectCount {
		body := fmt.Appendf(nil, "local-segment-%04d", index)
		ref := testContentRef(t, origin, KindSegments, body, ".ndjson")
		createTestStoreArtifact(t, store, ref, body)
	}

	result, err := transport.Exchange(t.Context(), store, origin)
	require.NoError(t, err)
	assert.Equal(t, objectCount, result.Published)
	require.NotEmpty(t, pageSizes)
	assert.LessOrEqual(t, maxInt(pageSizes), transportStorePageSize)
	assert.GreaterOrEqual(t, len(pageSizes), 2,
		"the two object-store pages must be observed")
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
