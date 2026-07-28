package main

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/pricing/catalog"
)

func TestAppendModelOverlay_FillsGaps(t *testing.T) {
	base := []catalog.ModelPricing{
		{ModelPattern: "existing-model", InputPerMTok: mustRate("1"), OutputPerMTok: mustRate("2")},
	}
	result := appendModelOverlay(base)

	byPattern := make(map[string]catalog.ModelPricing)
	for _, p := range result {
		byPattern[p.ModelPattern] = p
	}

	assert.Contains(t, byPattern, "existing-model", "base model preserved")
	assert.Contains(t, byPattern, "claude-opus-4-8", "overlay model appended")
	assert.Contains(t, byPattern, "gpt-5.5", "overlay model appended")
}

func TestAppendModelOverlay_DoesNotOverwriteExisting(t *testing.T) {
	base := []catalog.ModelPricing{
		{ModelPattern: "claude-opus-4-8", InputPerMTok: mustRate("99"), OutputPerMTok: mustRate("99")},
	}
	result := appendModelOverlay(base)

	var count int
	for _, p := range result {
		if p.ModelPattern == "claude-opus-4-8" {
			count++
			assert.Equal(t, mustRate("99"), p.InputPerMTok, "existing rate preserved")
		}
	}
	require.Equal(t, 1, count, "no duplicate entries for existing model")
}

func TestComputeVersion_Deterministic(t *testing.T) {
	data := []byte(`[{"ModelPattern":"test","InputPerMTok":{"microdollars":1000000}}]`)
	v1 := computeVersion(data)
	v2 := computeVersion(data)
	assert.Equal(t, v1, v2)
	assert.Regexp(t, `^litellm-[0-9a-f]{12}$`, v1)
}

func TestComputeVersion_DifferentInputDifferentHash(t *testing.T) {
	v1 := computeVersion([]byte(`[{"ModelPattern":"a"}]`))
	v2 := computeVersion([]byte(`[{"ModelPattern":"b"}]`))
	assert.NotEqual(t, v1, v2)
}

func TestDefaultOutputPathTargetsIgnoredSnapshot(t *testing.T) {
	assert.Equal(
		t,
		"internal/pricing/snapshot/litellm_snapshot.json.gz",
		filepath.ToSlash(defaultOutputPath),
	)
}

func TestDefaultSnapshotConstantsNonEmpty(t *testing.T) {
	assert.NotEmpty(t, defaultSnapshotRef, "pinned ref must be set")
	assert.Len(t, defaultSnapshotRef, 40, "pinned ref must be a full SHA-1")
	assert.NotEmpty(t, defaultSnapshotSHA256, "pinned SHA256 must be set")
	assert.Len(t, defaultSnapshotSHA256, 64, "pinned SHA256 must be a hex-encoded SHA-256")
	assert.NotEmpty(t, defaultSnapshotBranch, "pinned branch must be set")
	assert.NotEmpty(t, defaultSnapshotFile, "pinned file must be set")
}

func TestFileURLForPathUsesFileScheme(t *testing.T) {
	dir := t.TempDir()
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)

	got := fileURLForPath(dir)
	assert.True(t, strings.HasPrefix(got, "file://"), got)
	assert.NotContains(t, got, "\\")
	if filepath.VolumeName(abs) != "" {
		assert.True(t, strings.HasPrefix(got, "file:///"), got)
	}
}

func TestFileURLPathForAbsPrefixesWindowsDrivePaths(t *testing.T) {
	assert.Equal(t, "/C:/Users/test/repo", fileURLPathForAbs("C:/Users/test/repo", "C:"))
	assert.Equal(t, "/tmp/repo", fileURLPathForAbs("/tmp/repo", ""))
}

func TestValidateSnapshotFileAcceptsValidSnapshot(t *testing.T) {
	path := writeSnapshotFile(t, []byte(`{
		"version": "litellm-test",
		"models": [{"ModelPattern": "test-model", "InputPerMTok": {"microdollars": 1000000}}]
	}`))

	require.NoError(t, validateSnapshotFile(path))
}

func TestValidateSnapshotFileRejectsInvalidGzip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json.gz")
	require.NoError(t, os.WriteFile(path, []byte("not gzip"), 0o644))

	err := validateSnapshotFile(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating reader")
}

