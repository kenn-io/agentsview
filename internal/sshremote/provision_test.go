//go:build unix

package sshremote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/remotesync"
)

// fakeAgentsviewScript answers `remote-sync probe` like agentsview, so these
// tests never run a real binary on the fake remote.
const fakeAgentsviewScript = `#!/bin/sh
[ "$1 $2" = "remote-sync probe" ] &&
  printf '{"version":"%s","protocol_version":%s}\n' "$FAKE_AV_VERSION" "$FAKE_AV_PROTOCOL"
`

// releaseAgentsviewScript is the release tarball's executable. It differs
// from the local executable so tests can tell which one was installed.
const releaseAgentsviewScript = fakeAgentsviewScript + "# release build\n"

const failingAgentsviewScript = "#!/bin/sh\nexit 1\n"

// pathAgentsviewScript is an agentsview v0.45.0 the user installed on the
// remote PATH.
func pathAgentsviewScript(protocol int) string {
	return versionedAgentsviewScript("v0.45.0", protocol)
}

// versionedAgentsviewScript answers `version` like every agentsview release.
// It answers `remote-sync probe` too, as only v0.23.0 and later do safely,
// and records each probe in $HOME/path-probes so a test can tell whether a
// probe ran. A release before v0.23.0 would start its server instead.
func versionedAgentsviewScript(version string, protocol int) string {
	return fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
"version ") echo "agentsview %s (commit 0123abc, built 2026-01-01T00:00:00Z)" ;;
"remote-sync probe")
  echo probed >> "$HOME/path-probes"
  printf '{"version":"0.1.0","protocol_version":%%d}\n' %d ;;
