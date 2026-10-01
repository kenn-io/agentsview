// Package rawarchive retains immutable provider captures for offline recovery
// and explicit local reparsing. The caller owns the archive's writer lock.
package rawarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	Directory = "raw-archive"
	pageSize  = 128
)

// Archive shares the caller's database and exclusively owns its raw vault.
type Archive struct {
	database   *db.DB
	repository *artifact.Repository
	objects    rawsync.ObjectStore
	tenant     string
	progress   func(string)
}

func Open(ctx context.Context, database *db.DB, dataDir string, progress func(string)) (*Archive, error) {
	if database == nil {
		return nil, errors.New("archive database is required")
	}
	tenant, err := database.GetOrCreateArchiveID(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := artifact.OpenRepository(ctx, filepath.Join(dataDir, Directory))
	if err != nil {
		return nil, err
	}
	objects, err := rawsync.NewArtifactObjectStore(repo.Content())
	if err != nil {
		return nil, errors.Join(err, repo.Close())
	}
	return &Archive{database: database, repository: repo, objects: objects, tenant: tenant, progress: progress}, nil
}

func (a *Archive) Close() error { return a.repository.Close() }
func (a *Archive) report(message string) {
	if a.progress != nil {
		a.progress(message)
	}
}

// Report distinguishes custody from provider and parser coverage.
type Report struct {
	Roots        int      `json:"roots"`
	Files        int      `json:"files"`
	Bytes        int64    `json:"bytes"`
	Sources      int      `json:"sources"`
	Parsed       int      `json:"parsed"`
	Supplemental int      `json:"supplemental"`
	Gaps         []string `json:"gaps,omitempty"`
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func hashReader(ctx context.Context, r io.Reader) (rawsync.ObjectRef, error) {
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, r})
	if err != nil {
		return rawsync.ObjectRef{}, err
	}
	return rawsync.NewObjectRef(hex.EncodeToString(h.Sum(nil)), n)
}

func containedPath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", errors.New("path is outside capture root")
	}
	return rel, nil
}

func (a *Archive) roots(ctx context.Context) (map[string]db.RawArchiveRoot, error) {
	roots, err := a.database.ListRawArchiveRoots(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]db.RawArchiveRoot, len(roots))
	for _, root := range roots {
		out[root.ID] = root
	}
	return out, nil
}

func (a *Archive) canonical(ctx context.Context, source db.RawArchiveSource, roots map[string]db.RawArchiveRoot) (rawsync.CanonicalManifest, error) {
	root, ok := roots[source.RootID]
	if !ok {
		return rawsync.CanonicalManifest{}, errors.New("accepted source has no root binding")
	}
	identity, err := rawsync.NewAuthIdentity(a.tenant, root.DeviceID)
	if err != nil {
		return rawsync.CanonicalManifest{}, err
	}
	manifest, err := rawsync.ParseCanonicalManifest(identity, source.ManifestID, source.CanonicalJSON, rawsync.DefaultManifestLimits())
	if err != nil {
		return rawsync.CanonicalManifest{}, err
	}
	if manifest.Manifest.ConfiguredRootID != root.ID || string(manifest.Manifest.Provider) != root.Provider || manifest.Manifest.SourceKey != source.SourceKey {
		return rawsync.CanonicalManifest{}, errors.New("accepted source differs from its root binding")
	}
	return manifest, nil
}

func (a *Archive) sourcePages(ctx context.Context, visit func(db.RawArchiveSource) error) error {
	after := ""
	for {
		page, err := a.database.ListRawArchiveSources(ctx, after, pageSize)
		if err != nil {
			return err
		}
		for _, source := range page {
			if err := visit(source); err != nil {
				return err
			}
			after = source.ManifestID
		}
		if len(page) < pageSize {
			return nil
		}
	}
}
