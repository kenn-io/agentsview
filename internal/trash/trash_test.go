package trash

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T, impl backend) *Store {
	t.Helper()
	dataDir := t.TempDir()
	s := &Store{dataDir: dataDir, impl: impl, now: time.Now}
	require.NoError(t, impl.prepare())
	require.NoError(t, os.MkdirAll(s.manifestDir(), 0o755))
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// exdevBackend is a system-style backend whose rename always reports
// a cross-device error, driving the fallback path.
type exdevBackend struct {
	root string
}

func (b exdevBackend) name() string                          { return BackendDarwin }
func (b exdevBackend) prepare() error                        { return nil }
func (b exdevBackend) move(src, name string) (string, error) { return "", syscall.EXDEV }
func (b exdevBackend) cleanup(trashedPath string) error      { return nil }
func (b exdevBackend) destRoot() string                      { return b.root }

func TestTrashMovesFileAndRecordsManifest(t *testing.T) {
	s := newTestStore(t, fallbackBackend{dataDir: t.TempDir()})
	src := filepath.Join(t.TempDir(), "sessions", "a.jsonl")
	writeFile(t, src, "hello")

	items, err := s.Trash([]string{src}, []Meta{{SessionID: "s1", Agent: "codex"}})
	require.NoError(t, err)
	require.Len(t, items, 1)

	item := items[0]
	assert.Equal(t, BackendFallback, item.Backend)
	assert.Equal(t, "s1", item.SessionID)
	assert.Equal(t, "codex", item.Agent)
	assert.Equal(t, int64(len("hello")), item.Size)
	assert.NoFileExists(t, src)
	assert.FileExists(t, item.TrashedPath)

	listed, err := s.List()
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, item.ID, listed[0].ID)
	assert.Equal(t, src, listed[0].OriginalPath)
}

func TestTrashNameCollisionGetsSuffix(t *testing.T) {
	s := newTestStore(t, fallbackBackend{dataDir: t.TempDir()})

	first := filepath.Join(t.TempDir(), "a.jsonl")
	second := filepath.Join(t.TempDir(), "nested", "a.jsonl")
	writeFile(t, first, "one")
	writeFile(t, second, "two")

	items, err := s.Trash([]string{first, second}, nil)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "a.jsonl", filepath.Base(items[0].TrashedPath))
	assert.Equal(t, "a (2).jsonl", filepath.Base(items[1].TrashedPath))
}

func TestTrashCrossDeviceFallsBack(t *testing.T) {
	s := newTestStore(t, exdevBackend{root: t.TempDir()})
	src := filepath.Join(t.TempDir(), "a.jsonl")
	writeFile(t, src, "data")

	items, err := s.Trash([]string{src}, nil)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, BackendFallback, items[0].Backend)
	assert.NoFileExists(t, src)
	assert.FileExists(t, items[0].TrashedPath)
}

func TestTrashMissingPathIsReportedNotFatal(t *testing.T) {
	s := newTestStore(t, fallbackBackend{dataDir: t.TempDir()})
	present := filepath.Join(t.TempDir(), "ok.jsonl")
	writeFile(t, present, "x")
	missing := filepath.Join(t.TempDir(), "gone.jsonl")

	items, err := s.Trash([]string{missing, present}, nil)
	require.Error(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, present, items[0].OriginalPath)
}

func TestRestoreLastMovesBackAndClearsManifest(t *testing.T) {
	s := newTestStore(t, fallbackBackend{dataDir: t.TempDir()})
	src := filepath.Join(t.TempDir(), "proj", "session.jsonl")
	writeFile(t, src, "payload")

	items, err := s.Trash([]string{src}, []Meta{{SessionID: "s9"}})
	require.NoError(t, err)
	require.Len(t, items, 1)

	restored, err := s.RestoreLast()
	require.NoError(t, err)
	require.Len(t, restored, 1)
	assert.Equal(t, "s9", restored[0].SessionID)
	assert.FileExists(t, src)
	assert.NoFileExists(t, items[0].TrashedPath)

	listed, err := s.List()
	require.NoError(t, err)
	assert.Empty(t, listed)
}

