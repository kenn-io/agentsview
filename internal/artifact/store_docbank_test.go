package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
)

func TestDocbankStoreUsesCanonicalNamespaceAndMovesQuarantineNode(t *testing.T) {
	vault, store := newTestDocbankStore(t, docbank.Config{})
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000042.json")
	body := []byte(`{"origin":"contract-a1b2c3","sequence":42}`)
	created := createContractArtifact(t, store, ref, body)

	livePath := "/v1/contract-a1b2c3/checkpoints/cp-0000000042.json"
	live, err := vault.Stat(t.Context(), livePath)
	require.NoError(t, err)
	assert.Equal(t, created.Entry.Identity.SHA256, live.BlobHash)

	require.NoError(t, store.Quarantine(t.Context(), ref, "semantic validation failed"))
	_, err = vault.Stat(t.Context(), livePath)
	assert.ErrorIs(t, err, docbank.ErrNotFound)

	quarantined := walkDocbankTestEntries(t, vault, docbankQuarantineRoot)
	require.Len(t, quarantined, 1)
	assert.Equal(t, live.ID, quarantined[0].Node.ID, "quarantine must move the stable node")
	assert.Equal(t, live.BlobHash, quarantined[0].Node.BlobHash, "quarantine must not copy content")
	assert.Regexp(t,
		regexp.MustCompile(`^/\.quarantine/v1/contract-a1b2c3/checkpoints/[0-9a-f]{32}-cp-0000000042\.json$`),
		quarantined[0].Path,
	)

	replacement := []byte("trusted replacement")
	recreated := createContractArtifact(t, store, ref, replacement)
	assert.True(t, recreated.Created)
	assert.NotEqual(t, live.ID, mustDocbankNode(t, vault, livePath).ID)
	assert.Equal(t, replacement, readContractArtifact(t, store, ref))
}

func TestDocbankStoreRejectsReferenceAndNodeIdentityMismatchBeforeRead(t *testing.T) {
	vault, store := newTestDocbankStore(t, docbank.Config{})
	body := []byte("catalog-authorized but incorrectly named content")
	identity := identityForBytes(t, body)
	wrongHash := strings.Repeat("a", 64)
	require.NotEqual(t, identity.SHA256, wrongHash)
	ref := requireContractRef(t, contractOrigin, KindRaw, wrongHash)
	_, err := vault.Create(t.Context(), docbankPath(ref), bytes.NewReader(body), docbank.CreateOptions{
		MediaType: "application/octet-stream",
		Expected:  docbank.ContentIdentity{SHA256: identity.SHA256, Size: identity.Size},
	})
	require.NoError(t, err)

	_, err = store.Stat(t.Context(), ref)
	assert.ErrorIs(t, err, ErrArtifactCorrupt)
	_, reader, err := store.Open(t.Context(), ref)
	assert.Nil(t, reader)
	assert.ErrorIs(t, err, ErrArtifactCorrupt)
}

func TestDocbankStorePreservesTypedDocbankCauses(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")

	_, err := store.Stat(t.Context(), ref)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
	assert.ErrorIs(t, err, docbank.ErrNotFound)

	body := []byte("actual bytes")
	expected := identityForBytes(t, bytes.Repeat([]byte("x"), len(body)))
	require.Equal(t, int64(len(body)), expected.Size)
	_, err = store.Create(t.Context(), ref, expected, "application/json", bytes.NewReader(body))
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.ErrorIs(t, err, docbank.ErrDigestMismatch)

	created := createContractArtifact(t, store, ref, body)
	_, err = store.Create(t.Context(), ref, created.Entry.Identity,
		"application/octet-stream", bytes.NewReader(body))
	assert.ErrorIs(t, err, ErrArtifactConflict)
}

func TestDocbankStoreIdempotentRetryDoesNotReportPhysicalWrite(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
	body := []byte(`{"origin":"contract-a1b2c3","sequence":1}`)
	identity := identityForBytes(t, body)

	first, err := store.Create(t.Context(), ref, identity, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	assert.True(t, first.Created)
	assert.NotEqual(t, PhysicalWrite{}, first.Physical)

	retry, err := store.Create(t.Context(), ref, identity, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	assert.False(t, retry.Created)
	assert.Equal(t, first.Entry, retry.Entry)
	assert.Equal(t, PhysicalWrite{}, retry.Physical)
}

func TestDocbankStoreDistinctReferencesCountOnePhysicalWrite(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})

	body := []byte(`{"origin":"contract-a1b2c3","shared":"physical-content"}`)
	first := createCheckpointBody(t, store, 1, body)
	second := createCheckpointBody(t, store, 2, body)
	assert.True(t, first.Created)
	assert.True(t, second.Created, "the second logical reference is new")
	assert.NotEqual(t, PhysicalWrite{}, first.Physical)
	assert.Equal(t, PhysicalWrite{}, second.Physical,
		"the second logical reference must not claim a duplicate physical publication")
}

