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
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/mattn"
	"go.kenn.io/docbank/sqlite/modernc"
)

func TestOpenRepositoryOwnsCompressedDocbankContent(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()

	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	body := bytes.Repeat([]byte("repository compression policy\n"), 200)
	body = body[:4<<10]
	result := createCheckpointBody(t, repository.Content(), 1, body)
	assert.Equal("zstd", result.Physical.Encoding)
	assert.DirExists(filepath.Join(dataDir, "artifacts"))
	assert.FileExists(filepath.Join(dataDir, "artifacts", "docbank.db"))
}

func TestOpenRepositorySupportsDocbankSQLiteDrivers(t *testing.T) {
	t.Parallel()

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
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	dataDir := t.TempDir()
	artifactDir := filepath.Join(dataDir, "artifacts")
	legacy := filepath.Join(artifactDir, contractOrigin, string(KindCheckpoints), "cp-0000000001.json")
	require.NoError(os.MkdirAll(filepath.Dir(legacy), 0o755))
	original := []byte("disposable old loose artifact")
	require.NoError(os.WriteFile(legacy, original, 0o644))

	repository, err := OpenRepository(t.Context(), dataDir)
	assert.Nil(repository)
	require.ErrorContains(err, "old loose artifact layout")
	got, readErr := os.ReadFile(legacy)
	require.NoError(readErr)
	assert.Equal(original, got)
	assert.NoFileExists(filepath.Join(artifactDir, "docbank.db"))
}

func TestOpenRepositoryUsesDocbankHierarchyLock(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()

	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	second, err := OpenRepository(t.Context(), dataDir)
	assert.Nil(second)
	require.ErrorContains(t, err, "vault is locked")

	overlapping, err := OpenRepository(t.Context(), filepath.Join(dataDir, "artifacts"))
	assert.Nil(overlapping)
	assert.ErrorContains(err, "vault is locked")
}

func TestOpenRepositoryFollowsFinalRootSymlink(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	realDataDir := t.TempDir()
	realRoot := filepath.Join(realDataDir, "artifacts")
	realRepository, err := openRepository(t.Context(), realDataDir, modernc.Driver{})
	require.NoError(err)
	require.NoError(realRepository.Close())

	aliasDataDir := t.TempDir()
	require.NoError(os.Symlink(realRoot, filepath.Join(aliasDataDir, "artifacts")))
	aliased, err := openRepository(t.Context(), aliasDataDir, modernc.Driver{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(aliased.Close()) })
	canonicalRoot, err := filepath.EvalSymlinks(realRoot)
	require.NoError(err)
	assert.Equal(t, canonicalRoot, aliased.rootPath)
}

func TestOpenRepositoryRetainsAbsoluteCanonicalRoot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// Serial: Chdir changes the process-wide working directory.
	dataDir := t.TempDir()
	// Chdir to the temp dir's parent so the relative path stays on one
	// volume; on Windows CI the checkout and temp dirs are on different
	// drives and filepath.Rel cannot bridge them.
	t.Chdir(filepath.Dir(dataDir))
	repository, err := OpenRepository(t.Context(), filepath.Base(dataDir))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repository.Close()) })

	canonical, err := filepath.EvalSymlinks(filepath.Join(dataDir, "artifacts"))
	require.NoError(err)
	assert.True(filepath.IsAbs(repository.rootPath))
	assert.Equal(canonical, repository.rootPath)
}

func TestRepositoryCloseWaitsForReaderAndIsIdempotent(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
	createContractArtifact(t, repository.Content(), ref, []byte("reader lease"))
	_, reader, err := repository.Content().Open(t.Context(), ref)
	require.NoError(err)
	one := make([]byte, 1)
	_, err = reader.Read(one)
	require.NoError(err)

	closeResult := make(chan error, 1)
	go func() { closeResult <- repository.Close() }()
	select {
	case err := <-closeResult:
		require.Fail("repository close returned before reader close", "error: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	_, err = io.Copy(io.Discard, reader)
	require.NoError(err)
	require.NoError(reader.Verify())
	require.NoError(reader.Close())
	require.NoError(<-closeResult)
	assert.NoError(t, repository.Close())
}
