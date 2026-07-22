package artifact

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/mattn"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

func TestOpenRepositoryOwnsOneCompressedDocbankStore(t *testing.T) {
	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	assert.Same(t, repository.Store(), repository.Store())
	body := bytes.Repeat([]byte("repository compression policy\n"), 200)
	body = body[:4<<10]
	result := createCheckpointBody(t, repository.Store(), 1, body)
	assert.Equal(t, "zstd", result.Physical.Encoding)
	assert.DirExists(t, filepath.Join(dataDir, "artifacts"))
	assert.FileExists(t, filepath.Join(dataDir, "artifacts", "docbank.db"))
}

func TestOpenRepositoryStartsWithBothDocbankSQLiteDrivers(t *testing.T) {
	for _, driver := range []docsqlite.Driver{mattn.Driver{}, modernc.Driver{}} {
		t.Run(driver.Name(), func(t *testing.T) {
			repository, err := openRepository(t.Context(), t.TempDir(), driver)
			require.NoError(t, err)
			result := createCheckpointBody(t, repository.Store(), 1, []byte("driver parity"))
			assert.True(t, result.Created)
			require.NoError(t, repository.Close())
		})
	}
}

func TestOpenRepositoryRejectsOldLooseLayoutWithoutChangingIt(t *testing.T) {
	dataDir := t.TempDir()
	artifactDir := filepath.Join(dataDir, "artifacts")
	legacy := filepath.Join(artifactDir, contractOrigin, string(KindCheckpoints), "cp-0000000001.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
	original := []byte("disposable old loose artifact")
	require.NoError(t, os.WriteFile(legacy, original, 0o644))

	repository, err := OpenRepository(t.Context(), dataDir)
	assert.Nil(t, repository)
	assert.ErrorContains(t, err, "old loose artifact layout")
	assert.ErrorContains(t, err, "move or remove")
	assert.FileExists(t, legacy)
	got, readErr := os.ReadFile(legacy)
	require.NoError(t, readErr)
	assert.Equal(t, original, got)
	assert.NoFileExists(t, filepath.Join(artifactDir, "docbank.db"))
}

func TestOpenRepositoryInitializesEmptyDirectoryAndReopensOwnedVault(t *testing.T) {
	for _, driver := range []docsqlite.Driver{mattn.Driver{}, modernc.Driver{}} {
		t.Run(driver.Name(), func(t *testing.T) {
			dataDir := t.TempDir()
			artifactDir := filepath.Join(dataDir, "artifacts")
			require.NoError(t, os.Mkdir(artifactDir, 0o755))

			repository, err := openRepository(t.Context(), dataDir, driver)
			require.NoError(t, err)
			require.NoError(t, repository.Close())
			assert.Equal(t, []byte(repositoryOwnershipMarkerContent),
				mustReadFile(t, filepath.Join(artifactDir, repositoryOwnershipMarker)))

			reopened, err := openRepository(t.Context(), dataDir, driver)
			require.NoError(t, err)
			require.NoError(t, reopened.Close())
		})
	}
}

func TestOpenRepositoryRejectsUnownedPublicDocbankVaultWithoutMutation(t *testing.T) {
	for _, driver := range []docsqlite.Driver{mattn.Driver{}, modernc.Driver{}} {
		t.Run(driver.Name(), func(t *testing.T) {
			dataDir := t.TempDir()
			artifactDir := filepath.Join(dataDir, "artifacts")
			createPublicDocbankVault(t, artifactDir, driver)
			before := snapshotDirectory(t, artifactDir)

			repository, err := openRepository(t.Context(), dataDir, driver)
			assert.Nil(t, repository)
			assert.ErrorContains(t, err, "not owned by AgentsView")
			assert.Equal(t, before, snapshotDirectory(t, artifactDir))
		})
	}
}

func TestOpenRepositoryRejectsUnverifiedDatabaseMarkersWithoutMutation(t *testing.T) {
	for _, driver := range []docsqlite.Driver{mattn.Driver{}, modernc.Driver{}} {
		t.Run(driver.Name(), func(t *testing.T) {
			t.Run("empty regular marker", func(t *testing.T) {
				dataDir := t.TempDir()
				artifactDir := filepath.Join(dataDir, "artifacts")
				require.NoError(t, os.Mkdir(artifactDir, 0o755))
				marker := filepath.Join(artifactDir, "docbank.db")
				require.NoError(t, os.WriteFile(marker, nil, 0o600))

				repository, err := openRepository(t.Context(), dataDir, driver)
				assert.Nil(t, repository)
				assert.ErrorContains(t, err, "not owned by AgentsView")
				assert.Equal(t, []byte{}, mustReadFile(t, marker))
				assert.Equal(t, []string{"docbank.db"}, directoryEntryNames(t, artifactDir))
			})

			t.Run("unrelated sqlite database", func(t *testing.T) {
				dataDir := t.TempDir()
				artifactDir := filepath.Join(dataDir, "artifacts")
				require.NoError(t, os.Mkdir(artifactDir, 0o755))
				marker := filepath.Join(artifactDir, "docbank.db")
				db, err := driver.Open(marker, docsqlite.OpenOptions{
					Access: docsqlite.Create, TransactionMode: docsqlite.Immediate,
				})
				require.NoError(t, err)
				_, err = db.Exec(`CREATE TABLE unrelated (value TEXT NOT NULL)`)
				require.NoError(t, err)
				require.NoError(t, db.Close())
				before := mustReadFile(t, marker)

				repository, err := openRepository(t.Context(), dataDir, driver)
				assert.Nil(t, repository)
				assert.ErrorContains(t, err, "not owned by AgentsView")
				assert.Equal(t, before, mustReadFile(t, marker))
				assert.Equal(t, []string{"docbank.db"}, directoryEntryNames(t, artifactDir))
			})
		})
	}
}

func TestOpenRepositoryRejectsInvalidOwnershipMarkersWithoutMutation(t *testing.T) {
	for name, createMarker := range map[string]func(*testing.T, string) string{
		"empty marker": func(t *testing.T, marker string) string {
			require.NoError(t, os.WriteFile(marker, nil, 0o600))
			return ""
		},
		"wrong marker contents": func(t *testing.T, marker string) string {
			wrong := bytes.Repeat([]byte("x"), len(repositoryOwnershipMarkerContent))
			require.NoError(t, os.WriteFile(marker, wrong, 0o600))
			return ""
		},
		"symlink marker": func(t *testing.T, marker string) string {
			target := filepath.Join(t.TempDir(), "marker")
			require.NoError(t, os.WriteFile(target, []byte(repositoryOwnershipMarkerContent), 0o600))
			require.NoError(t, os.Symlink(target, marker))
			return target
		},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			artifactDir := filepath.Join(dataDir, "artifacts")
			require.NoError(t, os.Mkdir(artifactDir, 0o755))
			marker := filepath.Join(artifactDir, repositoryOwnershipMarker)
			externalTarget := createMarker(t, marker)
			before := snapshotDirectory(t, artifactDir)
			var externalBefore []byte
			if externalTarget != "" {
				externalBefore = mustReadFile(t, externalTarget)
			}

			repository, err := OpenRepository(t.Context(), dataDir)
			assert.Nil(t, repository)
			assert.ErrorContains(t, err, "invalid AgentsView ownership marker")
			assert.Equal(t, before, snapshotDirectory(t, artifactDir))
			if externalTarget != "" {
				assert.Equal(t, externalBefore, mustReadFile(t, externalTarget))
			}
		})
	}
}

