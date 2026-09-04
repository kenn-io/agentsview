package rawderive

import (
	"context"
	"fmt"
	"io"

	"go.kenn.io/agentsview/internal/rawsync"
)

// ManifestStore provides authenticated access to canonical manifest objects.
type ManifestStore interface {
	OpenManifest(
		context.Context,
		rawsync.AuthIdentity,
		string,
	) (rawsync.ObjectInfo, rawsync.VerifiedObjectReader, error)
}

// ManifestLoader loads and verifies the authoritative custody envelope for a
// leased parse job.
type ManifestLoader struct {
	Store  ManifestStore
	Limits rawsync.ManifestLimits
}

// Load returns the exact canonical manifest named by lease.
func (l ManifestLoader) Load(
	ctx context.Context,
	lease JobLease,
) (_ rawsync.CanonicalManifest, resultErr error) {
	if l.Store == nil {
		return rawsync.CanonicalManifest{}, fmt.Errorf("%w: manifest store is required", rawsync.ErrInvalid)
	}
	if l.Limits.MaxCanonicalBytes <= 0 {
		return rawsync.CanonicalManifest{}, fmt.Errorf("%w: manifest limits are invalid", rawsync.ErrInvalid)
	}
	if _, err := rawsync.NewObjectRef(lease.ManifestID, 0); err != nil {
		return rawsync.CanonicalManifest{}, fmt.Errorf("loading raw manifest: %w", err)
	}
	info, reader, err := l.Store.OpenManifest(ctx, lease.Identity, lease.ManifestID)
	if err != nil {
		return rawsync.CanonicalManifest{}, fmt.Errorf("opening raw manifest: %w", err)
	}
	if reader == nil {
		return rawsync.CanonicalManifest{}, fmt.Errorf("opening raw manifest: %w: reader is missing", rawsync.ErrInvalid)
	}
	defer func() {
		if err := reader.Close(); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("closing raw manifest: %w", err)
		}
	}()
	if info.Ref.SHA256 != lease.ManifestID || info.Ref.Length <= 0 ||
		info.Ref.Length > int64(l.Limits.MaxCanonicalBytes) {
		return rawsync.CanonicalManifest{}, fmt.Errorf(
			"%w: stored manifest identity is inconsistent", rawsync.ErrInvalid,
		)
	}
	canonicalJSON, err := io.ReadAll(io.LimitReader(
		reader, int64(l.Limits.MaxCanonicalBytes)+1,
	))
	if err != nil {
		return rawsync.CanonicalManifest{}, fmt.Errorf("reading raw manifest: %w", err)
	}
	if len(canonicalJSON) > l.Limits.MaxCanonicalBytes ||
		int64(len(canonicalJSON)) != info.Ref.Length {
		return rawsync.CanonicalManifest{}, fmt.Errorf(
			"%w: stored manifest length is inconsistent", rawsync.ErrInvalid,
		)
	}
	if err := reader.Verify(); err != nil {
		return rawsync.CanonicalManifest{}, fmt.Errorf("verifying raw manifest: %w", err)
	}
	manifest, err := rawsync.ParseCanonicalManifest(
		lease.Identity, lease.ManifestID, canonicalJSON, l.Limits,
	)
	if err != nil {
		return rawsync.CanonicalManifest{}, fmt.Errorf("parsing raw manifest: %w", err)
	}
	return manifest, nil
}
