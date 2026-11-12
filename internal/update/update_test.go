package update

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/releasesig"
	"go.kenn.io/agentsview/internal/releasesig/releasesigtest"
)

func TestIsDevBuildVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"dev", true},
		{"unknown", true},
		{"", true},
		{"0.1.0", false},
		{"v0.1.0", false},
		{"0.1.0-2-gabcdef", true},
		{"v0.1.0-2-gabcdef-dirty", true},
		{"0.1.0-rc1", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			assert.Equal(t, tt.want, IsDevBuildVersion(tt.version))
		})
	}
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		v1, v2 string
		want   bool
	}{
		{"0.2.0", "0.1.0", true},
		{"0.1.0", "0.2.0", false},
		{"0.1.0", "0.1.0", false},
		{"1.0.0", "0.9.9", true},
		{"0.1.0-rc2", "0.1.0-rc1", true},
		{"0.1.0", "0.1.0-rc1", true},
	}
	for _, tt := range tests {
		name := tt.v1 + "_vs_" + tt.v2
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, isNewer(tt.v1, tt.v2))
		})
	}
}

func TestExtractChecksum(t *testing.T) {
	body := `abc123  some_other_file.tar.gz
deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef  agentsview_0.1.0_linux_amd64.tar.gz
fff000  yet_another.zip`

	tests := []struct {
		filename string
		want     string
	}{
		{"agentsview_0.1.0_linux_amd64.tar.gz", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
		{"nonexistent.tar.gz", ""},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			assert.Equal(t, tt.want, extractChecksum(body, tt.filename))
		})
	}
}

func TestResolveLatestTag(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		location   string
		wantTag    string
		wantErrSub string
	}{
		{
			name:     "valid 302 redirect",
			status:   http.StatusFound,
			location: "https://github.com/kenn-io/agentsview/releases/tag/v0.30.1",
			wantTag:  "v0.30.1",
		},
		{
			name:     "pre-release tag",
			status:   http.StatusFound,
			location: "https://github.com/kenn-io/agentsview/releases/tag/v0.9.0-rc1",
			wantTag:  "v0.9.0-rc1",
		},
		{
			name:       "200 OK is not a redirect",
			status:     http.StatusOK,
			wantErrSub: "expected redirect",
		},
		{
			name:       "redirect target without /tag/",
			status:     http.StatusFound,
			location:   "https://github.com/kenn-io/agentsview/releases",
			wantErrSub: "unexpected redirect target",
		},
		{
			name:       "empty tag after /tag/",
			status:     http.StatusFound,
			location:   "https://github.com/kenn-io/agentsview/releases/tag/",
			wantErrSub: "empty tag",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					if tt.location != "" {
						w.Header().Set("Location", tt.location)
					}
					w.WriteHeader(tt.status)
				},
			))
			defer srv.Close()

			tag, err := resolveLatestTag(t.Context(), srv.URL)
			if tt.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSub)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantTag, tag)
		})
	}
}

func TestFetchContentLength(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		bodySize   int
		wantSize   int64
		wantErrSub string
	}{
		{
			name:     "200 with body",
			status:   http.StatusOK,
			bodySize: 1234,
			wantSize: 1234,
		},
		{
			name:       "404",
			status:     http.StatusNotFound,
			wantErrSub: "404",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					if tt.bodySize > 0 {
						w.Header().Set(
							"Content-Length",
							strconv.Itoa(tt.bodySize),
						)
					}
					w.WriteHeader(tt.status)
				},
			))
			defer srv.Close()

			size, err := fetchContentLength(t.Context(), srv.URL)
			if tt.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSub)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSize, size)
		})
	}
}

func TestSanitizePath(t *testing.T) {
	destDir := t.TempDir()

	tests := []struct {
		name     string
		path     string
		wantPath string
		wantErr  bool
	}{
		{"normal", "agentsview", filepath.Join(destDir, "agentsview"), false},
		{"subdir", "dir/agentsview", filepath.Join(destDir, "dir/agentsview"), false},
		{"absolute", "/etc/passwd", "", true},
		{"traversal", "../../../etc/passwd", "", true},
		{"hidden_traversal", "foo/../../etc/passwd", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, err := sanitizePath(destDir, tt.path)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPath, gotPath)
		})
	}
}

func TestExtractTarGz(t *testing.T) {
	srcDir := t.TempDir()
	destDir := t.TempDir()

	// Create a test tar.gz with a dummy binary
	archivePath := filepath.Join(srcDir, "test.tar.gz")
	createTestTarGz(t, archivePath, "agentsview", "binary-content")

	require.NoError(t, extractTarGz(archivePath, destDir))

	content, err := os.ReadFile(
		filepath.Join(destDir, "agentsview"),
	)
	require.NoError(t, err, "read extracted file")
	assert.Equal(t, "binary-content", string(content))
}

func TestInstallBinaryToSetsExecutableMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits not meaningful on Windows")
	}

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "agentsview")
	dstPath := filepath.Join(dstDir, "agentsview")

	require.NoError(t, os.WriteFile(srcPath, []byte("binary"), 0o644))

	require.NoError(t, installBinaryTo(srcPath, dstPath))

	info, err := os.Stat(dstPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestInstallBinaryToPreservesOnSourceMissing(t *testing.T) {
	dstDir := t.TempDir()
	dstPath := filepath.Join(dstDir, "agentsview")

	require.NoError(t, os.WriteFile(dstPath, []byte("original"), 0o755))

	missingSrc := filepath.Join(t.TempDir(), "does-not-exist")

	require.Error(t, installBinaryTo(missingSrc, dstPath), "expected error from missing source")

	got, err := os.ReadFile(dstPath)
	require.NoError(t, err, "dstPath should still exist")
	assert.Equal(t, "original", string(got))

	_, err = os.Stat(dstPath + ".new")
	assert.True(t, os.IsNotExist(err), "staging .new file should not be left behind")
	_, err = os.Stat(dstPath + ".old")
	assert.True(t, os.IsNotExist(err), "backup .old file should not be left behind")
}

func TestInstallBinaryToNeverMissingDuringUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(
			"Windows must rename the running binary aside; " +
				"a brief missing window is unavoidable",
		)
	}

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "agentsview")
	dstPath := filepath.Join(dstDir, "agentsview")

	require.NoError(t, os.WriteFile(srcPath, []byte("new"), 0o755))
	require.NoError(t, os.WriteFile(dstPath, []byte("old"), 0o755))

	var observations, missing atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(dstPath); err != nil &&
				os.IsNotExist(err) {
				missing.Add(1)
			}
			observations.Add(1)
		}
	}()

	const iterations = 1000
	for i := range iterations {
		if err := installBinaryTo(srcPath, dstPath); err != nil {
			close(stop)
			<-done
			require.NoErrorf(t, err, "install iteration %d", i)
		}
	}

	close(stop)
	<-done

	t.Logf(
		"iterations=%d observations=%d missing=%d",
		iterations, observations.Load(), missing.Load(),
	)

	if observations.Load() < 1000 {
		t.Skipf(
			"observer ran only %d times, test inconclusive",
			observations.Load(),
		)
	}
	assert.Zero(t, missing.Load(),
		"dstPath observed missing %d times during install", missing.Load())
}

func TestInstallBinaryToRemovesStaleStagingFile(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "agentsview")
	dstPath := filepath.Join(dstDir, "agentsview")

	require.NoError(t, os.WriteFile(srcPath, []byte("new-binary"), 0o755))
	require.NoError(t, os.WriteFile(dstPath, []byte("old-binary"), 0o755))
	stagingPath := dstPath + ".new"
	require.NoError(t, os.WriteFile(stagingPath, []byte("stale-staging"), 0o644))

	require.NoError(t, installBinaryTo(srcPath, dstPath))

	_, err := os.Stat(stagingPath)
	assert.True(t, os.IsNotExist(err), "stale staging file should be removed, got err=%v", err)
}

func TestInstallBinaryTo(t *testing.T) {
	tests := []struct {
		name         string
		existingDest string
		newBinary    string
		want         string
	}{
		{
			name:         "Install to empty destination",
			existingDest: "",
			newBinary:    "new-binary",
			want:         "new-binary",
		},
		{
			name:         "Install over existing",
			existingDest: "old-binary",
			newBinary:    "newer-binary",
			want:         "newer-binary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srcDir := t.TempDir()
			dstDir := t.TempDir()

			srcPath := filepath.Join(srcDir, "agentsview")
			dstPath := filepath.Join(dstDir, "agentsview")

			if tt.existingDest != "" {
				require.NoError(t, os.WriteFile(dstPath, []byte(tt.existingDest), 0o755))
			}

			require.NoError(t, os.WriteFile(srcPath, []byte(tt.newBinary), 0o755))

			require.NoError(t, installBinaryTo(srcPath, dstPath))

			got, err := os.ReadFile(dstPath)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))

			if tt.existingDest != "" {
				_, err := os.Stat(dstPath + ".old")
				assert.True(t, os.IsNotExist(err), "backup .old file should be removed")
			}
		})
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{10485760, "10.0 MB"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d_bytes", tt.bytes), func(t *testing.T) {
			assert.Equal(t, tt.want, FormatSize(tt.bytes))
		})
	}
}

func TestCacheRoundtrip(t *testing.T) {
	dir := t.TempDir()

	saveCache("v1.2.3", dir)

	cached, err := loadCache(dir)
	require.NoError(t, err)
	assert.Equal(t, "v1.2.3", cached.Version)
}