func TestOpenRepositoryRejectsSymlinkedDatabaseMarkerWithoutMutation(t *testing.T) {
	dataDir := t.TempDir()
	artifactDir := filepath.Join(dataDir, "artifacts")
	require.NoError(t, os.Mkdir(artifactDir, 0o755))
	target := filepath.Join(t.TempDir(), "external.db")
	original := []byte("external database bytes")
	require.NoError(t, os.WriteFile(target, original, 0o600))
	marker := filepath.Join(artifactDir, "docbank.db")
	require.NoError(t, os.Symlink(target, marker))

	repository, err := OpenRepository(t.Context(), dataDir)
	assert.Nil(t, repository)
	assert.ErrorContains(t, err, "not owned by AgentsView")
	info, statErr := os.Lstat(marker)
	require.NoError(t, statErr)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	assert.Equal(t, original, mustReadFile(t, target))
}

func TestOpenRepositoryRejectsMixedLegacyAndDocbankLayoutWithoutMutation(t *testing.T) {
	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	require.NoError(t, repository.Close())
	artifactDir := filepath.Join(dataDir, "artifacts")
	marker := filepath.Join(artifactDir, "docbank.db")
	markerBefore := mustReadFile(t, marker)
	legacy := filepath.Join(artifactDir, contractOrigin, string(KindCheckpoints), "cp-0000000001.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
	original := []byte("old loose artifact")
	require.NoError(t, os.WriteFile(legacy, original, 0o644))
	before := snapshotDirectory(t, artifactDir)

	reopened, err := OpenRepository(t.Context(), dataDir)
	assert.Nil(t, reopened)
	assert.ErrorContains(t, err, "old loose artifact layout")
	assert.Equal(t, markerBefore, mustReadFile(t, marker))
	assert.Equal(t, original, mustReadFile(t, legacy))
	assert.Equal(t, before, snapshotDirectory(t, artifactDir))
}

func TestOpenRepositoryFollowsFinalRootSymlinkToValidVault(t *testing.T) {
	for _, driver := range []docsqlite.Driver{mattn.Driver{}, modernc.Driver{}} {
		t.Run(driver.Name(), func(t *testing.T) {
			realDataDir := t.TempDir()
			realRoot := filepath.Join(realDataDir, "artifacts")
			realRepository, err := openRepository(t.Context(), realDataDir, driver)
			require.NoError(t, err)
			require.NoError(t, realRepository.Close())

			aliasDataDir := t.TempDir()
			require.NoError(t, os.Symlink(realRoot, filepath.Join(aliasDataDir, "artifacts")))

			aliased, err := openRepository(t.Context(), aliasDataDir, driver)
			require.NoError(t, err)
			canonicalRoot, err := filepath.EvalSymlinks(realRoot)
			require.NoError(t, err)
			assert.Equal(t, canonicalRoot, aliased.rootPath)
			require.NoError(t, aliased.Close())
		})
	}
}

func TestOpenRepositoryRecoversAfterMarkerPublicationBeforeDocbankInitialization(t *testing.T) {
	interrupted := errors.New("interrupted after ownership publication")
	dataDir := t.TempDir()
	repository, err := openRepositoryWith(
		t.Context(), dataDir, modernc.Driver{}, repositoryOpenHooks{
			beforeVaultOpen: func(*os.Root, string) error { return interrupted },
		},
	)
	assert.Nil(t, repository)
	assert.ErrorIs(t, err, interrupted)
	artifactDir := filepath.Join(dataDir, "artifacts")
	assert.Equal(t, []byte(repositoryOwnershipMarkerContent),
		mustReadFile(t, filepath.Join(artifactDir, repositoryOwnershipMarker)))
	assert.NoFileExists(t, filepath.Join(artifactDir, "docbank.db"))

	reopened, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

func TestOpenRepositoryRecoversAtomicMarkerPublicationStates(t *testing.T) {
	for name, publishFinal := range map[string]bool{
		"pending marker before publication": false,
		"published marker before cleanup":   true,
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			artifactDir := filepath.Join(dataDir, "artifacts")
			require.NoError(t, os.Mkdir(artifactDir, 0o700))
			pending := filepath.Join(artifactDir, repositoryOwnershipMarkerPending)
			require.NoError(t, os.WriteFile(
				pending, []byte(repositoryOwnershipMarkerContent), 0o600,
			))
			if publishFinal {
				require.NoError(t, os.WriteFile(
					filepath.Join(artifactDir, repositoryOwnershipMarker),
					[]byte(repositoryOwnershipMarkerContent), 0o600,
				))
			}

			repository, err := openRepository(t.Context(), dataDir, modernc.Driver{})
			require.NoError(t, err)
			require.NoError(t, repository.Close())
			assert.NoFileExists(t, pending)
			assert.Equal(t, []byte(repositoryOwnershipMarkerContent),
				mustReadFile(t, filepath.Join(artifactDir, repositoryOwnershipMarker)))
		})
	}
}

