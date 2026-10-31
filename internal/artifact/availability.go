package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"syscall"

	"go.kenn.io/docbank"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

// ErrArtifactUnavailable identifies durable missing or corrupt authorized
// content. Cancellation, unclassified errors and transient I/O retain their cause.
var ErrArtifactUnavailable = errors.New("artifact content unavailable")

// ContentProber checks catalog-authorized physical availability. A zero limit
// opens headers and closes without decoding the body or claiming its integrity.
// A positive limit requires terminal verification of content no larger than it.
// This optional interface leaves ordinary ArtifactStore stat/read semantics intact.
type ContentProber interface {
	ProbeContent(context.Context, Ref, int64) (Entry, error)
}

func (s *docbankStore) ProbeContent(ctx context.Context, ref Ref, verifyLimit int64) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if err := validateStoreRef(ref); err != nil {
		return Entry{}, err
	}
	if verifyLimit < 0 || verifyLimit == math.MaxInt64 {
		return Entry{}, ErrArtifactInvalid
	}
	content, err := s.vault.OpenContent(ctx, docbankPath(ref))
	if err != nil {
		return Entry{}, artifactStoreError("probe", ref, mapAvailabilityError(withoutIncompleteVerification(err)))
	}
	entry, entryErr := docbankAvailabilityEntry(ref, content.Node)
	var readErr error
	if entryErr == nil && verifyLimit > 0 {
		if entry.Identity.Size > verifyLimit {
			entryErr = ErrArtifactInvalid
		} else {
			// One extra byte reaches verified EOF even at the limit and detects excess.
			var n int64
			n, readErr = io.Copy(io.Discard, io.LimitReader(content.Reader, verifyLimit+1))
			if readErr == nil && n != entry.Identity.Size {
				readErr = packstore.ErrContentMismatch
			}
		}
	}
	closeErr := content.Reader.Close() // Sequential with Read; never drains implicitly.
	if verifyLimit == 0 || entryErr != nil || readErr != nil {
		closeErr = withoutIncompleteVerification(closeErr)
	}
	err = errors.Join(entryErr, readErr, closeErr, ctx.Err())
	if err != nil {
		return Entry{}, artifactStoreError("probe", ref, mapAvailabilityError(err))
	}
	return entry, nil
}

// A positive catalog/reference mismatch owns only its integrity diagnostic.
// Independent reader failures are joined separately by ProbeContent.
type catalogReferenceMismatchError struct {
	expected, actual string
}

func (e *catalogReferenceMismatchError) Error() string {
	return fmt.Sprintf("reference hash %s differs from catalog hash %s", e.expected, e.actual)
}

func (e *catalogReferenceMismatchError) Unwrap() []error {
	return []error{ErrArtifactCorrupt, ErrArtifactInvalid}
}

func docbankAvailabilityEntry(ref Ref, node docbank.Node) (Entry, error) {
	identity, identityErr := NewIdentity(node.BlobHash, node.Size)
	expected, refErr := refIdentitySHA(ref)
	if identityErr == nil && refErr == nil && expected != "" && expected != identity.SHA256 {
		return Entry{}, &catalogReferenceMismatchError{expected: expected, actual: identity.SHA256}
	}
	return docbankEntry(ref, node)
}

// Remove only the incomplete-verification sentinel, including from joined
// errors. Every independent Close failure survives; ordinary readers do not use
// this availability-only exception to the verified read contract.
func withoutIncompleteVerification(err error) error {
	if err == pack.ErrVerificationIncomplete {
		return nil
	}
	if !errors.Is(err, pack.ErrVerificationIncomplete) {
		return err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var kept []error
		for _, cause := range joined.Unwrap() {
			kept = append(kept, withoutIncompleteVerification(cause))
		}
		return errors.Join(kept...)
	}
	return withoutIncompleteVerification(errors.Unwrap(err))
}

// Only an error tree containing entirely classified durable causes becomes a
// finding. A joined unknown or transient error keeps the whole result inconclusive.
func mapAvailabilityError(err error) error {
	if err == nil {
		return nil
	}
	if durableAvailabilityError(err) {
		return fmt.Errorf("%w: %w", ErrArtifactUnavailable, err)
	}
	return err
}

func durableAvailabilityError(err error) bool {
	if _, ok := err.(*catalogReferenceMismatchError); ok {
		return true
	}
	switch err {
	case docbank.ErrNotFound, fs.ErrNotExist, syscall.ENOENT,
		packstore.ErrPhysicalMissing, packstore.ErrPhysicalAuthorityMissing,
		packstore.ErrPhysicalCorrupt, packstore.ErrContentMismatch,
		pack.ErrCorrupt, pack.ErrTruncated, pack.ErrChecksum, pack.ErrBadMagic,
		pack.ErrBlobMismatch, pack.ErrUnsupportedVersion, io.EOF, io.ErrUnexpectedEOF:
		return true
	case docbank.ErrContentUnavailable:
		// OpenContent wraps its underlying open error. This sentinel as its sole
		// cause denotes a catalog/physical size mismatch; unknown inner causes below
		// remain inconclusive. No matching of dependency error text is needed.
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !durableAvailabilityError(cause) {
				return false
			}
		}
		return true
	}
	cause := errors.Unwrap(err)
	return cause != nil && durableAvailabilityError(cause)
}
