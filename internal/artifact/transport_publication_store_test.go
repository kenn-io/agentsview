package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type manifestOpenCountingStore struct {
	ArtifactStore
	manifestBytes int64
	manifestOpens int
}

func (s *manifestOpenCountingStore) Open(
	ctx context.Context,
	ref Ref,
) (Entry, VerifiedReader, error) {
	entry, reader, err := s.ArtifactStore.Open(ctx, ref)
	if err == nil && ref.Kind == KindManifests {
		s.manifestOpens++
		s.manifestBytes += entry.Identity.Size
	}
	return entry, reader, err
}

const emptyArtifactPublicationMapSHA256 = "ca3d163bab055381827226140568f3bef7eaac187cebd76878e0b63e9e442356"

type countingPublicationAuthority struct {
	head         db.ArtifactCheckpointHead
	headCalls    int
	pageCalls    int
	pageLimits   []int
	publications []db.ArtifactPublication
}

func (a *countingPublicationAuthority) GetArtifactCheckpointHead(
	context.Context,
	string,
) (db.ArtifactCheckpointHead, bool, error) {
	a.headCalls++
	return a.head, true, nil
}

func (a *countingPublicationAuthority) ArtifactPublicationPage(
	_ context.Context,
	_ string,
	afterSessionID string,
	limit int,
) ([]db.ArtifactPublication, int64, bool, error) {
	a.pageCalls++
	a.pageLimits = append(a.pageLimits, limit)
	start := 0
	for start < len(a.publications) &&
		a.publications[start].SessionID <= afterSessionID {
		start++
	}
	end := min(start+limit, len(a.publications))
	return a.publications[start:end],
		a.head.PublicationRevision,
		end < len(a.publications),
		nil
}

func TestAuthoritativePublicationStoreBoundsEmptySegmentTraversal(t *testing.T) {
	t.Parallel()

	origin := "local-a1b2c3"
	content := newTestArtifactStore(t)
	manifestBody, err := canonicalJSON(manifest{
		Version:  manifestFormatVersion,
		Origin:   origin,
		Segments: []string{},
	})
	require.NoError(t, err)
	manifestIdentity := identityForBytes(t, manifestBody)
	manifestRef, err := NewRef(
		origin,
		KindManifests,
		manifestIdentity.SHA256+".json",
	)
	require.NoError(t, err)
	createTestStoreArtifact(t, content, manifestRef, manifestBody)

	publications := make([]db.ArtifactPublication, transportStorePageSize+1)
	for index := range publications {
		publications[index] = db.ArtifactPublication{
			Origin:       origin,
			SessionID:    fmt.Sprintf("session-%04d", index),
			ManifestHash: manifestIdentity.SHA256,
		}
	}
	authority := &countingPublicationAuthority{
		head: db.ArtifactCheckpointHead{
			Origin:              origin,
			Sequence:            1,
			PublicationRevision: 7,
			SessionMapSHA256:    emptyArtifactPublicationMapSHA256,
			CheckpointSHA256:    strings64("a"),
			CheckpointSize:      10,
		},
		publications: publications,
	}
	store, err := newAuthoritativePublicationStore(
		t.Context(),
		authority,
		content,
		origin,
	)
	require.NoError(t, err)

	entries, cursor, more, err := store.folderTransportPage(
		t.Context(),
		folderPushCursor{Origin: origin},
		folderExchangeMaxObjects,
		folderExchangeMaxBytes,
	)

	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.True(t, more)
	assert.Equal(t, "session-0511", cursor.PublicationSessionID)
	assert.Equal(t, 1, authority.pageCalls,
		"one exchange may inspect only one bounded publication page per kind")

	entries, cursor, more, err = store.folderTransportPage(
		t.Context(),
		cursor,
		folderExchangeMaxObjects,
		folderExchangeMaxBytes,
	)
	require.NoError(t, err)
	assert.Len(t, entries, folderExchangeMaxObjects)
	assert.True(t, more)
	assert.Equal(t, 1, cursor.KindIndex)
	assert.Equal(t, "session-0127", cursor.PublicationSessionID)
	assert.Equal(t, 3, authority.pageCalls,
		"resume reads the remaining segment page and one manifest page")
}

