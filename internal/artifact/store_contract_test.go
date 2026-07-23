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
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/mattn"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

const contractOrigin = "contract-a1b2c3"

func TestNewRefValidatesCanonicalReferences(t *testing.T) {
	hash := strings.Repeat("a", 64)
	tests := []struct {
		name string
		kind Kind
		ref  string
	}{
		{name: "checkpoint", kind: KindCheckpoints, ref: "cp-0000000001.json"},
		{name: "manifest", kind: KindManifests, ref: hash + ".json"},
		{name: "segment", kind: KindSegments, ref: hash + ".ndjson"},
		{name: "metadata", kind: KindMeta, ref: "20260721T010203.000000000Z-0-" + hash + ".json"},
		{name: "raw", kind: KindRaw, ref: hash},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewRef(contractOrigin, tt.kind, tt.ref)
			require.NoError(t, err)
			assert.Equal(t, Ref{Origin: contractOrigin, Kind: tt.kind, Name: tt.ref}, got)
		})
	}
}

func TestNewRefRejectsNoncanonicalReferences(t *testing.T) {
	hash := strings.Repeat("a", 64)
	tests := []struct {
		name   string
		origin string
		kind   Kind
		ref    string
	}{
		{name: "missing origin", kind: KindRaw, ref: hash},
		{name: "unknown kind", origin: contractOrigin, kind: "future", ref: hash},
		{name: "checkpoint without extension", origin: contractOrigin, kind: KindCheckpoints, ref: "cp-0000000001"},
		{name: "manifest wire extension", origin: contractOrigin, kind: KindManifests, ref: hash + ".json.zst"},
		{name: "segment wire extension", origin: contractOrigin, kind: KindSegments, ref: hash + ".ndjson.zst"},
		{name: "metadata without extension", origin: contractOrigin, kind: KindMeta, ref: "clock-" + hash},
		{name: "uppercase hash", origin: contractOrigin, kind: KindRaw, ref: strings.Repeat("A", 64)},
		{name: "path separator", origin: contractOrigin, kind: KindRaw, ref: "../" + hash},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRef(tt.origin, tt.kind, tt.ref)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
		})
	}
}

func TestNewIdentityValidatesCanonicalSHA256AndSize(t *testing.T) {
	hash := strings.Repeat("a", 64)
	identity, err := NewIdentity(hash, 0)
	require.NoError(t, err)
	assert.Equal(t, Identity{SHA256: hash, Size: 0}, identity)

	tests := []struct {
		name string
		hash string
		size int64
	}{
		{name: "missing hash"},
		{name: "short hash", hash: "abcd"},
		{name: "uppercase hash", hash: strings.Repeat("A", 64)},
		{name: "negative size", hash: hash, size: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewIdentity(tt.hash, tt.size)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
		})
	}
}

func TestArtifactOpErrorPreservesCause(t *testing.T) {
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
	err := &ArtifactOpError{Op: "open", Ref: ref, Err: context.Canceled}

	assert.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "open")
	assert.Contains(t, err.Error(), ref.Name)

	transient := fmt.Errorf("writing artifact: %w", syscall.EAGAIN)
	err = &ArtifactOpError{Op: "create", Ref: ref, Err: transient}
	assert.ErrorIs(t, err, syscall.EAGAIN)
}

type artifactStoreFactory func(t *testing.T) ArtifactStore

func TestArtifactStoreContractDocbank(t *testing.T) {
	for _, driver := range []docsqlite.Driver{mattn.Driver{}, modernc.Driver{}} {
		t.Run(driver.Name(), func(t *testing.T) {
			runArtifactStoreContract(t, func(t *testing.T) ArtifactStore {
				vault, err := docbank.New(t.Context(), docbank.Config{
					Root:   t.TempDir(),
					SQLite: driver,
				})
				require.NoError(t, err)
				return newDocbankContent(vault)
			})
		})
	}
}