func TestOpenRepositoryConcurrentFirstInitializationReturnsOwnershipLock(t *testing.T) {
	dataDir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	type openResult struct {
		repository *Repository
		err        error
	}
	firstResult := make(chan openResult, 1)
	go func() {
		repository, err := openRepositoryWith(
			t.Context(), dataDir, modernc.Driver{}, repositoryOpenHooks{
				beforeVaultOpen: func(*os.Root, string) error {
					close(entered)
					<-release
					return nil
				},
			},
		)
		firstResult <- openResult{repository: repository, err: err}
	}()
	<-entered

	second, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	assert.Nil(t, second)
	assert.ErrorContains(t, err, "ownership lock")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "layout")

	close(release)
	first := <-firstResult
	require.NoError(t, first.err)
	require.NotNil(t, first.repository)
	require.NoError(t, first.repository.Close())
}

func TestOpenRepositoryRejectsRootReplacementBeforeDocbankOpenWithoutMutation(t *testing.T) {
	dataDir := t.TempDir()
	initial, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	require.NoError(t, initial.Close())
	rootPath := filepath.Join(dataDir, "artifacts")
	movedPath := filepath.Join(dataDir, "recognized-artifacts")
	replacementBody := []byte("unrelated replacement")
	replaced := false
	t.Cleanup(func() {
		if replaced {
			require.NoError(t, os.RemoveAll(rootPath))
			require.NoError(t, os.Rename(movedPath, rootPath))
		}
	})

	repository, err := openRepositoryWith(
		t.Context(), dataDir, modernc.Driver{}, repositoryOpenHooks{
			beforeVaultOpen: func(*os.Root, string) error {
				require.NoError(t, os.Rename(rootPath, movedPath))
				require.NoError(t, os.Mkdir(rootPath, 0o755))
				require.NoError(t, os.WriteFile(
					filepath.Join(rootPath, "sentinel"), replacementBody, 0o600,
				))
				replaced = true
				return nil
			},
		},
	)
	assert.Nil(t, repository)
	assert.ErrorContains(t, err, "root changed while opening")
	assert.Equal(t, replacementBody, mustReadFile(t, filepath.Join(rootPath, "sentinel")))
	assert.Equal(t, []string{"sentinel"}, directoryEntryNames(t, rootPath))
}