func TestAuthoritativePublicationStoreChargesManifestInspection(t *testing.T) {
	t.Parallel()

	origin := "local-a1b2c3"
	content := &manifestOpenCountingStore{ArtifactStore: newTestArtifactStore(t)}
	manifestBody, err := canonicalJSON(manifest{
		Version:  manifestFormatVersion,
		Origin:   origin,
		Segments: []string{},
	})
	require.NoError(t, err)
	manifestIdentity := identityForBytes(t, manifestBody)
	manifestRef, err := NewRef(
		origin,
		KindManifests,
		manifestIdentity.SHA256+".json",
	)
	require.NoError(t, err)
	createTestStoreArtifact(t, content.ArtifactStore, manifestRef, manifestBody)

	publications := make([]db.ArtifactPublication, 3)
	for index := range publications {
		publications[index] = db.ArtifactPublication{
			Origin:       origin,
			SessionID:    fmt.Sprintf("session-%04d", index),
			ManifestHash: manifestIdentity.SHA256,
		}
	}
	authority := &countingPublicationAuthority{
		head: db.ArtifactCheckpointHead{
			Origin:              origin,
			Sequence:            1,
			PublicationRevision: 7,
			SessionMapSHA256:    emptyArtifactPublicationMapSHA256,
			CheckpointSHA256:    strings64("a"),
			CheckpointSize:      10,
		},
		publications: publications,
	}
	store, err := newAuthoritativePublicationStore(
		t.Context(), authority, content, origin,
	)
	require.NoError(t, err)

	entries, cursor, more, err := store.folderTransportPage(
		t.Context(),
		folderPushCursor{Origin: origin},
		folderExchangeMaxObjects,
		manifestIdentity.Size,
	)

	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.True(t, more)
	assert.Equal(t, "session-0000", cursor.PublicationSessionID)
	assert.Equal(t, 1, content.manifestOpens)
	assert.Equal(t, manifestIdentity.Size, content.manifestBytes)
}

func TestAuthoritativePublicationStoreReadsOnlyHeadDuringConstruction(
	t *testing.T,
) {
	t.Parallel()

	origin := "local-a1b2c3"
	authority := &countingPublicationAuthority{head: db.ArtifactCheckpointHead{
		Origin:              origin,
		Sequence:            1,
		PublicationRevision: 7,
		SessionMapSHA256:    emptyArtifactPublicationMapSHA256,
		CheckpointSHA256:    strings64("a"),
		CheckpointSize:      10,
	}}

	store, err := newAuthoritativePublicationStore(
		t.Context(),
		authority,
		newTestArtifactStore(t),
		origin,
	)

	require.NoError(t, err)
	require.NotNil(t, store)
	assert.Equal(t, 1, authority.headCalls)
	assert.Zero(t, authority.pageCalls,
		"an unchanged exchange compares the head before reading publications")
}

func TestAuthoritativePublicationStorePagesChangedPublications(t *testing.T) {
	t.Parallel()

	origin := "local-a1b2c3"
	checkpointBody := []byte("checkpoint")
	checkpointIdentity := identityForBytes(t, checkpointBody)
	authority := &countingPublicationAuthority{head: db.ArtifactCheckpointHead{
		Origin:              origin,
		Sequence:            1,
		PublicationRevision: 7,
		SessionMapSHA256:    emptyArtifactPublicationMapSHA256,
		CheckpointSHA256:    checkpointIdentity.SHA256,
		CheckpointSize:      checkpointIdentity.Size,
	}}
	content := newTestArtifactStore(t)
	checkpointRef, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	createTestStoreArtifact(t, content, checkpointRef, checkpointBody)
	store, err := newAuthoritativePublicationStore(
		t.Context(),
		authority,
		content,
		origin,
	)
	require.NoError(t, err)

	entries, _, more, err := store.folderTransportPage(
		t.Context(),
		folderPushCursor{Origin: origin},
		folderExchangeMaxObjects,
		folderExchangeMaxBytes,
	)

	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, entries, 1)
	assert.Equal(t, checkpointRef, entries[0].Ref)
	assert.Equal(t, 2, authority.pageCalls,
		"segments and manifests each use one bounded publication traversal")
	require.Len(t, authority.pageLimits, 2)
	assert.LessOrEqual(t, maxInt(authority.pageLimits), transportStorePageSize)
}

