//go:build pgtest

package postgres

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank"
	"go.kenn.io/kit/packstore"
)

type custodyProbeFixture struct {
	rawsync.ObjectStore
	objectError, manifestError   error
	objectChecks, manifestChecks int
	restored                     bool
	cancel                       context.CancelFunc
}

func (s *custodyProbeFixture) ProbeObject(ctx context.Context, tenant string, ref rawsync.ObjectRef) error {
	s.objectChecks++
	if s.cancel != nil {
		s.cancel()
		return ctx.Err()
	}
	if s.objectError != nil && (!s.restored || s.objectChecks == 1) {
		return s.objectError
	}
	return s.ObjectStore.(rawsync.CustodyProber).ProbeObject(ctx, tenant, ref)
}
func (s *custodyProbeFixture) ProbeManifest(ctx context.Context, id rawsync.AuthIdentity, key string, size int64) error {
	s.manifestChecks++
	if s.manifestError != nil {
		return s.manifestError
	}
	return s.ObjectStore.(rawsync.CustodyProber).ProbeManifest(ctx, id, key, size)
}

func auditCustodySweep(t *testing.T, f projectionFixture, objects rawsync.ObjectStore) {
	t.Helper()
	for {
		result, err := f.sink.AuditRawCustody(t.Context(), objects)
		require.NoError(t, err)
		require.LessOrEqual(t, result.Scanned, rawCustodyManifestBatchSize)
		if result.Wrapped {
			return
		}
	}
}

// Real repository reads protect healthy nonempty manifests, historical captures
// below tombstones, and physical loss with surviving logical metadata.
func TestRawCustodyAuditPhysicalLossAndRestore(t *testing.T) {
	for _, kind := range []string{"healthy", "object", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			f := newProjectionFixture(t)
			root := t.TempDir()
			repo, err := artifact.OpenRepository(t.Context(), root)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, repo.Close()) })
			f.objects, err = rawsync.NewArtifactObjectStore(repo.Content())
			require.NoError(t, err)
			f.custody, err = rawsync.NewService(f.objects, f.jobs, rawsync.DefaultManifestLimits(), "parser-1")
			require.NoError(t, err)
			m, receipt := f.accept(t, "device", "audit", "")
			f.tombstone(t, m, "departed", receipt.Receipt)
			before := rawAuthoritativeSnapshot(t, f)
			var path string
			var body []byte
			if kind != "healthy" {
				key := m.ManifestID
				body = m.CanonicalJSON
				if kind == "object" {
					key = m.Objects[0].SHA256
					body = []byte("synthetic custody audit")
				}
				layout, err := packstore.NewLayout(filepath.Join(root, "artifacts", "blobs"), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
				require.NoError(t, err)
				hash, err := packstore.ParseHash(key)
				require.NoError(t, err)
				path = layout.LoosePath(hash)
				require.NoError(t, os.Remove(path)) // Test-only damage, never a production path probe.
				// StatObject still succeeds for lost raw bytes: the catalog survives.
				if kind == "object" {
					_, err = f.objects.StatObject(t.Context(), f.tenant, m.Objects[0])
					require.NoError(t, err)
				}
			}
			auditCustodySweep(t, f, f.objects)
			require.Equal(t, before, rawAuthoritativeSnapshot(t, f))
			var count int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_custody_findings`).Scan(&count))
			if kind == "healthy" {
				require.Zero(t, count)
				return
			}
			require.Equal(t, 1, count)
			var savedKind string
			var savedLastSeen time.Time
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT kind,last_seen FROM raw_custody_findings WHERE manifest_id=$1`, m.ManifestID).Scan(&savedKind, &savedLastSeen))
			require.Equal(t, kind, savedKind)
			// Restore after a finding has committed. Findings remain historical.
			require.NoError(t, os.WriteFile(path, body, 0600))
			probe := f.objects.(rawsync.CustodyProber)
			require.NoError(t, probe.ProbeManifest(t.Context(), m.Identity, m.ManifestID, int64(len(m.CanonicalJSON))))
			require.NoError(t, probe.ProbeObject(t.Context(), f.tenant, m.Objects[0]))
			auditCustodySweep(t, f, f.objects)
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_custody_findings`).Scan(&count))
			require.Equal(t, 1, count)
			var lastSeen time.Time
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT last_seen FROM raw_custody_findings WHERE manifest_id=$1`, m.ManifestID).Scan(&lastSeen))
			require.Equal(t, savedLastSeen, lastSeen)
			require.Equal(t, before, rawAuthoritativeSnapshot(t, f))
		})
	}
}

