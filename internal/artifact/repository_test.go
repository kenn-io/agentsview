package artifact

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/mattn"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

func TestOpenRepositoryOwnsCompressedDocbankContent(t *testing.T) {
	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	body := bytes.Repeat([]byte("repository compression policy\n"), 200)
	body = body[:4<<10]
	result := createCheckpointBody(t, repository.Content(), 1, body)
	assert.Equal(t, "zstd", result.Physical.Encoding)
	assert.DirExists(t, filepath.Join(dataDir, "artifacts"))
	assert.FileExists(t, filepath.Join(dataDir, "artifacts", "docbank.db"))
}

func TestOpenRepositorySupportsDocbankSQLiteDrivers(t *testing.T) {
	for _, driver := range []docsqlite.Driver{mattn.Driver{}, modernc.Driver{}} {
		t.Run(driver.Name(), func(t *testing.T) {
			repository, err := openRepository(t.Context(), t.TempDir(), driver)
			require.NoError(t, err)
			result := createCheckpointBody(t, repository.Content(), 1, []byte("driver parity"))
			assert.True(t, result.Created)
			require.NoError(t, repository.Close())
		})
	}
}

func TestOpenRepositoryRejectsLegacyLooseLayoutWithoutMutation(t *testing.T) {
	dataDir := t.TempDir()
	artifactDir := filepath.Join(dataDir, "artifacts")
	legacy := filepath.Join(artifactDir, contractOrigin, string(KindCheckpoints), "cp-0000000001.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
	original := []byte("disposable old loose artifact")
	require.NoError(t, os.WriteFile(legacy, original, 0o644))

	repository, err := OpenRepository(t.Context(), dataDir)
	assert.Nil(t, repository)
	assert.ErrorContains(t, err, "old loose artifact layout")
	got, readErr := os.ReadFile(legacy)
	require.NoError(t, readErr)
	assert.Equal(t, original, got)
	assert.NoFileExists(t, filepath.Join(artifactDir, "docbank.db"))
}

func TestOpenRepositoryUsesDocbankHierarchyLock(t *testing.T) {
	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	second, err := OpenRepository(t.Context(), dataDir)
	assert.Nil(t, second)
	assert.ErrorContains(t, err, "vault is locked")

	overlapping, err := OpenRepository(t.Context(), filepath.Join(dataDir, "artifacts"))
	assert.Nil(t, overlapping)
	assert.ErrorContains(t, err, "vault is locked")
}

func TestOpenRepositoryFollowsFinalRootSymlink(t *testing.T) {
	realDataDir := t.TempDir()
	realRoot := filepath.Join(realDataDir, "artifacts")
	realRepository, err := openRepository(t.Context(), realDataDir, modernc.Driver{})
	require.NoError(t, err)
	require.NoError(t, realRepository.Close())

	aliasDataDir := t.TempDir()
	require.NoError(t, os.Symlink(realRoot, filepath.Join(aliasDataDir, "artifacts")))
	aliased, err := openRepository(t.Context(), aliasDataDir, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, aliased.Close()) })
	canonicalRoot, err := filepath.EvalSymlinks(realRoot)
	require.NoError(t, err)
	assert.Equal(t, canonicalRoot, aliased.rootPath)
}

func TestRepositoryRejectsOverlappingFolderTargets(t *testing.T) {
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
			transport, err := repository.NewFolderTransport(target)
			assert.Nil(t, transport)
			assert.ErrorContains(t, err, "must not overlap")
		})
	}

	transport, err := repository.NewFolderTransport(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, transport.(*folderTransport).Close())
}

func TestOpenRepositoryRetainsAbsoluteCanonicalRoot(t *testing.T) {
	dataDir := t.TempDir()
	workingDirectory, err := os.Getwd()
	require.NoError(t, err)
	relativeDataDir, err := filepath.Rel(workingDirectory, dataDir)
	require.NoError(t, err)
	repository, err := OpenRepository(t.Context(), relativeDataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	canonical, err := filepath.EvalSymlinks(filepath.Join(dataDir, "artifacts"))
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(repository.rootPath))
	assert.Equal(t, canonical, repository.rootPath)
}

func TestRepositoryCloseWaitsForReaderAndIsIdempotent(t *testing.T) {
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
	createContractArtifact(t, repository.Content(), ref, []byte("reader lease"))
	_, reader, err := repository.Content().Open(t.Context(), ref)
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