func TestFolderTransportNoOpSkipsAuthoritativePublicationPages(t *testing.T) {
	t.Parallel()

	origin := "local-a1b2c3"
	checkpointBody := []byte("checkpoint")
	checkpointIdentity := identityForBytes(t, checkpointBody)
	authority := &countingPublicationAuthority{head: db.ArtifactCheckpointHead{
		Origin:              origin,
		Sequence:            1,
		PublicationRevision: 7,
		SessionMapSHA256:    emptyArtifactPublicationMapSHA256,
		CheckpointSHA256:    checkpointIdentity.SHA256,
		CheckpointSize:      checkpointIdentity.Size,
	}}
	content := newTestArtifactStore(t)
	checkpointRef, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	createTestStoreArtifact(t, content, checkpointRef, checkpointBody)
	transport, err := OpenFolderTransport(t.TempDir(), FolderTransportOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	firstStore, err := newAuthoritativePublicationStore(
		t.Context(), authority, content, origin,
	)
	require.NoError(t, err)
	first, err := transport.Exchange(t.Context(), firstStore, origin)
	require.NoError(t, err)
	assert.Equal(t, 1, first.Published)
	assert.Equal(t, 2, authority.pageCalls)

	secondStore, err := newAuthoritativePublicationStore(
		t.Context(), authority, content, origin,
	)
	require.NoError(t, err)
	second, err := transport.Exchange(t.Context(), secondStore, origin)
	require.NoError(t, err)
	assert.Equal(t, ExchangeResult{}, second)
	assert.Equal(t, 2, authority.pageCalls,
		"an unchanged head must not inspect any publication page")
}

func TestFolderTransportResumesBoundedPublishedRepairAfterReopen(t *testing.T) {
	t.Parallel()

	origin := "local-a1b2c3"
	content := newTestArtifactStore(t)
	segmentBodies := [][]byte{
		[]byte("first segment\n"),
		[]byte("second segment\n"),
	}
	segmentHashes := make([]string, 0, len(segmentBodies))
	for _, body := range segmentBodies {
		ref := testContentRef(t, origin, KindSegments, body, ".ndjson")
		createTestStoreArtifact(t, content, ref, body)
		segmentHashes = append(segmentHashes, identityForBytes(t, body).SHA256)
	}
	manifestBody, err := canonicalJSON(manifest{
		Version:  manifestFormatVersion,
		Origin:   origin,
		Segments: segmentHashes,
	})
	require.NoError(t, err)
	manifestIdentity := identityForBytes(t, manifestBody)
	manifestRef, err := NewRef(
		origin,
		KindManifests,
		manifestIdentity.SHA256+".json",
	)
	require.NoError(t, err)
	createTestStoreArtifact(t, content, manifestRef, manifestBody)
	checkpointBody := []byte("checkpoint")
	checkpointIdentity := identityForBytes(t, checkpointBody)
	checkpointRef, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	createTestStoreArtifact(t, content, checkpointRef, checkpointBody)
	authority := &countingPublicationAuthority{
		head: db.ArtifactCheckpointHead{
			Origin:              origin,
			Sequence:            1,
			PublicationRevision: 7,
			SessionMapSHA256:    emptyArtifactPublicationMapSHA256,
			CheckpointSHA256:    checkpointIdentity.SHA256,
			CheckpointSize:      checkpointIdentity.Size,
		},
		publications: []db.ArtifactPublication{{
			Origin: origin, SessionID: "one",
			ManifestHash: manifestIdentity.SHA256,
		}},
	}
	publishedStore, err := newAuthoritativePublicationStore(
		t.Context(), authority, content, origin,
	)
	require.NoError(t, err)
	state := &testFolderTransportStateStore{}
	target := t.TempDir()
	initial, err := OpenFolderTransport(target, FolderTransportOptions{
		MaxObjects: 10,
		StateStore: state,
	})
	require.NoError(t, err)
	initialResult, err := initial.Exchange(t.Context(), publishedStore, origin)
	require.NoError(t, err)
	assert.Equal(t, 4, initialResult.Published)
	assert.False(t, initialResult.More)
	require.NoError(t, initial.Close())

	checkpointWire, err := ToWireRef(checkpointRef)
	require.NoError(t, err)
	checkpointPath := filepath.Join(
		target,
		checkpointWire.Origin,
		string(checkpointWire.Kind),
		checkpointWire.Name,
	)
	require.NoError(t, os.Remove(checkpointPath))
	journalSequence := readTestFolderJournalSequence(t, target)
	repair, err := OpenFolderTransport(target, FolderTransportOptions{
		MaxObjects:      1,
		StateStore:      state,
		RepairPublished: true,
	})
	require.NoError(t, err)
	firstRepair, err := repair.Exchange(t.Context(), publishedStore, origin)
	require.NoError(t, err)
	assert.Zero(t, firstRepair.Published)
	assert.True(t, firstRepair.More)
	require.NoError(t, repair.Close())

	published := 0
	more := true
	for attempts := 0; more && attempts < 10; attempts++ {
		resumed, openErr := OpenFolderTransport(target, FolderTransportOptions{
			MaxObjects: 1,
			StateStore: state,
		})
		require.NoError(t, openErr)
		result, exchangeErr := resumed.Exchange(
			t.Context(), publishedStore, origin,
		)
		require.NoError(t, exchangeErr)
		require.NoError(t, resumed.Close())
		published += result.Published
		more = result.More
	}
	assert.False(t, more)
	assert.Equal(t, 1, published)
	assert.FileExists(t, checkpointPath)
	assert.Equal(t, journalSequence+1, readTestFolderJournalSequence(t, target))

	kindRoot, err := os.OpenRoot(filepath.Dir(checkpointPath))
	require.NoError(t, err)
	require.NoError(t, (&folderTransport{}).writeFolderJournalRejectionLocked(
		kindRoot,
		checkpointWire.Name,
		checkpointIdentity,
	))
	require.NoError(t, kindRoot.Close())
	recovery, err := OpenFolderTransport(target, FolderTransportOptions{
		MaxObjects:      10,
		StateStore:      state,
		RepairPublished: true,
	})
	require.NoError(t, err)
	recoveryResult, err := recovery.Exchange(t.Context(), publishedStore, origin)
	require.NoError(t, err)
	assert.Zero(t, recoveryResult.Published)
	assert.False(t, recoveryResult.More)
	require.NoError(t, recovery.Close())
	assert.Equal(t, journalSequence+2, readTestFolderJournalSequence(t, target))
	assert.NoFileExists(t,
		filepath.Join(
			filepath.Dir(checkpointPath),
			folderJournalRejectionName(checkpointWire.Name),
		),
		"a durable repair event supersedes the rejection marker",
	)
}
