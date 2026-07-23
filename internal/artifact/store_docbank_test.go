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
