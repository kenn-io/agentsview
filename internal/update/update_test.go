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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestFetchSmallAsset(t *testing.T) {
	const limit = 8
	tests := []struct {
		name       string
		status     int
		body       string
		want       string
		wantErrSub string
	}{
		{name: "body under limit", status: http.StatusOK, body: "v1\n", want: "v1\n"},
		{name: "body at limit", status: http.StatusOK, body: "12345678", want: "12345678"},
		{
			name:       "body over limit",
			status:     http.StatusOK,
			body:       "123456789",
			wantErrSub: "body exceeds 8 bytes",
		},
		{name: "not found", status: http.StatusNotFound, wantErrSub: "404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var method, userAgent string
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					method, userAgent = r.Method, r.UserAgent()
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.body))
				},
			))
			t.Cleanup(srv.Close)

			got, err := fetchSmallAsset(t.Context(), srv.URL+"/VERSION", limit)
			assert.Equal(t, http.MethodGet, method)
			assert.Equal(t, updateUserAgent, userAgent)
			if tt.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSub)
				assert.Contains(t, err.Error(), srv.URL+"/VERSION")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFetchChecksumFromFile(t *testing.T) {
	sum := sha256Hex("binary")
	tests := []struct {
		name       string
		status     int
		want       string
		wantErrSub string
	}{
		{name: "listed asset", status: http.StatusOK, want: sum},
		{
			name:       "missing checksums file",
			status:     http.StatusNotFound,
			wantErrSub: "failed to fetch checksums",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var userAgent string
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					userAgent = r.UserAgent()
					w.WriteHeader(tt.status)
					_, _ = fmt.Fprintf(w, "%s  agentsview-linux-amd64\n", sum)
				},
			))
			t.Cleanup(srv.Close)

			got, err := fetchChecksumFromFile(t.Context(),
				srv.URL+"/SHA256SUMS", "agentsview-linux-amd64",
			)
			assert.Equal(t, updateUserAgent, userAgent)
			if tt.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSub)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
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

// fakeExecutable makes executablePath report path for the test.
func fakeExecutable(t *testing.T, path string, err error) {
	t.Helper()
	old := executablePath
	executablePath = func() (string, error) { return path, err }
	t.Cleanup(func() { executablePath = old })
}

func TestInstallTarget(t *testing.T) {
	rolling := rollingAssetName(runtime.GOOS, runtime.GOARCH)
	tests := []struct {
		name      string
		exeName   string
		symlink   bool
		rawBinary bool
		want      string
	}{
		{
			name:      "raw update replaces renamed executable",
			exeName:   rolling,
			rawBinary: true,
			want:      rolling,
		},
		{
			name:      "raw update replaces symlink target",
			exeName:   rolling,
			symlink:   true,
			rawBinary: true,
			want:      rolling,
		},
		{
			name:    "archive update installs agentsview beside executable",
			exeName: rolling,
			want:    binaryName(),
		},
		{
			name:    "archive update resolves symlink directory",
			exeName: binaryName(),
			symlink: true,
			want:    binaryName(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.symlink && runtime.GOOS == "windows" {
				t.Skip("symlinks need extra privileges on Windows")
			}
			// Resolve the temp dir itself, which may sit behind a symlink.
			realDir, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			exe := filepath.Join(realDir, tt.exeName)
			require.NoError(t, os.WriteFile(exe, []byte("old"), 0o755))
			if tt.symlink {
				link := filepath.Join(t.TempDir(), "agentsview-link")
				require.NoError(t, os.Symlink(exe, link))
				exe = link
			}
			fakeExecutable(t, exe, nil)

			got, err := installTarget(tt.rawBinary)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(realDir, tt.want), got)
		})
	}
}

func TestInstallTargetExecutableError(t *testing.T) {
	fakeExecutable(t, "", os.ErrNotExist)

	_, err := installTarget(true)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Contains(t, err.Error(), "find current executable")
}

// archiveBytes returns a release archive holding the agentsview binary.
func archiveBytes(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release.tar.gz")
	createTestTarGz(t, path, binaryName(), content)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestPerformUpdateInstallTarget(t *testing.T) {
	rolling := rollingAssetName(runtime.GOOS, runtime.GOARCH)
	tests := []struct {
		name      string
		rawBinary bool
		assetName string
		asset     func(t *testing.T) string
		wantFiles map[string]string
	}{
		{
			name:      "raw update replaces the running executable in place",
			rawBinary: true,
			assetName: rolling,
			asset:     func(*testing.T) string { return "new" },
			wantFiles: map[string]string{rolling: "new"},
		},
		{
			name:      "archive update installs agentsview beside it",
			assetName: "agentsview_0.45.0_test.tar.gz",
			asset:     func(t *testing.T) string { t.Helper(); return archiveBytes(t, "new") },
			wantFiles: map[string]string{rolling: "old", binaryName(): "new"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asset := tt.asset(t)
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(asset))
				},
			))
			t.Cleanup(srv.Close)

			dir := t.TempDir()
			exe := filepath.Join(dir, rolling)
			require.NoError(t, os.WriteFile(exe, []byte("old"), 0o755))
			fakeExecutable(t, exe, nil)

			require.NoError(t, PerformUpdate(t.Context(), &UpdateInfo{
				DownloadURL: srv.URL + "/" + tt.assetName,
				AssetName:   tt.assetName,
				Checksum:    sha256Hex(asset),
				rawBinary:   tt.rawBinary,
			}, nil))

			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			got := map[string]string{}
			for _, e := range entries {
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				require.NoError(t, err)
				got[e.Name()] = string(data)
			}
			assert.Equal(t, tt.wantFiles, got)
		})
	}
}

func TestDownloadFileReportsProgress(t *testing.T) {
	const size = 100_000 // spans several 32 KB reads
	type report struct{ downloaded, total int64 }
	tests := []struct {
		name        string
		status      int
		wantReports []report // first and last report; nil when none
		wantErrSub  string
	}{
		{
			name:        "reports the start before any bytes",
			status:      http.StatusOK,
			wantReports: []report{{0, size}, {size, size}},
		},
		{
			name:       "failed response reports nothing",
			status:     http.StatusNotFound,
			wantErrSub: "404",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tt.status)
					if tt.status == http.StatusOK {
						_, _ = w.Write(make([]byte, size))
					}
				},
			))
			defer srv.Close()

			var reports []report
			_, err := downloadFile(t.Context(), srv.URL,
				filepath.Join(t.TempDir(), "asset"), size,
				func(downloaded, total int64) {
					reports = append(reports, report{downloaded, total})
				},
			)
			if tt.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSub)
				assert.Empty(t, reports)
				return
			}
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(reports), 2)
			assert.Equal(t, tt.wantReports,
				[]report{reports[0], reports[len(reports)-1]})
		})
	}
}