func TestRawCustodyAuditRechecksAndRollsBack(t *testing.T) {
	for _, mode := range []string{"missing", "restored-between-probes", "transient", "cancel-after-finding"} {
		t.Run(mode, func(t *testing.T) {
			f := newProjectionFixture(t)
			f.accept(t, "device", "audit", "")
			objects := &custodyProbeFixture{ObjectStore: f.objects, objectError: rawsync.ErrCustodyUnavailable}
			ctx := t.Context()
			switch mode {
			case "restored-between-probes":
				objects.restored = true
			case "transient":
				objects.objectError = errors.New("transient read failure")
			case "cancel-after-finding":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				objects.manifestError = rawsync.ErrCustodyUnavailable
				objects.cancel = cancel // Object probe follows the already-inserted manifest finding.
			}
			before := rawMaintenanceSnapshot(t, f)
			_, err := f.sink.AuditRawCustody(ctx, objects)
			if mode == "transient" || mode == "cancel-after-finding" {
				require.Error(t, err)
				if objects.cancel != nil {
					require.ErrorIs(t, err, context.Canceled)
					require.Equal(t, 2, objects.manifestChecks)
					require.Equal(t, 1, objects.objectChecks)
				}
				require.Equal(t, before, rawMaintenanceSnapshot(t, f))
			} else {
				require.NoError(t, err)
				require.Equal(t, 2, objects.objectChecks)
				var count int
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_custody_findings`).Scan(&count))
				require.Equal(t, map[bool]int{true: 0, false: 1}[objects.restored], count)
			}
		})
	}
}

// Wrong but readable canonical/raw bytes must become a rechecked custody finding,
// then let the cursor reach the following healthy manifest without changing jobs.
func TestRawCustodyAuditReadableCatalogReplacement(t *testing.T) {
	for _, kind := range []string{"manifest", "object"} {
		t.Run(kind, func(t *testing.T) {
			f := newProjectionFixture(t)
			root := t.TempDir()
			repo, err := artifact.OpenRepository(t.Context(), root)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, repo.Close()) })
			f.objects, err = rawsync.NewArtifactObjectStore(repo.Content())
			require.NoError(t, err)
			f.custody, err = rawsync.NewService(f.objects, f.jobs, rawsync.DefaultManifestLimits(), "parser-1")
			require.NoError(t, err)
			first, receipt := f.accept(t, "device", "catalog-damage", "")
			second, _ := f.accept(t, "device", "following", receipt.Receipt)
			if second.ManifestID < first.ManifestID {
				first, second = second, first
			}
			key := first.ManifestID
			if kind == "object" {
				key = first.Objects[0].SHA256
			}
			before := rawAuthoritativeSnapshot(t, f)
			require.NoError(t, repo.Close())
			replaceReadableCustodyContent(t, root, key)
			reopened, err := artifact.OpenRepository(t.Context(), root)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			f.objects, err = rawsync.NewArtifactObjectStore(reopened.Content())
			require.NoError(t, err)
			probe := f.objects.(rawsync.CustodyProber)
			manifestErr := probe.ProbeManifest(t.Context(), first.Identity, first.ManifestID, int64(len(first.CanonicalJSON)))
			objectErr := probe.ProbeObject(t.Context(), f.tenant, first.Objects[0])
			if kind == "manifest" {
				require.ErrorIs(t, manifestErr, rawsync.ErrCustodyUnavailable)
				require.ErrorIs(t, manifestErr, artifact.ErrArtifactUnavailable)
				require.NoError(t, objectErr)
			} else {
				require.ErrorIs(t, objectErr, rawsync.ErrCustodyUnavailable)
				require.ErrorIs(t, objectErr, artifact.ErrArtifactUnavailable)
				require.NoError(t, manifestErr)
			}
			for i, m := range []rawsync.CanonicalManifest{first, second} {
				result, err := f.sink.AuditRawCustody(t.Context(), f.objects)
				require.NoError(t, err)
				require.Equal(t, 1, result.Scanned)
				require.Equal(t, 1-i, result.Found)
				require.False(t, result.Wrapped)
				var encoded []byte
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT cursor FROM raw_maintenance_state WHERE check_name='custody'`).Scan(&encoded))
				var cursor []string
				require.NoError(t, json.Unmarshal(encoded, &cursor))
				require.Equal(t, []string{m.ManifestID, "", "", ""}, cursor)
			}
			result, err := f.sink.AuditRawCustody(t.Context(), f.objects)
			require.NoError(t, err)
			require.True(t, result.Wrapped)
			require.Zero(t, result.Scanned)
			var count int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_custody_findings WHERE manifest_id=$1 AND kind=$2 AND key=$3`, first.ManifestID, kind, key).Scan(&count))
			require.Equal(t, 1, count)
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_custody_findings`).Scan(&count))
			require.Equal(t, 1, count)
			require.Equal(t, before, rawAuthoritativeSnapshot(t, f))
		})
	}
}

// Public Docbank APIs model an incorrect restore at an existing virtual path.
// Walk discovers that path without relying on physical storage or catalog SQL.
func replaceReadableCustodyContent(t *testing.T, root, hash string) {
	t.Helper()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(root, "artifacts")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	walker, err := vault.Walk(t.Context(), "/v1", docbank.WalkOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })
	var paths []string
	for {
		page, err := walker.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		for _, entry := range page {
			if entry.Node.BlobHash == hash {
				paths = append(paths, entry.Path)
			}
		}
	}
	require.NoError(t, walker.Close())
	require.Len(t, paths, 1)
	replacement := []byte("readable wrong custody content")
	_, err = vault.Put(t.Context(), paths[0], bytes.NewReader(replacement), docbank.PutOptions{})
	require.NoError(t, err)
	content, err := vault.OpenContent(t.Context(), paths[0])
	require.NoError(t, err)
	body, readErr := io.ReadAll(content.Reader)
	closeErr := content.Reader.Close()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	require.Equal(t, replacement, body)
	require.NoError(t, vault.Close())
}