func TestArtifactStoreIteratorContractDocbank(t *testing.T) {
	for _, driver := range []docsqlite.Driver{mattn.Driver{}, modernc.Driver{}} {
		t.Run(driver.Name(), func(t *testing.T) {
			store := newContractStore(t, func(t *testing.T) ArtifactStore {
				vault, err := docbank.New(t.Context(), docbank.Config{
					Root: t.TempDir(), SQLite: driver,
				})
				require.NoError(t, err)
				return newDocbankContent(vault)
			})
			iterable := store

			originalOrigins := []string{
				"alpha-a1b2c3",
				"charlie-c3d4e5",
				"echo-e5f6a7",
				"foxtrot-f6a7b8",
			}
			for i, origin := range originalOrigins {
				ref := requireContractRef(t, origin, KindCheckpoints, "cp-0000000001.json")
				createContractArtifact(t, store, ref, []byte{byte(i + 1)})
			}

			origins, err := iterable.Origins(t.Context())
			require.NoError(t, err)
			firstOrigins, err := origins.Next(t.Context(), 1)
			require.NoError(t, err)
			assert.Equal(t, originalOrigins[:1], firstOrigins)

			insertedOrigin := requireContractRef(
				t, "bravo-b2c3d4", KindCheckpoints, "cp-0000000001.json",
			)
			createContractArtifact(t, store, insertedOrigin, []byte("inserted origin"))
			restOrigins, err := origins.Next(t.Context(), 3)
			require.ErrorIs(t, err, io.EOF)
			assert.Equal(t, originalOrigins[1:], restOrigins)
			_, err = origins.Next(t.Context(), 1)
			assert.ErrorIs(t, err, io.EOF)
			require.NoError(t, origins.Close())

			originalNames := []string{
				"cp-0000000001.json",
				"cp-0000000003.json",
				"cp-0000000005.json",
				"cp-0000000007.json",
			}
			for _, name := range originalNames {
				ref := requireContractRef(t, contractOrigin, KindCheckpoints, name)
				createContractArtifact(t, store, ref, []byte(name))
			}
			entries, err := iterable.Entries(t.Context(), contractOrigin, KindCheckpoints)
			require.NoError(t, err)
			firstEntries, err := entries.Next(t.Context(), 1)
			require.NoError(t, err)
			assert.Equal(t, originalNames[:1], entryNames(firstEntries))

			insertedEntry := requireContractRef(
				t, contractOrigin, KindCheckpoints, "cp-0000000002.json",
			)
			createContractArtifact(t, store, insertedEntry, []byte(insertedEntry.Name))
			restEntries, err := entries.Next(t.Context(), 3)
			require.ErrorIs(t, err, io.EOF)
			assert.Equal(t, originalNames[1:], entryNames(restEntries))
			_, err = entries.Next(t.Context(), 1)
			assert.ErrorIs(t, err, io.EOF)
			require.NoError(t, entries.Close())

			cancelEntries, err := iterable.Entries(t.Context(), contractOrigin, KindCheckpoints)
			require.NoError(t, err)
			_, err = cancelEntries.Next(t.Context(), 1)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = cancelEntries.Next(ctx, 1)
			assert.ErrorIs(t, err, context.Canceled)
			_, err = cancelEntries.Next(t.Context(), 1)
			assert.ErrorIs(t, err, fs.ErrClosed)

			closedOrigins, err := iterable.Origins(t.Context())
			require.NoError(t, err)
			require.NoError(t, closedOrigins.Close())
			require.NoError(t, closedOrigins.Close())
			_, err = closedOrigins.Next(t.Context(), 1)
			assert.ErrorIs(t, err, fs.ErrClosed)
		})
	}
}

