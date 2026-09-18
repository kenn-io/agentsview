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
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	origin := "local-a1b2c3"
	content := newTestArtifactStore(t)
	manifestBody, err := canonicalJSON(manifest{
		Version:  manifestFormatVersion,
		Origin:   origin,
		Segments: []string{},
	})
	require.NoError(err)
	manifestIdentity := identityForBytes(t, manifestBody)
	manifestRef, err := NewRef(
		origin,
		KindManifests,
		manifestIdentity.SHA256+".json",
	)
	require.NoError(err)
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
	require.NoError(err)

	entries, cursor, more, err := store.folderTransportPage(
		t.Context(),
		folderPushCursor{Origin: origin},
		folderExchangeMaxObjects,
		folderExchangeMaxBytes,
	)

	require.NoError(err)
	assert.Empty(entries)
	assert.True(more)
	assert.Equal("session-0511", cursor.PublicationSessionID)
	assert.Equal(1, authority.pageCalls,
		"one exchange may inspect only one bounded publication page per kind")

	entries, cursor, more, err = store.folderTransportPage(
		t.Context(),
		cursor,
		folderExchangeMaxObjects,
		folderExchangeMaxBytes,
	)
	require.NoError(err)
	assert.Len(entries, folderExchangeMaxObjects)
	assert.True(more)
	assert.Equal(1, cursor.KindIndex)
	assert.Equal("session-0127", cursor.PublicationSessionID)
	assert.Equal(3, authority.pageCalls,
		"resume reads the remaining segment page and one manifest page")
}

func TestAuthoritativePublicationStoreChargesManifestInspection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	origin := "local-a1b2c3"
	content := &manifestOpenCountingStore{ArtifactStore: newTestArtifactStore(t)}
	manifestBody, err := canonicalJSON(manifest{
		Version:  manifestFormatVersion,
		Origin:   origin,
		Segments: []string{},
	})
	require.NoError(err)
	manifestIdentity := identityForBytes(t, manifestBody)
	manifestRef, err := NewRef(
		origin,
		KindManifests,
		manifestIdentity.SHA256+".json",
	)
	require.NoError(err)
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
	require.NoError(err)

	entries, cursor, more, err := store.folderTransportPage(
		t.Context(),
		folderPushCursor{Origin: origin},
		folderExchangeMaxObjects,
		manifestIdentity.Size,
	)

	require.NoError(err)
	assert.Empty(entries)
	assert.True(more)
	assert.Equal("session-0000", cursor.PublicationSessionID)
	assert.Equal(1, content.manifestOpens)
	assert.Equal(manifestIdentity.Size, content.manifestBytes)
}

func TestAuthoritativePublicationStoreReadsOnlyHeadDuringConstruction(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.NoError(err)
	require.NotNil(store)
	assert.Equal(1, authority.headCalls)
	assert.Zero(authority.pageCalls,
		"an unchanged exchange compares the head before reading publications")
}

func TestAuthoritativePublicationStorePagesChangedPublications(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	createTestStoreArtifact(t, content, checkpointRef, checkpointBody)
	store, err := newAuthoritativePublicationStore(
		t.Context(),
		authority,
		content,
		origin,
	)
	require.NoError(err)

	entries, _, more, err := store.folderTransportPage(
		t.Context(),
		folderPushCursor{Origin: origin},
		folderExchangeMaxObjects,
		folderExchangeMaxBytes,
	)

	require.NoError(err)
	assert.False(more)
	require.Len(entries, 1)
	assert.Equal(checkpointRef, entries[0].Ref)
	assert.Equal(2, authority.pageCalls,
		"segments and manifests each use one bounded publication traversal")
	require.Len(authority.pageLimits, 2)
	assert.LessOrEqual(maxInt(authority.pageLimits), transportStorePageSize)
}