func TestDocbankStoreReportsConfiguredLooseCompressionAndBacklog(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{LooseCompression: docbank.LooseCompressionOptions{
		Enabled:           true,
		MinBytes:          4 << 10,
		MinSavingsPercent: 10,
	}})

	compressible := bytes.Repeat([]byte("canonical manifest payload\n"), 200)
	compressible = compressible[:4<<10]
	compressed := createCheckpointBody(t, store, 1, compressible)
	assert.Equal(t, "loose", compressed.Physical.Kind)
	assert.Equal(t, "zstd", compressed.Physical.Encoding)
	assert.Equal(t, int64(len(compressible)), compressed.Physical.LogicalBytes)
	assert.Less(t, compressed.Physical.StoredBytes, int64(len(compressible))*9/10)

	belowThreshold := bytes.Repeat([]byte("x"), (4<<10)-1)
	rawSmall := createCheckpointBody(t, store, 2, belowThreshold)
	assert.Equal(t, "raw", rawSmall.Physical.Encoding)

	incompressible := deterministicDocbankBytes(4 << 10)
	rawSavings := createCheckpointBody(t, store, 3, incompressible)
	assert.Equal(t, "raw", rawSavings.Physical.Encoding)

	assertCanonical := func(result CreateResult, want []byte) {
		t.Helper()
		entry, reader, err := store.Open(t.Context(), result.Entry.Ref)
		require.NoError(t, err)
		got, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Verify())
		require.NoError(t, reader.Close())
		assert.Equal(t, want, got)
		assert.Equal(t, identityForBytes(t, want), entry.Identity,
			"physical encoding must not change the canonical SHA-256 or size")
	}
	artifacts := []struct {
		result CreateResult
		body   []byte
	}{
		{result: compressed, body: compressible},
		{result: rawSmall, body: belowThreshold},
		{result: rawSavings, body: incompressible},
	}
	for _, artifact := range artifacts {
		assertCanonical(artifact.result, artifact.body)
	}

	backlog, err := store.LooseBacklog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3), backlog.EligibleObjects)
	assert.Equal(t, int64(len(compressible)+len(belowThreshold)+len(incompressible)),
		backlog.EligibleBytes)
	assert.Equal(t,
		compressed.Physical.StoredBytes+rawSmall.Physical.StoredBytes+rawSavings.Physical.StoredBytes,
		backlog.EligibleStoredBytes,
		"the indexed backlog must report physical loose bytes after compression",
	)

	packed, err := store.Pack(t.Context(), 1<<20)
	require.NoError(t, err)
	assert.Equal(t, 3, packed.PackedObjects)
	assert.Equal(t, backlog.EligibleBytes, packed.LogicalBytes)
	assert.False(t, packed.More)
	for _, artifact := range artifacts {
		assertCanonical(artifact.result, artifact.body)
	}
	backlog, err = store.LooseBacklog(t.Context())
	require.NoError(t, err)
	assert.Zero(t, backlog.EligibleObjects)
	assert.Zero(t, backlog.EligibleBytes)
	assert.Zero(t, backlog.EligibleStoredBytes)
}

