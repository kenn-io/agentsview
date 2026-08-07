package parser

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// autofsTestsSupported reports whether the running OS uses POSIX
// path separators, which the autofs prefix matching depends on.
// Windows-normalised paths (\home\...) would not match the /home/
// prefix form autofs reports, and Windows has no autofs in the
// first place, so these tests are skipped there.
func autofsTestsSupported() bool {
	return runtime.GOOS != "windows"
}

// TestExtractProjectFromCwd_AutofsUnresolved_SkipsWalk verifies
// that a cwd under an autofs prefix whose first component does
// not resolve locally (the classic "Linux cwd on a macOS box"
// case) pays for exactly one stat — the probe — and skips the
// full git-root walk. Without the skip, statting every ancestor
// hammers automountd/opendirectoryd via /usr/libexec/od_user_homes.
func TestExtractProjectFromCwd_AutofsUnresolved_SkipsWalk(t *testing.T) {
	if !autofsTestsSupported() {
		t.Skip("autofs tests require POSIX path separators")
	}
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return nil, os.ErrNotExist
	}

	cwd := "/home/wes/code/example-project"
	got := ExtractProjectFromCwdWithBranch(cwd, "")
	assert.Equal(t, "example_project", got,
		"ExtractProjectFromCwdWithBranch(%q)", cwd)
	assert.Equal(t, int64(1), count.Load(),
		"osStat call count for unresolved autofs cwd "+
			"%q; expected 1 (probe only, walk skipped)", cwd)
}

// TestExtractProjectFromCwd_AutofsUnresolved_ProbeCached checks
// that a bulk sync with many cwds under the same autofs first
// component pays for only one stat across the batch.
func TestExtractProjectFromCwd_AutofsUnresolved_ProbeCached(t *testing.T) {
	if !autofsTestsSupported() {
		t.Skip("autofs tests require POSIX path separators")
	}
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return nil, os.ErrNotExist
	}

	for _, cwd := range []string{
		"/home/wes/code/proj-a",
		"/home/wes/code/proj-b",
		"/home/wes/code/nested/proj-c/src",
	} {
		_ = ExtractProjectFromCwdWithBranch(cwd, "")
	}
	assert.Equal(t, int64(1), count.Load(),
		"osStat calls across 3 cwds sharing /home/wes; expected 1 (cached probe)")
}

// TestExtractProjectFromCwd_AutofsResolved_Walks is the regression
// guard for the reviewer's concern: enterprise hosts where an
// autofs prefix has a real backing (e.g. NFS-mounted /home) must
// still resolve projects to their repository roots. A resolving
// probe lets the git-root walk proceed normally.
func TestExtractProjectFromCwd_AutofsResolved_Walks(t *testing.T) {
	if !autofsTestsSupported() {
		t.Skip("autofs tests require POSIX path separators")
	}
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()

	// Use a real directory's FileInfo so IsDir() answers true
	// when the probe "resolves".
	realDir := t.TempDir()
	realInfo, err := os.Stat(realDir)
	require.NoError(t, err)

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		if path == "/home/wes" {
			return realInfo, nil
		}
		return nil, os.ErrNotExist
	}

	cwd := "/home/wes/code/example"
	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	assert.GreaterOrEqual(t, count.Load(), int64(2),
		"with resolving autofs probe, expected the walk "+
			"to stat multiple paths (probe + walk)")
}

// TestExtractProjectFromCwd_NativePath_StillWalks confirms that
// paths outside any autofs-managed prefix still trigger the
// git-root walk.
func TestExtractProjectFromCwd_NativePath_StillWalks(t *testing.T) {
	if !autofsTestsSupported() {
		t.Skip("autofs tests require POSIX path separators")
	}
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return orig(path)
	}

	cwd := "/Users/nobody-agentsview-test/code/example"
	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	assert.NotZero(t, count.Load(),
		"osStat never called for %q; "+
			"git-root walk should run for non-autofs paths", cwd)
}

// TestExtractProjectFromCwd_HomePathWithoutAutofs_StillWalks covers
// the edge case flagged in review: a user with a real filesystem
// mounted at /home (no autofs entry) should still get git-root
// resolution, not a basename-only fallback.
func TestExtractProjectFromCwd_HomePathWithoutAutofs_StillWalks(t *testing.T) {
	if !autofsTestsSupported() {
		t.Skip("autofs tests require POSIX path separators")
	}
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = nil
	resetAutofsProbes()

	realDir := t.TempDir()
	realInfo, err := os.Stat(realDir)
	require.NoError(t, err)

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	cwd := "/home/nobody-agentsview-test/code/example"
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		if path == cwd {
			return realInfo, nil
		}
		return nil, os.ErrNotExist
	}

	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	assert.GreaterOrEqual(t, count.Load(), int64(2),
		"osStat never called for /home path with empty "+
			"autofs config; walk must proceed for a real mount")
}