func TestRestoreWhenOriginalPathOccupied(t *testing.T) {
	s := newTestStore(t, fallbackBackend{dataDir: t.TempDir()})
	src := filepath.Join(t.TempDir(), "session.jsonl")
	writeFile(t, src, "old")

	_, err := s.Trash([]string{src}, nil)
	require.NoError(t, err)

	// The user recreated a file at the original path before restore.
	writeFile(t, src, "new content")

	restored, err := s.RestoreLast()
	require.NoError(t, err)
	require.Len(t, restored, 1)

	// The trashed payload comes back under a restored- sibling; the
	// user's file is never overwritten.
	entries, err := os.ReadDir(filepath.Dir(src))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	require.Len(t, names, 2)
	assert.Contains(t, names, "session.jsonl")
	foundRestored := false
	for _, name := range names {
		if strings.HasPrefix(name, "session.jsonl.restored-") {
			foundRestored = true
			payload, readErr := os.ReadFile(filepath.Join(filepath.Dir(src), name))
			require.NoError(t, readErr)
			assert.Equal(t, "old", string(payload))
		}
	}
	assert.True(t, foundRestored, "restored sibling missing; got %v", names)

	newContent, err := os.ReadFile(src)
	require.NoError(t, err)
	assert.Equal(t, "new content", string(newContent))
}

func TestRestoreBatchIsolatesBatches(t *testing.T) {
	s := newTestStore(t, fallbackBackend{dataDir: t.TempDir()})
	first := filepath.Join(t.TempDir(), "one.jsonl")
	second := filepath.Join(t.TempDir(), "two.jsonl")
	writeFile(t, first, "1")
	writeFile(t, second, "2")

	_, err := s.Trash([]string{first}, nil)
	require.NoError(t, err)
	items, err := s.Trash([]string{second}, nil)
	require.NoError(t, err)
	require.Len(t, items, 1)

	restored, err := s.RestoreBatch(items[0].BatchID)
	require.NoError(t, err)
	require.Len(t, restored, 1)
	assert.Equal(t, second, restored[0].OriginalPath)
	assert.FileExists(t, second)
	assert.NoFileExists(t, first, "first batch must stay trashed")
}

func TestRestoreMissingTrashedFileReportsNotFound(t *testing.T) {
	s := newTestStore(t, fallbackBackend{dataDir: t.TempDir()})
	src := filepath.Join(t.TempDir(), "session.jsonl")
	writeFile(t, src, "x")
	items, err := s.Trash([]string{src}, nil)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.NoError(t, os.Remove(items[0].TrashedPath))

	restored, err := s.RestoreLast()
	assert.Empty(t, restored)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "want ErrNotFound, got %v", err)
}

func TestManifestSurvivesCorruptTail(t *testing.T) {
	s := newTestStore(t, fallbackBackend{dataDir: t.TempDir()})
	src := filepath.Join(t.TempDir(), "a.jsonl")
	writeFile(t, src, "x")
	items, err := s.Trash([]string{src}, nil)
	require.NoError(t, err)
	require.Len(t, items, 1)

	// Simulate a crash mid-append: garbage after the last newline.
	f, err := os.OpenFile(s.manifestPath(), os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"id":"trunc`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	listed, err := s.List()
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, items[0].ID, listed[0].ID)
}

func TestManifestRoundTripTimestamps(t *testing.T) {
	s := newTestStore(t, fallbackBackend{dataDir: t.TempDir()})
	src := filepath.Join(t.TempDir(), "a.jsonl")
	writeFile(t, src, "x")
	items, err := s.Trash([]string{src}, nil)
	require.NoError(t, err)
	require.Len(t, items, 1)

	listed, err := s.List()
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.WithinDuration(t, items[0].DeletedAt, listed[0].DeletedAt, time.Second)
	assert.Contains(t, listed[0].ID, items[0].BatchID)
}

func TestFreedesktopTrashInfoRoundTrip(t *testing.T) {
	encoded := trashinfoPath("/home/user/my dir/a.jsonl")
	decoded, err := parseTrashInfoPath(encoded)
	require.NoError(t, err)
	assert.Equal(t, "/home/user/my dir/a.jsonl", decoded)

	_, err = parseTrashInfoPath("relative/path")
	assert.Error(t, err)
}
