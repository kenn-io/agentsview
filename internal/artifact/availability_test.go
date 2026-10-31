package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

// The probe must use physical content, not just the surviving logical node.
func TestDocbankAvailabilityWithSurvivingMetadata(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprint(compressed), func(t *testing.T) {
			root := t.TempDir()
			vault, store := newTestDocbankStore(t, docbank.Config{Root: root, LooseCompression: docbank.LooseCompressionOptions{Enabled: compressed, MinBytes: 1}})
			ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
			body := bytes.Repeat([]byte("availability\n"), 1000)
			created := createContractArtifact(t, store, ref, body)
			entry, err := store.ProbeContent(t.Context(), ref, 0)
			require.NoError(t, err)
			require.Equal(t, created.Entry.Identity, entry.Identity)
			_, err = store.ProbeContent(t.Context(), ref, int64(len(body)))
			require.NoError(t, err)
			_, err = store.ProbeContent(t.Context(), ref, 1)
			require.ErrorIs(t, err, ErrArtifactInvalid)
			require.NotErrorIs(t, err, ErrArtifactUnavailable)
			// Test-only physical damage; production never derives Docbank paths.
			layout, err := packstore.NewLayout(filepath.Join(root, "blobs"), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
			require.NoError(t, err)
			hash, err := packstore.ParseHash(created.Entry.Identity.SHA256)
			require.NoError(t, err)
			path := layout.LoosePath(hash)
			if compressed {
				path = layout.CompressedLoosePath(hash)
			}
			require.NoError(t, os.Remove(path))
			_, err = vault.Stat(t.Context(), docbankPath(ref))
			require.NoError(t, err)
			_, err = store.ProbeContent(t.Context(), ref, 0)
			require.ErrorIs(t, err, ErrArtifactUnavailable)
			require.NoError(t, store.RepairContent(t.Context(), created.Entry.Identity, bytes.NewReader(body)))
			require.NoError(t, os.Truncate(path, 0))
			_, err = store.ProbeContent(t.Context(), ref, 0)
			require.ErrorIs(t, err, ErrArtifactUnavailable)
			require.NoError(t, store.RepairContent(t.Context(), created.Entry.Identity, bytes.NewReader(body)))
			_, err = store.ProbeContent(t.Context(), ref, int64(len(body)))
			require.NoError(t, err)
		})
	}
}

func TestAvailabilityPreservesNonVerificationCloseErrors(t *testing.T) {
	transient := &fs.PathError{Op: "read", Path: "synthetic", Err: syscall.EIO}
	err := withoutIncompleteVerification(fmt.Errorf("close: %w", errors.Join(pack.ErrVerificationIncomplete, transient)))
	require.ErrorIs(t, err, syscall.EIO)
	require.NotErrorIs(t, err, pack.ErrVerificationIncomplete)
	require.Nil(t, withoutIncompleteVerification(pack.ErrVerificationIncomplete))
	for _, tc := range []struct {
		err         error
		unavailable bool
	}{
		{ErrArtifactInvalid, false}, {fmt.Errorf("%w: %w", ErrArtifactCorrupt, ErrArtifactInvalid), false},
		{docbank.ErrNotFound, true}, {packstore.ErrPhysicalMissing, true}, {packstore.ErrPhysicalCorrupt, true},
		{errors.Join(docbank.ErrContentUnavailable, transient), false},
		{errors.Join(pack.ErrCorrupt, transient), false},
		{errors.Join(docbank.ErrContentUnavailable, context.Canceled), false},
		{docbank.ErrContentUnavailable, true}, {errors.Join(docbank.ErrContentUnavailable, errors.New("unclassified read failure")), false},
	} {
		mapped := mapAvailabilityError(tc.err)
		require.Equal(t, tc.unavailable, errors.Is(mapped, ErrArtifactUnavailable), "%v", tc.err)
		require.ErrorIs(t, mapped, tc.err)
	}
}

// A readable replacement at an immutable reference is durable catalog damage,
// not a caller's invalid input. Both canonical and raw references use this boundary.
func TestDocbankAvailabilityRejectsReadableCatalogReplacement(t *testing.T) {
	for _, kind := range []Kind{KindManifests, KindRaw} {
		t.Run(string(kind), func(t *testing.T) {
			vault, store := newTestDocbankStore(t, docbank.Config{})
			original := []byte("original accepted content")
			identity := identityForBytes(t, original)
			name := identity.SHA256
			if kind == KindManifests {
				name += ".json"
			}
			ref := requireContractRef(t, contractOrigin, kind, name)
			createContractArtifact(t, store, ref, original)
			limit := int64(0)
			if kind == KindManifests {
				limit = identity.Size
			}
			_, err := store.ProbeContent(t.Context(), ref, limit)
			require.NoError(t, err)
			replacement := []byte("readable wrong content")
			_, err = vault.Put(t.Context(), docbankPath(ref), bytes.NewReader(replacement), docbank.PutOptions{MediaType: canonicalArtifactMediaType(kind)})
			require.NoError(t, err)
			content, err := vault.OpenContent(t.Context(), docbankPath(ref))
			require.NoError(t, err)
			body, readErr := io.ReadAll(content.Reader)
			closeErr := content.Reader.Close()
			require.NoError(t, readErr)
			require.NoError(t, closeErr)
			require.Equal(t, replacement, body) // The replacement itself is physically healthy.
			_, err = store.ProbeContent(t.Context(), ref, limit)
			require.ErrorIs(t, err, ErrArtifactUnavailable)
			require.ErrorIs(t, err, ErrArtifactCorrupt)
			// Independent failures must not be hidden by the positive mismatch tag.
			_, mismatch := docbankAvailabilityEntry(ref, content.Node)
			require.Error(t, mismatch)
			for _, independent := range []error{syscall.EIO, context.Canceled, context.DeadlineExceeded, errors.New("unknown close failure")} {
				closeErr := fmt.Errorf("close: %w", errors.Join(pack.ErrVerificationIncomplete, independent))
				mapped := mapAvailabilityError(withoutIncompleteVerification(errors.Join(mismatch, closeErr)))
				require.NotErrorIs(t, mapped, pack.ErrVerificationIncomplete)
				require.NotErrorIs(t, mapped, ErrArtifactUnavailable)
				require.ErrorIs(t, mapped, independent)
				require.ErrorIs(t, mapped, mismatch)
			}
		})
	}
}