// runArtifactStoreContract exercises only the logical API. Backends register
// this helper from their own top-level tests and may not expose physical paths
// or implementation handles to it.
func runArtifactStoreContract(t *testing.T, factory artifactStoreFactory) {
	t.Helper()

	t.Run("missing reads", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")

		_, err := store.Stat(t.Context(), ref)
		assert.ErrorIs(t, err, ErrArtifactNotFound)
		_, reader, err := store.Open(t.Context(), ref)
		assert.Nil(t, reader)
		assert.ErrorIs(t, err, ErrArtifactNotFound)
	})

	t.Run("identical retry is immutable and idempotent", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		body := []byte(`{"origin":"contract-a1b2c3","sequence":1}`)
		identity := identityForBytes(t, body)

		first, err := store.Create(t.Context(), ref, identity, "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		assert.True(t, first.Created)
		assert.Equal(t, ref, first.Entry.Ref)
		assert.Equal(t, identity, first.Entry.Identity)
		assert.False(t, first.Entry.Modified.IsZero())

		retry, err := store.Create(t.Context(), ref, identity, "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		assert.False(t, retry.Created)
		assert.Equal(t, first.Entry, retry.Entry)
		assert.Equal(t, body, readContractArtifact(t, store, ref))
	})

	t.Run("different expected identity conflicts without mutation", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		original := []byte("original checkpoint")
		replacement := []byte("replacement checkpoint")
		createContractArtifact(t, store, ref, original)

		_, err := store.Create(t.Context(), ref, identityForBytes(t, replacement),
			"application/json", bytes.NewReader(replacement))
		assert.ErrorIs(t, err, ErrArtifactConflict)
		assert.Equal(t, original, readContractArtifact(t, store, ref))
	})

	t.Run("duplicate path rejects stream mismatching expected identity", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		original := []byte("original checkpoint")
		replacement := []byte("different bytes with the original expected identity")
		first := createContractArtifact(t, store, ref, original)

		_, err := store.Create(t.Context(), ref, first.Entry.Identity,
			"application/json", bytes.NewReader(replacement))
		assert.ErrorIs(t, err, ErrArtifactInvalid)
		entry, err := store.Stat(t.Context(), ref)
		require.NoError(t, err)
		assert.Equal(t, first.Entry, entry)
		assert.Equal(t, original, readContractArtifact(t, store, ref))
	})

	t.Run("duplicate path rejects media type mismatch", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		body := []byte("original checkpoint")
		first := createContractArtifact(t, store, ref, body)

		_, err := store.Create(t.Context(), ref, first.Entry.Identity,
			"application/octet-stream", bytes.NewReader(body))
		assert.ErrorIs(t, err, ErrArtifactConflict)
		entry, err := store.Stat(t.Context(), ref)
		require.NoError(t, err)
		assert.Equal(t, first.Entry, entry)
		retry, err := store.Create(t.Context(), ref, first.Entry.Identity,
			"application/json", bytes.NewReader(body))
		require.NoError(t, err)
		assert.False(t, retry.Created)
		assert.Equal(t, first.Entry, retry.Entry)
		assert.Equal(t, body, readContractArtifact(t, store, ref))
	})

	t.Run("expected identity mismatch creates nothing", func(t *testing.T) {
		tests := []struct {
			name     string
			identity func(t *testing.T, body []byte) Identity
		}{
			{
				name: "hash",
				identity: func(t *testing.T, body []byte) Identity {
					different := []byte("malicious checkpoint")
					require.Len(t, different, len(body))
					require.NotEqual(t, body, different)
					return identityForBytes(t, different)
				},
			},
			{
				name: "size",
				identity: func(t *testing.T, body []byte) Identity {
					correct := identityForBytes(t, body)
					identity, err := NewIdentity(correct.SHA256, correct.Size+1)
					require.NoError(t, err)
					return identity
				},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				store := newContractStore(t, factory)
				ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
				body := []byte("canonical checkpoint")

				_, err := store.Create(t.Context(), ref, tt.identity(t, body),
					"application/json", bytes.NewReader(body))
				assert.ErrorIs(t, err, ErrArtifactInvalid)
				_, err = store.Stat(t.Context(), ref)
				assert.ErrorIs(t, err, ErrArtifactNotFound)
			})
		}

		t.Run("hash-bearing reference", func(t *testing.T) {
			body := []byte("canonical artifact content")
			identity := identityForBytes(t, body)
			other := identityForBytes(t, []byte("different artifact content"))
			tests := []struct {
				name      string
				kind      Kind
				refName   string
				mediaType string
			}{
				{name: "manifest", kind: KindManifests, refName: other.SHA256 + ".json", mediaType: "application/json"},
				{name: "segment", kind: KindSegments, refName: other.SHA256 + ".ndjson", mediaType: "application/x-ndjson"},
				{
					name: "metadata", kind: KindMeta,
					refName: "20260721T010203.000000000Z-0-" + other.SHA256 + ".json", mediaType: "application/json",
				},
				{name: "raw", kind: KindRaw, refName: other.SHA256, mediaType: "application/octet-stream"},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					store := newContractStore(t, factory)
					ref := requireContractRef(t, contractOrigin, tt.kind, tt.refName)

					_, err := store.Create(t.Context(), ref, identity,
						tt.mediaType, bytes.NewReader(body))
					assert.ErrorIs(t, err, ErrArtifactInvalid)
					_, err = store.Stat(t.Context(), ref)
					assert.ErrorIs(t, err, ErrArtifactNotFound)
				})
			}
		})
	})

	t.Run("open verifies a streamed read", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		body := []byte("streamed checkpoint content")
		want := createContractArtifact(t, store, ref, body).Entry

		entry, reader, err := store.Open(t.Context(), ref)
		require.NoError(t, err)
		require.NotNil(t, reader)
		assert.Equal(t, want, entry)
		prefix := make([]byte, 8)
		_, err = io.ReadFull(reader, prefix)
		require.NoError(t, err)
		assert.Equal(t, []byte("streamed"), prefix)
		require.NoError(t, reader.Verify())
		assert.NoError(t, reader.Close())

		entry, reader, err = store.Open(t.Context(), ref)
		require.NoError(t, err)
		assert.Equal(t, want, entry)
		got, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, body, got)
		assert.NoError(t, reader.Verify())
		assert.NoError(t, reader.Close())
	})

	t.Run("early close does not drain or damage content", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		body := []byte("content that must be explicitly verified")
		createContractArtifact(t, store, ref, body)

		_, reader, err := store.Open(t.Context(), ref)
		require.NoError(t, err)
		one := make([]byte, 1)
		_, err = reader.Read(one)
		require.NoError(t, err)
		assert.Error(t, reader.Close())
		assert.Equal(t, body, readContractArtifact(t, store, ref))
	})

	t.Run("quarantine excludes content and permits recreation", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		createContractArtifact(t, store, ref, []byte("invalid current-format checkpoint"))

		require.NoError(t, store.Quarantine(t.Context(), ref, "semantic validation failed"))
		_, err := store.Stat(t.Context(), ref)
		assert.ErrorIs(t, err, ErrArtifactNotFound)
		assert.Empty(t, listAllContractEntries(t, store, contractOrigin, KindCheckpoints, 10))
		assert.Empty(t, listAllContractOrigins(t, store, 10))

		replacement := []byte("trusted replacement checkpoint")
		result := createContractArtifact(t, store, ref, replacement)
		assert.True(t, result.Created)
		assert.Equal(t, replacement, readContractArtifact(t, store, ref))
	})

	t.Run("trash removes content from live reads and enumeration", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		createContractArtifact(t, store, ref, []byte("unreachable checkpoint"))

		require.NoError(t, store.Trash(t.Context(), ref))
		_, err := store.Stat(t.Context(), ref)
		assert.ErrorIs(t, err, ErrArtifactNotFound)
		_, reader, err := store.Open(t.Context(), ref)
		assert.Nil(t, reader)
		assert.ErrorIs(t, err, ErrArtifactNotFound)
		assert.Empty(t, listAllContractEntries(t, store, contractOrigin, KindCheckpoints, 10))
		assert.Empty(t, listAllContractOrigins(t, store, 10))
	})

	t.Run("operations preserve cancellation", func(t *testing.T) {
		store := newContractStore(t, factory)
		existing := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		quarantineRef := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000002.json")
		trashRef := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000003.json")
		createContractArtifact(t, store, existing, []byte("existing"))
		createContractArtifact(t, store, quarantineRef, []byte("quarantine"))
		createContractArtifact(t, store, trashRef, []byte("trash"))
		missing := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000004.json")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := store.Create(ctx, missing, identityForBytes(t, []byte("new")),
			"application/json", strings.NewReader("new"))
		assert.ErrorIs(t, err, context.Canceled)
		_, err = store.Stat(ctx, existing)
		assert.ErrorIs(t, err, context.Canceled)
		_, reader, err := store.Open(ctx, existing)
		if reader != nil {
			_ = reader.Close()
		}
		assert.ErrorIs(t, err, context.Canceled)
		_, err = store.Origins(ctx)
		assert.ErrorIs(t, err, context.Canceled)
		_, err = store.Entries(ctx, contractOrigin, KindCheckpoints)
		assert.ErrorIs(t, err, context.Canceled)
		assert.ErrorIs(t, store.Quarantine(ctx, quarantineRef, "cancelled"), context.Canceled)
		assert.ErrorIs(t, store.Trash(ctx, trashRef), context.Canceled)
		_, err = store.Stat(t.Context(), missing)
		assert.ErrorIs(t, err, ErrArtifactNotFound)
		_, err = store.Stat(t.Context(), quarantineRef)
		assert.NoError(t, err)
		_, err = store.Stat(t.Context(), trashRef)
		assert.NoError(t, err)
	})

	t.Run("concurrent identical creates converge", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		body := []byte("one immutable concurrent value")
		identity := identityForBytes(t, body)
		const writers = 8
		results := make([]CreateResult, writers)
		errs := make([]error, writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(writers)
		for i := range writers {
			go func() {
				defer wg.Done()
				<-start
				results[i], errs[i] = store.Create(t.Context(), ref, identity,
					"application/json", bytes.NewReader(body))
			}()
		}
		close(start)
		wg.Wait()

		created := 0
		for i := range writers {
			assert.NoError(t, errs[i])
			assert.Equal(t, ref, results[i].Entry.Ref)
			assert.Equal(t, identity, results[i].Entry.Identity)
			if results[i].Created {
				created++
			}
		}
		assert.Equal(t, 1, created)
		assert.Equal(t, body, readContractArtifact(t, store, ref))
	})

	t.Run("concurrent distinct creates preserve one winner", func(t *testing.T) {
		store := newContractStore(t, factory)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
		bodies := [2][]byte{
			[]byte("first immutable concurrent value"),
			[]byte("second immutable concurrent value"),
		}
		identities := [2]Identity{
			identityForBytes(t, bodies[0]),
			identityForBytes(t, bodies[1]),
		}
		var results [2]CreateResult
		var errs [2]error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(len(bodies))
		for i := range bodies {
			go func() {
				defer wg.Done()
				<-start
				results[i], errs[i] = store.Create(t.Context(), ref, identities[i],
					"application/json", bytes.NewReader(bodies[i]))
			}()
		}
		close(start)
		wg.Wait()

		winner := -1
		successes := 0
		conflicts := 0
		for i := range bodies {
			if errs[i] == nil {
				winner = i
				successes++
				assert.True(t, results[i].Created)
				assert.Equal(t, ref, results[i].Entry.Ref)
				assert.Equal(t, identities[i], results[i].Entry.Identity)
				continue
			}
			if assert.ErrorIs(t, errs[i], ErrArtifactConflict) {
				conflicts++
			}
		}
		require.NotEqual(t, -1, winner)
		assert.Equal(t, 1, successes)
		assert.Equal(t, 1, conflicts)
		assert.Equal(t, bodies[winner], readContractArtifact(t, store, ref))
		entry, err := store.Stat(t.Context(), ref)
		require.NoError(t, err)
		assert.Equal(t, identities[winner], entry.Identity)
	})
}