func TestOpenRepositoryPreservesDocbankHierarchyLockErrors(t *testing.T) {
	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	createCheckpointBody(t, repository.Store(), 1, []byte("uncheckpointed WAL content"))
	assert.FileExists(t, filepath.Join(dataDir, "artifacts", "docbank.db-wal"))

	second, err := OpenRepository(t.Context(), dataDir)
	assert.Nil(t, second)
	assert.ErrorContains(t, err, "vault is locked")

	overlapping, err := OpenRepository(t.Context(), filepath.Join(dataDir, "artifacts"))
	assert.Nil(t, overlapping)
	assert.ErrorContains(t, err, "vault is locked")
}

func TestOpenRepositoryRejectsLiveDescendantWithOwnershipLockClassification(t *testing.T) {
	parentDataDir := t.TempDir()
	childDataDir := filepath.Join(parentDataDir, "artifacts")
	child, err := openRepository(t.Context(), childDataDir, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, child.Close()) })

	parent, err := openRepository(t.Context(), parentDataDir, modernc.Driver{})
	if parent != nil {
		t.Cleanup(func() { require.NoError(t, parent.Close()) })
	}
	assert.Nil(t, parent)
	assert.ErrorContains(t, err, "ownership lock")
	assert.ErrorContains(t, err, "vault is locked")
	if assert.Error(t, err) {
		assert.NotContains(t, err.Error(), "old loose artifact layout")
	}

	require.NoError(t, child.Close())
	reopened, err := openRepository(t.Context(), childDataDir, modernc.Driver{})
	require.NoError(t, err, "closing a repository must release its live-root reservation")
	require.NoError(t, reopened.Close())
}

func TestOpenRepositoryConcurrentInverseOverlapReturnsOwnershipLock(t *testing.T) {
	parentDataDir := t.TempDir()
	childDataDir := filepath.Join(parentDataDir, "artifacts")
	entered := make(chan struct{})
	release := make(chan struct{})
	type openResult struct {
		repository *Repository
		err        error
	}
	childResult := make(chan openResult, 1)
	go func() {
		repository, err := openRepositoryWith(
			t.Context(), childDataDir, modernc.Driver{}, repositoryOpenHooks{
				beforeVaultOpen: func(*os.Root, string) error {
					close(entered)
					<-release
					return nil
				},
			},
		)
		childResult <- openResult{repository: repository, err: err}
	}()
	<-entered

	parent, err := openRepository(t.Context(), parentDataDir, modernc.Driver{})
	if parent != nil {
		t.Cleanup(func() { require.NoError(t, parent.Close()) })
	}
	assert.Nil(t, parent)
	assert.ErrorContains(t, err, "ownership lock")
	assert.ErrorContains(t, err, "vault is locked")
	if assert.Error(t, err) {
		assert.NotContains(t, err.Error(), "old loose artifact layout")
	}

	close(release)
	result := <-childResult
	require.NoError(t, result.err)
	require.NotNil(t, result.repository)
	require.NoError(t, result.repository.Close())
}