func TestNormalizeSemver(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"0.1.0", "v0.1.0"},
		{"v0.1.0", "v0.1.0"},
		{"0.1.0-rc1", "v0.1.0-rc.1"},
		{"0.1.0-2-gabcdef", "v0.1.0"},
		{"0.1.0-2-gabcdef-dirty", "v0.1.0"},
		{"1.0.0-beta10", "v1.0.0-beta.10"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeSemver(tt.input))
		})
	}
}

func createTestTarGz(
	t *testing.T,
	archivePath, fileName, content string,
) {
	t.Helper()

	f, err := os.Create(archivePath)
	require.NoError(t, err)
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	data := []byte(content)
	header := &tar.Header{
		Name: fileName,
		Mode: 0o755,
		Size: int64(len(data)),
	}
	require.NoError(t, tw.WriteHeader(header))
	_, err = tw.Write(data)
	require.NoError(t, err)
}

func TestFetchVerifiedChecksum(t *testing.T) {
	const asset = "agentsview_9.9.9_linux_amd64.tar.gz"
	const sum = "a49876edf9fe8d0641978afe69d776540175852a446a5d42a6d439215eb0dc45"
	sums := []byte(sum + "  " + asset + "\n" +
		strings.Repeat("0", 64) + "  agentsview_9.9.9_darwin_arm64.tar.gz\n")

	releaseKey := releasesigtest.NewKey(t)
	keys, err := releasesig.ParseKeys(releaseKey.PEM)
	require.NoError(t, err)
	goodSig := releaseKey.Sign(t, sums)

	tampered := []byte(strings.Repeat("f", 64) + "  " + asset + "\n")
	otherKeySig := releasesigtest.NewKey(t).Sign(t, sums)

	tests := []struct {
		name       string
		sums       []byte
		sig        []byte // nil serves a 404
		asset      string
		wantErrIs  error
		wantErrSub string
	}{
		{name: "signed by a release key", sums: sums, sig: goodSig, asset: asset},
		{name: "signature missing", sums: sums, asset: asset, wantErrSub: "SHA256SUMS.sig"},
		{name: "SHA256SUMS altered after signing", sums: tampered, sig: goodSig, asset: asset, wantErrIs: releasesig.ErrBadSignature},
		{name: "signed by another key", sums: sums, sig: otherKeySig, asset: asset, wantErrIs: releasesig.ErrBadSignature},
		{name: "no entry for this platform", sums: sums, sig: goodSig, asset: "agentsview_9.9.9_windows_arm64.zip", wantErrSub: "no entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v9.9.9/SHA256SUMS":
						_, _ = w.Write(tt.sums)
					case "/v9.9.9/SHA256SUMS.sig":
						if tt.sig == nil {
							http.NotFound(w, r)
							return
						}
						_, _ = w.Write(tt.sig)
					default:
						http.NotFound(w, r)
					}
				},
			))
			defer srv.Close()

			got, err := fetchVerifiedChecksum(
				t.Context(), keys, srv.URL+"/v9.9.9/SHA256SUMS", tt.asset,
			)
			switch {
			case tt.wantErrIs != nil:
				require.ErrorIs(t, err, tt.wantErrIs)
				assert.Empty(t, got)
			case tt.wantErrSub != "":
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSub)
				assert.Empty(t, got)
			default:
				require.NoError(t, err)
				assert.Equal(t, sum, got)
			}
		})
	}
}

func TestFetchVerifiedChecksumRejectsOversizedSHA256SUMS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(make([]byte, maxChecksumsBytes+1))
		},
	))
	defer srv.Close()

	keys, err := releasesig.ParseKeys(releasesigtest.NewKey(t).PEM)
	require.NoError(t, err)
	_, err = fetchVerifiedChecksum(
		t.Context(), keys, srv.URL+"/SHA256SUMS", "agentsview.tar.gz",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "larger than")
}

func TestPerformUpdateRefusesUnverifiedChecksum(t *testing.T) {
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			downloads.Add(1)
			_, _ = w.Write([]byte("binary"))
		},
	))
	defer srv.Close()

	err := PerformUpdate(t.Context(), &UpdateInfo{
		AssetName:   "agentsview_9.9.9_linux_amd64.tar.gz",
		DownloadURL: srv.URL + "/agentsview_9.9.9_linux_amd64.tar.gz",
		checksumErr: releasesig.ErrBadSignature,
	}, nil)
	require.ErrorIs(t, err, releasesig.ErrBadSignature)
	assert.Contains(t, err.Error(), "refusing unverified binary")
	assert.Zero(t, downloads.Load(), "nothing is downloaded without a verified checksum")
}