func TestCollectDocbankWalkJoinsCleanupErrors(t *testing.T) {
	nextErr := errors.New("walk page failed")
	closeErr := errors.New("walk cleanup failed")
	entry := docbank.WalkEntry{Path: "/v1"}
	for _, test := range []struct {
		name       string
		walker     *docbankWalkerStub
		want       []docbank.WalkEntry
		wantErrors []error
	}{
		{
			name:   "EOF remains successful",
			walker: &docbankWalkerStub{pages: [][]docbank.WalkEntry{{entry}}},
			want:   []docbank.WalkEntry{entry},
		},
		{
			name:       "EOF exposes cleanup failure",
			walker:     &docbankWalkerStub{pages: [][]docbank.WalkEntry{{entry}}, closeErr: closeErr},
			want:       []docbank.WalkEntry{entry},
			wantErrors: []error{closeErr},
		},
		{
			name:       "page and cleanup failures are joined",
			walker:     &docbankWalkerStub{nextErr: nextErr, closeErr: closeErr},
			wantErrors: []error{nextErr, closeErr},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries, err := collectDocbankWalk(t.Context(), func(
				context.Context,
			) (docbankWalker, error) {
				return test.walker, nil
			})
			assert.Equal(t, test.want, entries)
			if len(test.wantErrors) == 0 {
				assert.NoError(t, err)
			}
			for _, wantErr := range test.wantErrors {
				assert.ErrorIs(t, err, wantErr)
			}
			assert.Equal(t, 1, test.walker.closeCalls)
		})
	}
}