esac
`, version, protocol)
}

func fakeUnameScript(unameS, unameM string) string {
	return fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo %s ;; -m) echo %s ;; esac\n",
		unameS, unameM)
}

// remoteToolDir links the commands the remote scripts use. The real PATH may
// hold an installed agentsview, which must not answer the probe.
func remoteToolDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{
		"sh", "uname", "tar", "gzip", "mktemp", "chmod", "mkdir", "mv", "touch", "find", "rm",
	} {
		path, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(path, filepath.Join(dir, tool)))
	}
	return dir
}

type provisionEnv struct {
	fake  fakeSSH
	tools string
}

func newProvisionEnv(t *testing.T) provisionEnv {
	t.Helper()
	env := provisionEnv{fake: newFakeSSH(t), tools: remoteToolDir(t)}
	env.setRemotePath(t)
	t.Setenv("FAKE_AV_VERSION", "0.45.0")
	t.Setenv("FAKE_AV_PROTOCOL", strconv.Itoa(remotesync.ProtocolVersion))
	return env
}

// setRemotePath sets the remote PATH to dirs followed by the tool directory.
func (e provisionEnv) setRemotePath(t *testing.T, dirs ...string) {
	t.Helper()
	t.Setenv("FAKE_SSH_PATH", strings.Join(append(dirs, e.tools), ":"))
}

func (e provisionEnv) binDir() string {
	return filepath.Join(e.fake.Home, ".cache", "agentsview", "bin")
}

func (e provisionEnv) sshCalls(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(e.fake.Log)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return strings.Count(string(data), "\n")
}

func (e provisionEnv) binEntries(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(e.binDir())
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (e provisionEnv) provisioner(build LocalBuild, releaseURL, dataDir string) Provisioner {
	return Provisioner{
		Runner:   Runner{Destination: "devbox.example.test", SSHPath: e.fake.Path},
		Build:    build,
		DataDir:  dataDir,
		releases: releaseFetcher{BaseURL: releaseURL},
	}
}

// installCached places an agentsview at bin/<id> whose .last-used is old.
func (e provisionEnv) installCached(t *testing.T, id, script string) {
	t.Helper()
	dir := filepath.Join(e.binDir(), id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeExecutable(t, dir, "agentsview", script)
	setAge(t, filepath.Join(dir, ".last-used"), 40*24*time.Hour)
}

// setAge creates path when missing and sets its mtime to age ago.
func setAge(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		require.NoError(t, os.WriteFile(path, nil, 0o644))
	}
	old := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, old, old))
}

type tarEntry struct {
	name string
	body string
}

func gzipTarball(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: entry.name, Mode: 0o755, Size: int64(len(entry.body)), Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write([]byte(entry.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func writeTarball(t *testing.T, entries ...tarEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agentsview.tar.gz")
	require.NoError(t, os.WriteFile(path, gzipTarball(t, entries...), 0o600))
	return path
}

// provisionReleaseServer serves v0.45.0 for the local platform, or answers
// 404 to everything, and counts requests.
func provisionReleaseServer(t *testing.T, published bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	tarball := gzipTarball(t, tarEntry{"agentsview", releaseAgentsviewScript})
	name := releaseFileName("0.45.0", localPlatform())
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case !published:
			http.NotFound(w, r)
		case r.URL.Path == "/v0.45.0/SHA256SUMS":
			_, _ = w.Write(sumsLine(sha256Hex(tarball), name))
		case r.URL.Path == "/v0.45.0/"+name:
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// otherSupportedUname reports a supported platform that is not this one.
func otherSupportedUname() (string, string) {
	if localPlatform() == (platform{OS: "linux", Arch: "amd64"}) {
		return "Darwin", "arm64"
	}
	return "Linux", "x86_64"
}

// probeThrough runs the chosen binary on the fake remote.
func probeThrough(t *testing.T, p Provisioner, bin remoteBinary) ProbeResult {
	t.Helper()
	out, err := p.Runner.Output(t.Context(), bin.shellPath+" remote-sync probe", nil)
	require.NoError(t, err)
	var result ProbeResult
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out), &result))
	return result
}

func TestProvisionEnsure(t *testing.T) {
	otherS, otherM := otherSupportedUname()
	tests := []struct {
		name         string
		release      bool
		published    bool
		cached       []string // "release" or "content" identities already installed
		uname        []string // fake `uname -s` and `uname -m`; nil uses the real one
		pathProtocol int      // protocol of an agentsview on PATH; 0 means none
		wantBinary   string   // "release", "content", or "path"
		wantSSH      int
		wantHTTP     int64
		wantErr      string
	}{
		{
			name: "release build installs the release", release: true, published: true,
			wantBinary: "release", wantSSH: 2, wantHTTP: 2,
		},
		{
			name: "release build reuses its cached release", release: true, published: true,
			cached: []string{"release"}, wantBinary: "release", wantSSH: 1,
		},
		{
			name: "release build reuses its cached upload", release: true, published: true,
			cached: []string{"content"}, wantBinary: "content", wantSSH: 1,
		},
		{name: "development build uploads itself", wantBinary: "content", wantSSH: 2},
		{
			name: "development build reuses its upload", cached: []string{"content"},
			wantBinary: "content", wantSSH: 1,
		},
		{
			name:         "matching PATH binary wins over an upload",
			pathProtocol: remotesync.ProtocolVersion, wantBinary: "path", wantSSH: 1,
		},
		{
			name: "cached identity wins over PATH", cached: []string{"content"},
			pathProtocol: remotesync.ProtocolVersion, wantBinary: "content", wantSSH: 1,
		},
		{
			name:         "PATH binary with another protocol is skipped",
			pathProtocol: remotesync.ProtocolVersion + 1, wantBinary: "content", wantSSH: 2,
		},
		{
			name: "unpublished release uploads itself", release: true,
			wantBinary: "content", wantSSH: 2, wantHTTP: 1,
		},
		{
			name: "unpublished release on another platform fails", release: true,
			uname: []string{otherS, otherM}, wantSSH: 1, wantHTTP: 1,
			wantErr: "install agentsview on the remote",
		},
		{
			name:  "development build on another platform fails",
			uname: []string{otherS, otherM}, wantSSH: 1,
			wantErr: "install agentsview on the remote",
		},
		{
			name: "unsupported OS fails", release: true, published: true,
			uname: []string{"FreeBSD", "amd64"}, wantSSH: 1,
			wantErr: "install agentsview on the remote",
		},
		{
			name: "unsupported architecture fails", release: true, published: true,
			uname: []string{"Linux", "riscv64"}, wantSSH: 1,
			wantErr: "install agentsview on the remote",
		},
		{
			name: "unsupported platform still uses a matching PATH binary", release: true,
			uname: []string{"FreeBSD", "amd64"}, pathProtocol: remotesync.ProtocolVersion,
			wantBinary: "path", wantSSH: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newProvisionEnv(t)
			version := "dev"
			if tt.release {
				version = "v0.45.0"
			}
			build := LocalBuild{
				Version:    version,
				Executable: writeExecutable(t, t.TempDir(), "agentsview", fakeAgentsviewScript),
			}
			contentID, err := build.ContentID()
			require.NoError(t, err)
			ids := map[string]string{"release": "v0.45.0", "content": contentID}
			scripts := map[string]string{
				"release": releaseAgentsviewScript, "content": fakeAgentsviewScript,
			}
			for _, kind := range tt.cached {
				env.installCached(t, ids[kind], scripts[kind])
			}
			var pathDirs []string
			if tt.uname != nil {
				dir := t.TempDir()
				writeExecutable(t, dir, "uname", fakeUnameScript(tt.uname[0], tt.uname[1]))
				pathDirs = append(pathDirs, dir)
			}
			if tt.pathProtocol != 0 {
				dir := t.TempDir()
				writeExecutable(t, dir, "agentsview", pathAgentsviewScript(tt.pathProtocol))
				pathDirs = append(pathDirs, dir)
			}
			env.setRemotePath(t, pathDirs...)
			srv, hits := provisionReleaseServer(t, tt.published)
			p := env.provisioner(build, srv.URL, t.TempDir())
			before := env.binEntries(t)

			bin, err := p.Ensure(t.Context())

			assert.Equal(t, tt.wantSSH, env.sshCalls(t), "ssh calls")
			assert.Equal(t, tt.wantHTTP, hits.Load(), "release server requests")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.False(t, remotesync.IsHostUnavailable(err), "ssh reached the host")
				assert.Contains(t, remotesync.FailureSummary(err), tt.wantErr,
					"the summary keeps the remedy")
				assert.Equal(t, before, env.binEntries(t), "nothing installed")
				return
			}
			require.NoError(t, err)
			if tt.wantBinary == "path" {
				assert.Equal(t, pathBinary, bin)
				assert.Equal(t, before, env.binEntries(t), "nothing installed")
				return
			}
			id := ids[tt.wantBinary]
			assert.Equal(t, cachedBinary(id), bin)
			installed := filepath.Join(env.binDir(), id, "agentsview")
			info, err := os.Stat(installed)
			require.NoError(t, err)
			assert.NotZero(t, info.Mode()&0o111, "installed binary is executable")
			got, err := os.ReadFile(installed)
			require.NoError(t, err)
			assert.Equal(t, scripts[tt.wantBinary], string(got))
			lastUsed, err := os.Stat(filepath.Join(env.binDir(), id, ".last-used"))
			require.NoError(t, err)
			assert.WithinDuration(t, time.Now(), lastUsed.ModTime(), time.Hour,
				".last-used is refreshed")
			assert.Equal(t, remotesync.ProtocolVersion, probeThrough(t, p, bin).ProtocolVersion)
		})
	}
}

func TestProvisionReleaseServerFailureIsNotAnOfflineHost(t *testing.T) {
	env := newProvisionEnv(t)
	build := LocalBuild{
		Version:    "v0.45.0",
		Executable: writeExecutable(t, t.TempDir(), "agentsview", fakeAgentsviewScript),
	}
	// A closed server refuses connections, the error an unreachable host
	// gives. ssh reached the remote, so the sync must report a failure.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	p := env.provisioner(build, srv.URL, t.TempDir())

	_, err := p.Ensure(t.Context())

	require.Error(t, err)
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
	assert.False(t, remotesync.IsHostUnavailable(err))
	assert.Contains(t, remotesync.FailureSummary(err),
		"SSH remote sync failed: could not install agentsview on the remote")
}

func TestProvisionProbesPathBinaryOnlyWhenItsVersionIsSafe(t *testing.T) {
	tests := []struct {
		version   string
		wantProbe bool
	}{
		{version: "v0.22.2", wantProbe: false},
		{version: "0.22.2", wantProbe: false},
		{version: "v0.9.1", wantProbe: false},
		{version: "v0.2.2", wantProbe: false},
		{version: "dev", wantProbe: false},
		{version: "0123abc", wantProbe: false},
		{version: "8e848395b", wantProbe: false},
		{version: "1a2b3c4", wantProbe: false},
		{version: "v0.23.0", wantProbe: true},
		{version: "0.23.0", wantProbe: true},
		{version: "v0.45.0", wantProbe: true},
		{version: "v0.45.0-3-g0123abc-dirty", wantProbe: true},
		{version: "v0.100.0", wantProbe: true},
		{version: "v1.0.0", wantProbe: true},
		{version: "1.0.0", wantProbe: true},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			env := newProvisionEnv(t)
			build := LocalBuild{
				Version:    "dev",
				Executable: writeExecutable(t, t.TempDir(), "agentsview", fakeAgentsviewScript),
			}
			contentID, err := build.ContentID()
			require.NoError(t, err)
			dir := t.TempDir()
			writeExecutable(t, dir, "agentsview",
				versionedAgentsviewScript(tt.version, remotesync.ProtocolVersion))
			env.setRemotePath(t, dir)
			srv, _ := provisionReleaseServer(t, false)

			bin, err := env.provisioner(build, srv.URL, t.TempDir()).Ensure(t.Context())

			require.NoError(t, err)
			probes := filepath.Join(env.fake.Home, "path-probes")
			if tt.wantProbe {
				assert.FileExists(t, probes)
				assert.Equal(t, pathBinary, bin)
				return
			}
			assert.NoFileExists(t, probes, "an old agentsview would start its server")
			assert.Equal(t, cachedBinary(contentID), bin, "uploads its own build instead")
		})
	}
}

func TestProvisionReportsProgressOnlyForAnUpload(t *testing.T) {
	tests := []struct {
		name   string
		cached bool
		want   []string
	}{
		{name: "upload", want: []string{"Installing agentsview on devbox"}},
		{name: "cached binary", cached: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newProvisionEnv(t)
			build := LocalBuild{
				Version:    "dev",
				Executable: writeExecutable(t, t.TempDir(), "agentsview", fakeAgentsviewScript),
			}
			if tt.cached {
				contentID, err := build.ContentID()
				require.NoError(t, err)
				env.installCached(t, contentID, fakeAgentsviewScript)
			}
			srv, _ := provisionReleaseServer(t, false)
			p := env.provisioner(build, srv.URL, t.TempDir())
			p.Host = "devbox"
			var details []string
			p.Progress = func(detail string) { details = append(details, detail) }

			_, err := p.Ensure(t.Context())

			require.NoError(t, err)
			assert.Equal(t, tt.want, details)
		})
	}
}

func TestProvisionDistinctDevelopmentBuilds(t *testing.T) {
	env := newProvisionEnv(t)
	srv, hits := provisionReleaseServer(t, false)
	dataDir := t.TempDir()
	var ids []string
	for _, body := range []string{fakeAgentsviewScript, fakeAgentsviewScript + "# rebuilt\n"} {
		build := LocalBuild{
			Version:    "dev",
			Executable: writeExecutable(t, t.TempDir(), "agentsview", body),
		}
		id, err := build.ContentID()
		require.NoError(t, err)
		ids = append(ids, id)

		bin, err := env.provisioner(build, srv.URL, dataDir).Ensure(t.Context())

		require.NoError(t, err)
		assert.Equal(t, cachedBinary(id), bin)
		got, err := os.ReadFile(filepath.Join(env.binDir(), id, "agentsview"))
		require.NoError(t, err)
		assert.Equal(t, body, string(got))
	}
	assert.NotEqual(t, ids[0], ids[1])
	assert.ElementsMatch(t, ids, env.binEntries(t))
	assert.Equal(t, 4, env.sshCalls(t))
	assert.Zero(t, hits.Load())
}

func TestProvisionRetention(t *testing.T) {
	env := newProvisionEnv(t)
	bin := env.binDir()
	for _, dir := range []string{"old-a", "old-b", ".tmp-stale", ".tmp-fresh"} {
		require.NoError(t, os.MkdirAll(filepath.Join(bin, dir), 0o755))
	}
	setAge(t, filepath.Join(bin, "old-a", ".last-used"), 40*24*time.Hour)
	setAge(t, filepath.Join(bin, "old-b", ".last-used"), 24*time.Hour)
	setAge(t, filepath.Join(bin, ".tmp-stale"), 48*time.Hour)
	p := env.provisioner(LocalBuild{Version: "dev"}, "", t.TempDir())

	_, err := p.installTarball(t.Context(), "dev-0123456789abcdef",
		writeTarball(t, tarEntry{"agentsview", fakeAgentsviewScript}))

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"dev-0123456789abcdef", "old-b", ".tmp-fresh"},
		env.binEntries(t))
}

func TestProvisionSkipsACachedBinaryItCannotRefresh(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping: root can write into read-only directories")
	}
	env := newProvisionEnv(t)
	build := LocalBuild{
		Version:    "dev",
		Executable: writeExecutable(t, t.TempDir(), "agentsview", fakeAgentsviewScript),
	}
	contentID, err := build.ContentID()
	require.NoError(t, err)
	env.installCached(t, contentID, fakeAgentsviewScript)
	dir := filepath.Join(env.binDir(), contentID)
	require.NoError(t, os.Remove(filepath.Join(dir, ".last-used")))
	// Without a refreshed .last-used, retention may remove the binary later,
	// so the probe must not select it.
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, 0o755)) })
	pathDir := t.TempDir()
	writeExecutable(t, pathDir, "agentsview", pathAgentsviewScript(remotesync.ProtocolVersion))
	env.setRemotePath(t, pathDir)
	srv, _ := provisionReleaseServer(t, false)
	p := env.provisioner(build, srv.URL, t.TempDir())

	bin, err := p.Ensure(t.Context())

	require.NoError(t, err)
	assert.Equal(t, pathBinary, bin)
}

func TestProvisionRetentionFailureKeepsTheInstall(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping: root can remove read-only directories")
	}
	env := newProvisionEnv(t)
	bin := env.binDir()
	// A read-only directory inside makes these impossible to remove.
	for _, dir := range []string{"old-locked", ".tmp-locked"} {
		locked := filepath.Join(bin, dir, "locked")
		require.NoError(t, os.MkdirAll(locked, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(locked, "file"), nil, 0o644))
		require.NoError(t, os.Chmod(locked, 0o555))
		t.Cleanup(func() { assert.NoError(t, os.Chmod(locked, 0o755)) })
	}
	setAge(t, filepath.Join(bin, "old-locked", ".last-used"), 40*24*time.Hour)
	setAge(t, filepath.Join(bin, ".tmp-locked"), 48*time.Hour)
	p := env.provisioner(LocalBuild{Version: "dev"}, "", t.TempDir())
	const id = "dev-0123456789abcdef"

	got, err := p.installTarball(t.Context(), id,
		writeTarball(t, tarEntry{"agentsview", fakeAgentsviewScript}))

	require.NoError(t, err)
	assert.Equal(t, cachedBinary(id), got)
	assert.FileExists(t, filepath.Join(bin, "old-locked", "locked", "file"),
		"the stale directory could not be removed")
	assert.FileExists(t, filepath.Join(bin, ".tmp-locked", "locked", "file"),
		"the stale temp directory could not be removed")
	assert.Equal(t, remotesync.ProtocolVersion, probeThrough(t, p, got).ProtocolVersion)
}

func TestProvisionInstallFailures(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarEntry
		wantErr string
	}{
		{
			name:    "archive without agentsview",
			entries: []tarEntry{{"README", "not a binary\n"}},
			wantErr: "archive has no agentsview executable",
		},
		{
			name:    "binary whose probe fails",
			entries: []tarEntry{{"agentsview", failingAgentsviewScript}},
			wantErr: "ssh exited",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newProvisionEnv(t)
			p := env.provisioner(LocalBuild{Version: "dev"}, "", t.TempDir())

			_, err := p.installTarball(t.Context(), "dev-0123456789abcdef",
				writeTarball(t, tt.entries...))

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NoFileExists(t,
				filepath.Join(env.binDir(), "dev-0123456789abcdef", "agentsview"))
			assert.Empty(t, env.binEntries(t), "no temp directory is left")
		})
	}
}

func TestProvisionConcurrentInstallsOfOneIdentity(t *testing.T) {
	env := newProvisionEnv(t)
	p := env.provisioner(LocalBuild{Version: "dev"}, "", t.TempDir())
	tarball := writeTarball(t, tarEntry{"agentsview", fakeAgentsviewScript})
	const id = "dev-0123456789abcdef"

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = p.installTarball(t.Context(), id, tarball)
		})
	}
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, []string{id}, env.binEntries(t), "no temp directory is left")
	assert.Equal(t, remotesync.ProtocolVersion,
		probeThrough(t, p, cachedBinary(id)).ProtocolVersion)
}

func TestProvisionScriptsRejectInvalidIdentity(t *testing.T) {
	for _, id := range []string{"", "v1 2", "a/b", "$(id)", `a"b`, "a;b"} {
		t.Run(id, func(t *testing.T) {
			_, err := probeScript([]string{"v0.45.0", id})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid remote binary identity")
			_, err = installScript(id)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid remote binary identity")
		})
	}
}