func TestFolderTransportNoOpSkipsAuthoritativePublicationPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	createTestStoreArtifact(t, content, checkpointRef, checkpointBody)
	transport, err := OpenFolderTransport(t.TempDir(), FolderTransportOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(transport.Close()) })

	firstStore, err := newAuthoritativePublicationStore(
		t.Context(), authority, content, origin,
	)
	require.NoError(err)
	first, err := transport.Exchange(t.Context(), firstStore, origin)
	require.NoError(err)
	assert.Equal(1, first.Published)
	assert.Equal(2, authority.pageCalls)

	secondStore, err := newAuthoritativePublicationStore(
		t.Context(), authority, content, origin,
	)
	require.NoError(err)
	second, err := transport.Exchange(t.Context(), secondStore, origin)
	require.NoError(err)
	assert.Equal(ExchangeResult{}, second)
	assert.Equal(2, authority.pageCalls,
		"an unchanged head must not inspect any publication page")
}

func TestFolderTransportResumesBoundedPublishedRepairAfterReopen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	manifestIdentity := identityForBytes(t, manifestBody)
	manifestRef, err := NewRef(
		origin,
		KindManifests,
		manifestIdentity.SHA256+".json",
	)
	require.NoError(err)
	createTestStoreArtifact(t, content, manifestRef, manifestBody)
	checkpointBody := []byte("checkpoint")
	checkpointIdentity := identityForBytes(t, checkpointBody)
	checkpointRef, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(err)
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
	require.NoError(err)
	state := &testFolderTransportStateStore{}
	target := t.TempDir()
	initial, err := OpenFolderTransport(target, FolderTransportOptions{
		MaxObjects: 10,
		StateStore: state,
	})
	require.NoError(err)
	initialResult, err := initial.Exchange(t.Context(), publishedStore, origin)
	require.NoError(err)
	assert.Equal(4, initialResult.Published)
	assert.False(initialResult.More)
	require.NoError(initial.Close())

	checkpointWire, err := ToWireRef(checkpointRef)
	require.NoError(err)
	checkpointPath := filepath.Join(
		target,
		checkpointWire.Origin,
		string(checkpointWire.Kind),
		checkpointWire.Name,
	)
	require.NoError(os.Remove(checkpointPath))
	journalSequence := readTestFolderJournalSequence(t, target)
	repair, err := OpenFolderTransport(target, FolderTransportOptions{
		MaxObjects:      1,
		StateStore:      state,
		RepairPublished: true,
	})
	require.NoError(err)
	firstRepair, err := repair.Exchange(t.Context(), publishedStore, origin)
	require.NoError(err)
	assert.Zero(firstRepair.Published)
	assert.True(firstRepair.More)
	require.NoError(repair.Close())

	published := 0
	more := true
	for attempts := 0; more && attempts < 10; attempts++ {
		resumed, openErr := OpenFolderTransport(target, FolderTransportOptions{
			MaxObjects: 1,
			StateStore: state,
		})
		require.NoError(openErr)
		result, exchangeErr := resumed.Exchange(
			t.Context(), publishedStore, origin,
		)
		require.NoError(exchangeErr)
		require.NoError(resumed.Close())
		published += result.Published
		more = result.More
	}
	assert.False(more)
	assert.Equal(1, published)
	assert.FileExists(checkpointPath)
	assert.Equal(journalSequence+1, readTestFolderJournalSequence(t, target))

	kindRoot, err := os.OpenRoot(filepath.Dir(checkpointPath))
	require.NoError(err)
	require.NoError((&folderTransport{}).writeFolderJournalRejectionLocked(
		kindRoot,
		checkpointWire.Name,
		checkpointIdentity,
	))
	require.NoError(kindRoot.Close())
	recovery, err := OpenFolderTransport(target, FolderTransportOptions{
		MaxObjects:      10,
		StateStore:      state,
		RepairPublished: true,
	})
	require.NoError(err)
	recoveryResult, err := recovery.Exchange(t.Context(), publishedStore, origin)
	require.NoError(err)
	assert.Zero(recoveryResult.Published)
	assert.False(recoveryResult.More)
	require.NoError(recovery.Close())
	assert.Equal(journalSequence+2, readTestFolderJournalSequence(t, target))
	assert.NoFileExists(filepath.Join(
		filepath.Dir(checkpointPath),
		folderJournalRejectionName(checkpointWire.Name),
	),
		"a durable repair event supersedes the rejection marker",
	)
}