func TestWalkCheckpointFloorStreamsAllQuarantinePages(t *testing.T) {
	collection := docbankQuarantineRoot + "/" + contractOrigin + "/" + string(KindCheckpoints)
	prefix := collection + "/" + strings.Repeat("a", 32) + "-"
	firstPage := make([]docbank.WalkEntry, checkpointFloorPageSize)
	for i := range firstPage {
		firstPage[i] = docbank.WalkEntry{
			Path: prefix + fmt.Sprintf("cp-%010d.json", i+1),
			Node: docbank.Node{BlobHash: "blob"},
		}
	}
	walker := &docbankWalkerStub{pages: [][]docbank.WalkEntry{
		firstPage,
		{
			{Path: collection + "/malformed", Node: docbank.Node{BlobHash: "blob"}},
			{Path: prefix + "not-a-checkpoint", Node: docbank.Node{BlobHash: "blob"}},
			{Path: prefix + "cp-0000000900.json", Node: docbank.Node{BlobHash: "blob"}},
		},
	}}

	floor, err := walkCheckpointFloor(t.Context(), collection, true, func(
		context.Context,
	) (docbankWalker, error) {
		return walker, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 900, floor)
	assert.Equal(t, 1, walker.closeCalls)
}

func TestWalkCheckpointFloorSkipsMalformedLiveNamesAcrossPages(t *testing.T) {
	collection := docbankLiveRoot + "/" + contractOrigin + "/" + string(KindCheckpoints)
	walker := &docbankWalkerStub{pages: [][]docbank.WalkEntry{
		{
			{Path: collection + "/cp-0000000007.json", Node: docbank.Node{BlobHash: "blob"}},
			{Path: collection + "/cp-malformed.json", Node: docbank.Node{BlobHash: "blob"}},
		},
		{
			{Path: collection + "/nested/cp-0000000999.json", Node: docbank.Node{BlobHash: "blob"}},
			{Path: collection + "/cp-0000000042.json", Node: docbank.Node{BlobHash: "blob"}},
		},
	}}

	floor, err := walkCheckpointFloor(t.Context(), collection, false, func(
		context.Context,
	) (docbankWalker, error) {
		return walker, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 42, floor)
}

func TestWalkCheckpointFloorPropagatesCancellationAndCloseError(t *testing.T) {
	closeErr := errors.New("close walker")
	ctx, cancel := context.WithCancel(t.Context())
	walker := &docbankWalkerStub{
		pages:    [][]docbank.WalkEntry{{}},
		closeErr: closeErr,
		onNext:   cancel,
	}

	_, err := walkCheckpointFloor(ctx, "/v1/origin/checkpoints", false, func(
		context.Context,
	) (docbankWalker, error) {
		return walker, nil
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, err, closeErr)
}

func TestDocbankIteratorClosesWalkerExactlyOnce(t *testing.T) {
	t.Run("explicit close", func(t *testing.T) {
		walker := &docbankWalkerStub{}
		iterator := &docbankOriginIterator{
			iterator: docbankIterator{
				opened: true,
				state:  docbankTraversal{walker: walker},
			},
		}

		require.NoError(t, iterator.Close())
		require.NoError(t, iterator.Close())
		assert.Equal(t, 1, walker.closeCalls)
		_, err := iterator.Next(t.Context(), 1)
		assert.ErrorIs(t, err, fs.ErrClosed)
	})

	t.Run("cancellation", func(t *testing.T) {
		walker := &docbankWalkerStub{}
		iterator := &docbankOriginIterator{
			iterator: docbankIterator{
				opened: true,
				state:  docbankTraversal{walker: walker},
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := iterator.Next(ctx, 1)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, walker.closeCalls)
		_, err = iterator.Next(t.Context(), 1)
		assert.ErrorIs(t, err, fs.ErrClosed)
	})

	t.Run("end of iteration", func(t *testing.T) {
		walker := &docbankWalkerStub{}
		iterator := &docbankOriginIterator{
			iterator: docbankIterator{
				opened: true,
				state:  docbankTraversal{walker: walker},
			},
		}

		_, err := iterator.Next(t.Context(), 1)
		assert.ErrorIs(t, err, io.EOF)
		assert.Equal(t, 1, walker.closeCalls)
		_, err = iterator.Next(t.Context(), 1)
		assert.ErrorIs(t, err, io.EOF)
		assert.Equal(t, 1, walker.closeCalls)
	})
}

type docbankWalkerStub struct {
	pages      [][]docbank.WalkEntry
	nextErr    error
	closeErr   error
	next       int
	closeCalls int
	onNext     func()
}

func (w *docbankWalkerStub) Next(context.Context) ([]docbank.WalkEntry, error) {
	if w.onNext != nil {
		w.onNext()
		w.onNext = nil
	}
	if w.next < len(w.pages) {
		page := w.pages[w.next]
		w.next++
		return page, nil
	}
	if w.nextErr != nil {
		return nil, w.nextErr
	}
	return nil, io.EOF
}

func (w *docbankWalkerStub) Close() error {
	w.closeCalls++
	return w.closeErr
}

func newTestDocbankStore(t *testing.T, config docbank.Config) (*docbank.Vault, *docbankStore) {
	t.Helper()
	if config.Root == "" {
		config.Root = t.TempDir()
	}
	vault, err := docbank.New(t.Context(), config)
	require.NoError(t, err)
	store := newDocbankContent(vault)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return vault, store
}

func newTestArtifactStore(t *testing.T) *docbankStore {
	t.Helper()
	_, store := newTestDocbankStore(t, docbank.Config{})
	return store
}

func createMetadataArtifactInStore(
	t *testing.T, store ArtifactStore, event metadataEvent,
) Ref {
	t.Helper()
	stamp, err := ParseHLCTimestamp(event.HLC)
	require.NoError(t, err)
	data, err := canonicalJSON(event)
	require.NoError(t, err)
	hash := hashHex(data)
	ref, err := NewRef(
		event.Origin, KindMeta, stamp.OrderingKey(hash)+metadataEventExtension,
	)
	require.NoError(t, err)
	createContractArtifact(t, store, ref, data)
	return ref
}

// newProtocolTestStore opens a real isolated Docbank store at root without
// registering test cleanup. Callers use it for explicit close/reopen protocol
// and transport scenarios.
func newProtocolTestStore(root string) (*docbankStore, error) {
	vault, err := docbank.New(context.Background(), docbank.Config{Root: root})
	if err != nil {
		return nil, err
	}
	return newDocbankContent(vault), nil
}

func walkDocbankTestEntries(
	t *testing.T, vault *docbank.Vault, root string,
) []docbank.WalkEntry {
	t.Helper()
	walker, err := vault.Walk(t.Context(), root, docbank.WalkOptions{PageSize: 2})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })
	var entries []docbank.WalkEntry
	for {
		page, err := walker.Next(t.Context())
		if errors.Is(err, io.EOF) {
			return entries
		}
		require.NoError(t, err)
		for _, entry := range page {
			if entry.Node.BlobHash != "" {
				entries = append(entries, entry)
			}
		}
	}
}

func mustDocbankNode(t *testing.T, vault *docbank.Vault, path string) docbank.Node {
	t.Helper()
	node, err := vault.Stat(t.Context(), path)
	require.NoError(t, err)
	return node
}

func createCheckpointBody(
	t *testing.T, store ArtifactStore, sequence int, body []byte,
) CreateResult {
	t.Helper()
	ref := requireContractRef(t, contractOrigin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", sequence))
	return createContractArtifact(t, store, ref, body)
}

func deterministicDocbankBytes(size int) []byte {
	data := make([]byte, 0, size)
	for counter := 0; len(data) < size; counter++ {
		sum := sha256.Sum256([]byte(strconv.Itoa(counter)))
		data = append(data, sum[:]...)
	}
	return data[:size]
}