func newContractStore(t *testing.T, factory artifactStoreFactory) ArtifactStore {
	t.Helper()
	store := factory(t)
	require.NotNil(t, store)
	t.Cleanup(func() {
		closer, ok := any(store).(io.Closer)
		require.True(t, ok)
		require.NoError(t, closer.Close())
	})
	return store
}

func requireContractRef(t *testing.T, origin string, kind Kind, name string) Ref {
	t.Helper()
	ref, err := NewRef(origin, kind, name)
	require.NoError(t, err)
	return ref
}

func identityForBytes(t *testing.T, body []byte) Identity {
	t.Helper()
	sum := sha256.Sum256(body)
	identity, err := NewIdentity(hex.EncodeToString(sum[:]), int64(len(body)))
	require.NoError(t, err)
	return identity
}

func createContractArtifact(t *testing.T, store ArtifactStore, ref Ref, body []byte) CreateResult {
	t.Helper()
	result, err := store.Create(t.Context(), ref, identityForBytes(t, body),
		canonicalArtifactMediaType(ref.Kind), bytes.NewReader(body))
	require.NoError(t, err)
	return result
}

func readContractArtifact(t *testing.T, store ArtifactStore, ref Ref) []byte {
	t.Helper()
	_, reader, err := store.Open(t.Context(), ref)
	require.NoError(t, err)
	require.NotNil(t, reader)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Verify())
	require.NoError(t, reader.Close())
	return data
}

func listAllContractEntries(
	t *testing.T, store ArtifactStore, origin string, kind Kind, limit int,
) []Entry {
	t.Helper()
	var entries []Entry
	iterator, err := store.Entries(t.Context(), origin, kind)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, iterator.Close()) })
	for {
		page, nextErr := iterator.Next(t.Context(), limit)
		require.True(t, nextErr == nil || errors.Is(nextErr, io.EOF))
		assert.LessOrEqual(t, len(page), limit)
		entries = append(entries, page...)
		if errors.Is(nextErr, io.EOF) {
			break
		}
	}
	return entries
}

func listAllContractOrigins(t *testing.T, store ArtifactStore, limit int) []string {
	t.Helper()
	var origins []string
	iterator, err := store.Origins(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, iterator.Close()) })
	for {
		page, nextErr := iterator.Next(t.Context(), limit)
		require.True(t, nextErr == nil || errors.Is(nextErr, io.EOF))
		assert.LessOrEqual(t, len(page), limit)
		origins = append(origins, page...)
		if errors.Is(nextErr, io.EOF) {
			break
		}
	}
	return origins
}

func entryNames(entries []Entry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Ref.Name)
	}
	return names
}