func TestRepositoryValidatesExternalTargetsAgainstOpenedRootIdentity(t *testing.T) {
	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	vaultRoot := filepath.Join(dataDir, "artifacts")

	for name, target := range map[string]string{
		"same root":  vaultRoot,
		"ancestor":   dataDir,
		"descendant": filepath.Join(vaultRoot, "blobs"),
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorContains(t, repository.validateExternalTarget(target), "must not overlap")
		})
	}

	alias := filepath.Join(t.TempDir(), "vault-alias")
	if err := os.Symlink(vaultRoot, alias); err == nil {
		assert.ErrorContains(t, repository.validateExternalTarget(alias), "must not overlap")
	}
	assert.NoError(t, repository.validateExternalTarget(t.TempDir()))

	relocatedParent := t.TempDir()
	movedRoot := filepath.Join(relocatedParent, "moved-artifacts")
	if err := os.Rename(vaultRoot, movedRoot); err == nil {
		restored := false
		t.Cleanup(func() {
			if !restored {
				require.NoError(t, os.Rename(movedRoot, vaultRoot))
			}
		})
		assert.ErrorContains(t, repository.validateExternalTarget(movedRoot), "must not overlap",
			"opened directory identity must survive a root rename")
		assert.ErrorContains(t,
			repository.validateExternalTarget(filepath.Join(movedRoot, "future-export")),
			"must not overlap", "a descendant must resolve through the moved root identity")
		assert.ErrorContains(t, repository.validateExternalTarget(relocatedParent),
			"cannot prove non-overlap", "a moved root's ancestor must fail closed")
		assert.ErrorContains(t, repository.validateExternalTarget(t.TempDir()),
			"cannot prove non-overlap", "an unreachable canonical root makes disjointness unprovable")
		require.NoError(t, os.Rename(movedRoot, vaultRoot))
		restored = true
	}
}

func TestOpenRepositoryRetainsAbsoluteCanonicalRootForRelativeDataDir(t *testing.T) {
	dataDir := t.TempDir()
	workingDirectory, err := os.Getwd()
	require.NoError(t, err)
	relativeDataDir, err := filepath.Rel(workingDirectory, dataDir)
	require.NoError(t, err)
	repository, err := OpenRepository(t.Context(), relativeDataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	assert.True(t, filepath.IsAbs(repository.rootPath))
	canonical, err := filepath.EvalSymlinks(filepath.Join(dataDir, "artifacts"))
	require.NoError(t, err)
	assert.Equal(t, canonical, repository.rootPath)
	assert.ErrorContains(t, repository.validateExternalTarget(relativeDataDir), "must not overlap",
		"the relative data directory is an ancestor of the canonical vault root")
}

func TestRepositoryCloseWaitsForReaderAndIsIdempotent(t *testing.T) {
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
	createContractArtifact(t, repository.Store(), ref, []byte("reader lease"))
	_, reader, err := repository.Store().Open(t.Context(), ref)
	require.NoError(t, err)
	one := make([]byte, 1)
	_, err = reader.Read(one)
	require.NoError(t, err)

	closeResult := make(chan error, 1)
	go func() { closeResult <- repository.Close() }()
	select {
	case err := <-closeResult:
		require.Fail(t, "repository close returned before reader close", "error: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	_, err = io.Copy(io.Discard, reader)
	require.NoError(t, err)
	require.NoError(t, reader.Verify())
	require.NoError(t, reader.Close())
	require.NoError(t, <-closeResult)
	assert.NoError(t, repository.Close())
}

func TestOpenRepositoryJoinsVaultCleanupFailureAfterRootRetentionFailure(t *testing.T) {
	retainErr := errors.New("retain root failed")
	cleanupErr := errors.New("vault cleanup failed")
	dataDir := t.TempDir()
	validationCalls := 0
	repository, err := openRepositoryWith(
		t.Context(), dataDir, modernc.Driver{}, repositoryOpenHooks{
			validateRoot: func(*os.Root, string) (fs.FileInfo, error) {
				validationCalls++
				if validationCalls == 1 {
					return os.Stat(filepath.Join(dataDir, "artifacts"))
				}
				return nil, retainErr
			},
			closeVault: func(vault *docbank.Vault) error {
				return errors.Join(vault.Close(), cleanupErr)
			},
		},
	)
	assert.Nil(t, repository)
	assert.ErrorIs(t, err, retainErr)
	assert.ErrorIs(t, err, cleanupErr)

	reopened, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err, "the real vault close must still release its ownership lock")
	require.NoError(t, reopened.Close())
}

func snapshotDirectory(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snapshot := make(map[string][]byte)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			snapshot[relative+string(filepath.Separator)] = nil
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			snapshot[relative] = []byte("symlink:" + target)
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[relative] = content
		return nil
	}))
	return snapshot
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}

func directoryEntryNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func createPublicDocbankVault(t *testing.T, root string, driver docsqlite.Driver) {
	t.Helper()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root, SQLite: driver})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
}