func TestValidateSnapshotFileRejectsEmptyModels(t *testing.T) {
	path := writeSnapshotFile(t, []byte(`{
		"version": "litellm-test",
		"models": []
	}`))

	err := validateSnapshotFile(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing snapshot models")
}

func TestValidateSnapshotFileRejectsOversizedDecompressedPayload(t *testing.T) {
	path := writeSnapshotFile(t, bytes.Repeat(
		[]byte(" "),
		maxSnapshotJSONBytes+1,
	))

	err := validateSnapshotFile(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decompressed snapshot exceeds")
}

func TestRestoreSnapshotFileRestoresPinnedArtifact(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")

	source := filepath.Join(repo, "litellm_snapshot.json.gz")
	require.NoError(t, os.WriteFile(source, gzipSnapshot(t, []byte(`{
		"version": "litellm-test",
		"models": [{"ModelPattern": "test-model", "InputPerMTok": {"microdollars": 1000000}}]
	}`)), 0o644))
	runGit(t, repo, "add", "litellm_snapshot.json.gz")
	runGit(t, repo, "commit", "-m", "snapshot")
	ref := runGit(t, repo, "rev-parse", "HEAD")

	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(repo))
	defer func() {
		require.NoError(t, os.Chdir(cwd))
	}()

	out := filepath.Join(repo, "out", "snapshot.json.gz")
	require.NoError(t, restoreSnapshotFile(
		out,
		ref,
		"litellm_snapshot.json.gz",
		sha256FileForTest(t, source),
		"unused-snapshot-branch",
		"",
	))

	require.FileExists(t, out)
	require.NoError(t, validateSnapshotFile(out))
	assert.Equal(t, sha256FileForTest(t, source), sha256FileForTest(t, out))
}

func TestRestoreSnapshotFileFetchesPinnedArtifactAfterBranchAdvances(t *testing.T) {
	remote := t.TempDir()
	runGit(t, remote, "init")
	runGit(t, remote, "config", "user.email", "test@example.com")
	runGit(t, remote, "config", "user.name", "Test")
	runGit(t, remote, "config", "uploadpack.allowReachableSHA1InWant", "true")

	source := filepath.Join(remote, "litellm_snapshot.json.gz")
	require.NoError(t, os.WriteFile(source, gzipSnapshot(t, []byte(`{
		"version": "litellm-old",
		"models": [{"ModelPattern": "old-model", "InputPerMTok": {"microdollars": 1000000}}]
	}`)), 0o644))
	runGit(t, remote, "add", "litellm_snapshot.json.gz")
	runGit(t, remote, "commit", "-m", "old snapshot")
	oldRef := runGit(t, remote, "rev-parse", "HEAD")
	oldSHA := sha256FileForTest(t, source)

	require.NoError(t, os.WriteFile(source, gzipSnapshot(t, []byte(`{
		"version": "litellm-new",
		"models": [{"ModelPattern": "new-model", "InputPerMTok": {"microdollars": 2000000}}]
	}`)), 0o644))
	runGit(t, remote, "add", "litellm_snapshot.json.gz")
	runGit(t, remote, "commit", "-m", "new snapshot")
	branch := runGit(t, remote, "branch", "--show-current")

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", "--depth=1", fileURLForPath(remote), clone)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(clone))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(cwd))
	})

	out := filepath.Join(clone, "out", "snapshot.json.gz")
	require.NoError(t, restoreSnapshotFile(
		out,
		oldRef,
		"litellm_snapshot.json.gz",
		oldSHA,
		branch,
		"",
	))

	assert.Equal(t, oldSHA, sha256FileForTest(t, out))
}

func TestRestoreSnapshotFileDownloadsPinnedArtifactWithoutGitCheckout(t *testing.T) {
	source := filepath.Join(t.TempDir(), "litellm_snapshot.json.gz")
	require.NoError(t, os.WriteFile(source, gzipSnapshot(t, []byte(`{
		"version": "litellm-url",
		"models": [{"ModelPattern": "url-model", "InputPerMTok": {"microdollars": 1000000}}]
	}`)), 0o644))
	sourceSHA := sha256FileForTest(t, source)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/litellm_snapshot.json.gz", r.URL.Path)
		http.ServeFile(w, r, source)
	}))
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workspace))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(cwd))
	})

	out := filepath.Join(workspace, "snapshot", "litellm_snapshot.json.gz")
	require.NoError(t, restoreSnapshotFile(
		out,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"litellm_snapshot.json.gz",
		sourceSHA,
		"unused-snapshot-branch",
		server.URL+"/litellm_snapshot.json.gz",
	))

	assert.Equal(t, sourceSHA, sha256FileForTest(t, out))
	require.NoError(t, validateSnapshotFile(out))
}

func writeSnapshotFile(t *testing.T, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "snapshot.json.gz")
	require.NoError(t, os.WriteFile(path, gzipSnapshot(t, data), 0o644))
	return path
}

func gzipSnapshot(t *testing.T, data []byte) []byte {
	t.Helper()

	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	_, err := writer.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return out.Bytes()
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed:\n%s", args, out)
	return string(bytes.TrimSpace(out))
}

func fileURLForPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return (&url.URL{
		Scheme: "file",
		Path:   fileURLPathForAbs(filepath.ToSlash(abs), filepath.VolumeName(abs)),
	}).String()
}

func fileURLPathForAbs(abs, volumeName string) string {
	if volumeName != "" && !strings.HasPrefix(abs, "/") {
		return "/" + abs
	}
	return abs
}

func sha256FileForTest(t *testing.T, path string) string {
	t.Helper()

	sum, err := sha256File(path)
	require.NoError(t, err)
	return sum
}