func TestProvisionScriptsAreValidRemoteScripts(t *testing.T) {
	// Both builders return validateScript's verdict as their error.
	_, err := probeScript([]string{"v0.45.0", "v0.45.0-0123456789abcdef"})
	require.NoError(t, err)
	_, err = installScript("dev+dirty-0123456789abcdef")
	require.NoError(t, err)
}

func TestParseProbeIgnoresUnrelatedOutput(t *testing.T) {
	out := []byte("Welcome to devbox\n" +
		"os=Linux\narch=aarch64\n" +
		"cached v0.45.0 {\"version\":\"0.45.0\",\"protocol_version\":2}\n" +
		"cached broken not-json\n" +
		"path {\"version\":\"0.44.0\",\"protocol_version\":1}\n")

	report, err := parseProbe(out)

	require.NoError(t, err)
	remote, err := report.platform()
	require.NoError(t, err)
	assert.Equal(t, platform{OS: "linux", Arch: "arm64"}, remote)
	assert.Equal(t, map[string]ProbeResult{"v0.45.0": {Version: "0.45.0", ProtocolVersion: 2}},
		report.cached)
	require.NotNil(t, report.path)
	assert.Equal(t, ProbeResult{Version: "0.44.0", ProtocolVersion: 1}, *report.path)
}

func TestProvisionUnreachableHostStaysOffline(t *testing.T) {
	sshPath := newFailingSSH(t,
		"ssh: connect to host 192.0.2.10 port 22: Connection refused\n", 255)
	p := Provisioner{
		Runner: Runner{Destination: "devbox.example.test", SSHPath: sshPath},
		Build: LocalBuild{
			Version:    "dev",
			Executable: writeExecutable(t, t.TempDir(), "agentsview", fakeAgentsviewScript),
		},
		DataDir: t.TempDir(),
	}

	_, err := p.Ensure(t.Context())

	require.Error(t, err)
	assert.True(t, remotesync.IsHostUnavailable(err))
}