// TestParseMountOutputForAutofs exercises the pure mount-output
// parser. It runs on every platform because it operates on the
// string form of `mount`'s output.
func TestParseMountOutputForAutofs(t *testing.T) {
	// Representative macOS output mixing real filesystems with
	// autofs entries; includes the /System/Volumes/Data firmlink
	// prefix that must be stripped.
	input := []byte(`/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
devfs on /dev (devfs, local, nobrowse)
/dev/disk3s6 on /System/Volumes/VM (apfs, local, noexec, journaled, nobrowse, sealed)
map auto_home on /System/Volumes/Data/home (autofs, automounted, nobrowse)
map -hosts on /System/Volumes/Data/net (autofs, automounted, nobrowse)
map -fstab on /System/Volumes/Data/Network/Servers (autofs, automounted, nobrowse)
server.example:/export on /corp/home (autofs, nobrowse)
`)
	got := parseMountOutputForAutofs(input)
	want := []string{
		"/home/",
		"/net/",
		"/Network/Servers/",
		"/corp/home/",
	}
	assert.Equal(t, want, got, "parseMountOutputForAutofs()")
}

// TestDetectAutofsPrefixes uses the injectable mount source so the
// end-to-end detection path runs without actually invoking mount(8).
func TestDetectAutofsPrefixes(t *testing.T) {
	origSrc := autofsMountSource
	defer func() { autofsMountSource = origSrc }()
	autofsMountSource = func() ([]byte, error) {
		return []byte(
			"map auto_home on /System/Volumes/Data/home " +
				"(autofs, automounted, nobrowse)\n" +
				"map -fstab on /System/Volumes/Data/Network/Servers " +
				"(autofs, automounted, nobrowse)\n",
		), nil
	}

	got := detectAutofsPrefixes()
	if runtime.GOOS != "darwin" {
		assert.Nil(t, got, "detectAutofsPrefixes() on %s", runtime.GOOS)
		return
	}
	want := []string{"/home/", "/Network/Servers/"}
	assert.Equal(t, want, got, "detectAutofsPrefixes()")
}

// TestExtractProjectFromCwd_AutofsConcurrent_SingleProbe exercises
// the single-flight guarantee: N goroutines hitting the same
// unresolved autofs prefix must share a single probe, not race
// each other into N duplicate automount attempts.
func TestExtractProjectFromCwd_AutofsConcurrent_SingleProbe(t *testing.T) {
	if !autofsTestsSupported() {
		t.Skip("autofs tests require POSIX path separators")
	}
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()

	// Hold osStat open so later callers have a chance to pile
	// up against the first one. Without single-flight all of
	// them will issue their own stat before the cache populates.
	release := make(chan struct{})
	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		<-release
		return nil, os.ErrNotExist
	}

	const workers = 16
	var started sync.WaitGroup
	var done sync.WaitGroup
	started.Add(workers)
	done.Add(workers)
	for range workers {
		go func() {
			defer done.Done()
			started.Done()
			_ = ExtractProjectFromCwdWithBranch(
				"/home/wes/code/proj", "")
		}()
	}
	started.Wait()
	// Give the workers a moment to actually enter the probe.
	time.Sleep(50 * time.Millisecond)
	close(release)
	done.Wait()

	assert.Equal(t, int64(1), count.Load(),
		"concurrent probes issued osStat calls; "+
			"expected exactly 1 under %d-way concurrency", workers)
}

// TestExtractProjectFromCwd_AutofsProbe_TTLExpires guards against
// permanently caching a negative probe result. A path that was
// unreachable at first access should be re-probed after the TTL
// so a later-appearing mount (e.g. delayed NFS availability on a
// long-running server) can start resolving to its repository
// root.
func TestExtractProjectFromCwd_AutofsProbe_TTLExpires(t *testing.T) {
	if !autofsTestsSupported() {
		t.Skip("autofs tests require POSIX path separators")
	}
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()

	origNow := nowFn
	defer func() { nowFn = origNow }()
	current := time.Now()
	nowFn = func() time.Time { return current }

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return nil, os.ErrNotExist
	}

	cwd := "/home/wes/code/proj"
	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	require.Equal(t, int64(1), count.Load(), "first call")

	// Within TTL — cached, no new probe.
	current = current.Add(autofsProbeTTL / 2)
	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	require.Equal(t, int64(1), count.Load(), "within TTL")

	// Past TTL — re-probe.
	current = current.Add(autofsProbeTTL + time.Second)
	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	assert.Equal(t, int64(2), count.Load(), "past TTL")
}

// TestDetectAutofsPrefixes_MountFails confirms that a mount(8)
// failure degrades to nil rather than crashing or blocking the
// git-root walk on unrelated paths.
func TestDetectAutofsPrefixes_MountFails(t *testing.T) {
	origSrc := autofsMountSource
	defer func() { autofsMountSource = origSrc }()
	autofsMountSource = func() ([]byte, error) {
		return nil, errors.New("mock mount failure")
	}
	assert.Nil(t, detectAutofsPrefixes(), "detectAutofsPrefixes() with mount failure")
}
