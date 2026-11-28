package remotesync

import (
	"os"
	"path/filepath"
	"testing"
)

// A multi-GB state.db snapshot must not default to /tmp, which is often a small
// tmpfs; with TMPDIR unset it goes under the user cache dir. An explicit TMPDIR wins.
func TestSQLiteSnapshotTempRoot(t *testing.T) {
	cache := t.TempDir()
	// os.UserCacheDir reads XDG_CACHE_HOME on Linux, $HOME on macOS and
	// LocalAppData on Windows; point all of them at the temp dir.
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t.Setenv("LocalAppData", cache)
	t.Setenv("TMPDIR", "")
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("user cache dir: %v", err)
	}
	want := filepath.Join(base, "agentsview", "tmp")
	if got := sqliteSnapshotTempRoot(); got != want {
		t.Fatalf("TMPDIR unset: got %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("cache tmp dir not created: %v", err)
	}
	explicit := t.TempDir()
	t.Setenv("TMPDIR", explicit)
	if got := sqliteSnapshotTempRoot(); got != "" {
		t.Fatalf("explicit TMPDIR must be honoured via os.TempDir, got %q", got)
	}
}
