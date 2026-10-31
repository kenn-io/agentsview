package rawsync

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/artifact"
)

// ErrCustodyUnavailable means authorized content is durably missing or corrupt.
// Other errors are inconclusive and must abort the audit without a loss finding.
var ErrCustodyUnavailable = errors.New("custody content unavailable")

// CustodyProber is the in-process audit capability, separate from metadata Stat.
// Object probes do not read whole bodies; canonical manifests are verified within
// the accepted manifest's exact byte bound. No repository paths escape the adapter.
type CustodyProber interface {
	ProbeObject(context.Context, string, ObjectRef) error
	ProbeManifest(context.Context, AuthIdentity, string, int64) error
}

func (s *artifactObjectStore) ProbeObject(ctx context.Context, tenant string, object ObjectRef) error {
	ref, expected, err := rawArtifactCoordinates(tenant, object)
	if err != nil {
		return err
	}
	return s.probeContent(ctx, ref, expected, 0)
}

func (s *artifactObjectStore) ProbeManifest(ctx context.Context, id AuthIdentity, key string, size int64) error {
	canonical, err := NewAuthIdentity(id.TenantID, id.DeviceID)
	if err != nil || canonical != id || !isCanonicalSHA256(key) || size <= 0 || size > int64(DefaultManifestLimits().MaxCanonicalBytes) {
		return ErrInvalid
	}
	origin, err := tenantArtifactOrigin(id.TenantID)
	if err != nil {
		return err
	}
	ref, err := artifact.NewRef(origin, artifact.KindManifests, key+".json")
	if err != nil {
		return err
	}
	return s.probeContent(ctx, ref, artifact.Identity{SHA256: key, Size: size}, size)
}

func (s *artifactObjectStore) probeContent(ctx context.Context, ref artifact.Ref, expected artifact.Identity, limit int64) error {
	probe, ok := s.store.(artifact.ContentProber)
	if !ok {
		return fmt.Errorf("%w: artifact store lacks physical availability probe", ErrInvalid)
	}
	entry, err := probe.ProbeContent(ctx, ref, limit)
	if err != nil {
		if errors.Is(err, artifact.ErrArtifactUnavailable) {
			return fmt.Errorf("%w: %w", ErrCustodyUnavailable, err)
		}
		return err
	}
	if entry.Identity != expected {
		return fmt.Errorf("%w: catalog content identity differs", ErrCustodyUnavailable)
	}
	return nil
}
