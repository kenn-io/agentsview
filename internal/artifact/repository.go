package artifact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"go.kenn.io/docbank"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const (
	repositoryDirectory              = "artifacts"
	repositoryLooseCompressionBytes  = int64(4 << 10)
	repositoryLooseCompressionSaving = 10
)

// Repository owns the process's local Docbank vault, opened on demand. Docbank
// owns root canonicalization, hierarchy locking, catalog validation, and vault
// reset.
type Repository struct {
	mu sync.Mutex

	content      *docbankStore
	root         *os.Root
	rootIdentity fs.FileInfo
	rootPath     string
	driver       docsqlite.Driver
	closed       bool
}

// OpenRepository opens the dedicated Docbank vault below dataDir.
func OpenRepository(ctx context.Context, dataDir string) (*Repository, error) {
	return openRepository(ctx, dataDir, nil)
}

func openRepository(
	ctx context.Context, dataDir string, driver docsqlite.Driver,
) (*Repository, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: repository context is required", ErrArtifactInvalid)
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("%w: repository data directory is required", ErrArtifactInvalid)
	}
	rootPath, err := canonicalRepositoryRoot(dataDir)
	if err != nil {
		return nil, err
	}
	if err := rejectLegacyRepositoryLayout(rootPath); err != nil {
		return nil, err
	}
	vault, err := docbank.New(ctx, repositoryDocbankConfig(rootPath, driver))
	if err != nil {
		return nil, fmt.Errorf("opening artifact repository: %w", err)
	}
	return repositoryFromVault(vault, rootPath, driver)
}

func repositoryDocbankConfig(rootPath string, driver docsqlite.Driver) docbank.Config {
	return docbank.Config{
		Root:   rootPath,
		SQLite: driver,
		LooseCompression: docbank.LooseCompressionOptions{
			Enabled:           true,
			MinBytes:          repositoryLooseCompressionBytes,
			MinSavingsPercent: repositoryLooseCompressionSaving,
		},
	}
}

func repositoryFromVault(
	vault *docbank.Vault, rootPath string, driver docsqlite.Driver,
) (_ *Repository, retErr error) {
	if vault == nil {
		return nil, errors.New("artifact repository requires an open Docbank vault")
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, vault.Close())
		}
	}()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("retaining artifact repository root: %w", err)
	}
	rootOpen := true
	defer func() {
		if rootOpen {
			retErr = errors.Join(retErr, root.Close())
		}
	}()
	identity, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("stating artifact repository root: %w", err)
	}
	current, err := os.Stat(rootPath)
	if err != nil || !os.SameFile(identity, current) {
		return nil, errors.New("artifact repository root changed while opening")
	}

	content := newDocbankContent(vault)
	repository := &Repository{
		content:      content,
		root:         root,
		rootIdentity: identity,
		rootPath:     rootPath,
		driver:       driver,
	}
	rootOpen = false
	return repository, nil
}

// rejectLegacyRepositoryLayout prevents an unreleased loose-artifact tree from
// being silently adopted as a Docbank vault. Existing Docbank roots are left to
// Docbank's catalog and layout validation.
func rejectLegacyRepositoryLayout(rootPath string) error {
	info, err := os.Lstat(rootPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("checking artifact repository layout: %w", err)
	case !info.IsDir():
		return fmt.Errorf(
			"artifact repository path is not a Docbank vault; move or remove %s and retry",
			rootPath,
		)
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return fmt.Errorf("checking artifact repository layout: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	for _, entry := range entries {
		if entry.Name() == "docbank.db" || entry.Name() == "vault.lock" {
			return nil
		}
	}
	return fmt.Errorf(
		"artifact repository contains the old loose artifact layout; move or remove %s and retry",
		rootPath,
	)
}

// Content returns the logical artifact boundary owned by this repository.
func (r *Repository) Content() ArtifactStore {
	if r == nil {
		return nil
	}
	return r.content
}

// Closed reports whether repository ownership has already been released or
// transferred by an explicit reset.
func (r *Repository) Closed() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// Close waits for active verified readers through Docbank, then releases the
// retained root identity. It is safe to call more than once.
func (r *Repository) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return errors.Join(r.content.Close(), r.root.Close())
}

func canonicalRepositoryRoot(dataDir string) (string, error) {
	dataAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolving artifact repository root: %w", err)
	}
	rootPath, err := canonicalArtifactPath(filepath.Join(dataAbs, repositoryDirectory))
	if err != nil {
		return "", fmt.Errorf("resolving artifact repository root symlinks: %w", err)
	}
	return filepath.Clean(rootPath), nil
}

// canonicalArtifactPath resolves path's symlinks even when its final
// components do not yet exist, so a repository root under construction can be
// canonicalized before Docbank creates it.
func canonicalArtifactPath(path string) (string, error) {
	missing := make([]string, 0, 2)
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for _, part := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, part)
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
