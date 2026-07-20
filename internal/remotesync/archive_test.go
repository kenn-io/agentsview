package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestWriteArchivePreservesRootRelativePathAndMTime(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "home", "wes", ".claude")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("body"), 0o644))
	wantMTime := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(path, wantMTime, wantMTime))

	var buf bytes.Buffer
	err := WriteArchive(&buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
	})
	require.NoError(t, err)

	tr := tar.NewReader(&buf)
	var found *tar.Header
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Name == archiveNameForTest(t, path) {
			found = hdr
			break
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, byte(tar.TypeReg), found.Typeflag)
	assert.Equal(t, wantMTime.Unix(), found.ModTime.Unix())
}

func TestWriteArchiveDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o644))
	link := filepath.Join(dir, "link.jsonl")
	require.NoError(t, os.Symlink(target, link))

	var buf bytes.Buffer
	require.NoError(t, WriteArchive(&buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
	}))

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		assert.NotEqual(t, archiveNameForTest(t, link), hdr.Name)
	}
}

func TestWriteArchiveIgnoresBytesAppendedAfterHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	writer := newBlockAfterFirstTarHeaderWriter()
	errCh := make(chan error, 1)
	go func() {
		errCh <- WriteArchive(writer, TargetSet{
			ExtraFiles: []string{path},
		})
	}()

	select {
	case <-writer.headerWritten:
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "tar header was not written")
	}
	require.NoError(t, appendFile(path, "new"))
	close(writer.proceed)

	require.NoError(t, <-errCh)
	tr := tar.NewReader(bytes.NewReader(writer.Bytes()))
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, archiveNameForTest(t, path), hdr.Name)
	body, err := io.ReadAll(tr)
	require.NoError(t, err)
	assert.Equal(t, "old", string(body))
}

func TestWriteArchiveToleratesMissingExtraFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	missing := filepath.Join(dir, "missing.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("body"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, WriteArchive(&buf, TargetSet{
		ExtraFiles: []string{path, missing},
	}))
	archiveBytes := slices.Clone(buf.Bytes())

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, archiveNameForTest(t, path), hdr.Name)
	_, err = io.ReadAll(tr)
	require.NoError(t, err)
	_, err = tr.Next()
	assert.ErrorIs(t, err, io.EOF)
	assert.True(t, hasTarEndMarker(archiveBytes))
}

func TestWriteArchiveSkipsDirectoryValuedExtraFile(t *testing.T) {
	root := t.TempDir()
	extraDir := filepath.Join(root, "state.db-wal")
	require.NoError(t, os.Mkdir(extraDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(extraDir, "credential.txt"), []byte("secret"), 0o600,
	))

	var buf bytes.Buffer
	require.NoError(t, WriteArchive(&buf, TargetSet{
		ExtraFiles: []string{extraDir},
	}))

	_, err := tar.NewReader(&buf).Next()
	assert.ErrorIs(t, err, io.EOF)
}

func TestHermesArchivesSnapshotWALCommitBeforeCheckpoint(t *testing.T) {
	tests := []struct {
		name  string
		write func(io.Writer, string) error
	}{
		{
			name: "full archive",
			write: func(w io.Writer, stateDB string) error {
				return WriteArchive(w, TargetSet{
					Dirs: map[parser.AgentType][]string{
						parser.AgentHermes: {stateDB},
					},
					ExtraFiles: hermesTestSidecars(stateDB),
				})
			},
		},
		{
			name: "delta archive",
			write: func(w io.Writer, stateDB string) error {
				wal := stateDB + "-wal"
				allowed := TargetSet{
					Dirs: map[parser.AgentType][]string{
						parser.AgentHermes: {stateDB},
					},
					ExtraFiles: hermesTestSidecars(stateDB),
				}
				return WriteArchiveFiles(w, allowed, []string{stateDB, wal})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDB := filepath.Join(t.TempDir(), "profile", "state.db")
			require.NoError(t, os.MkdirAll(filepath.Dir(stateDB), 0o755))
			writeHermesImportStateDB(t, stateDB)
			writer, err := sql.Open("sqlite3", stateDB)
			require.NoError(t, err)
			t.Cleanup(func() { _ = writer.Close() })
			var journalMode string
			require.NoError(t,
				writer.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode))
			assert.Equal(t, "wal", journalMode)
			_, err = writer.Exec(`PRAGMA wal_autocheckpoint = 0`)
			require.NoError(t, err)
			_, err = writer.Exec(`
				UPDATE sessions
				SET title = 'Committed in WAL'
				WHERE id = 'database-only'
			`)
			require.NoError(t, err)
			wal := stateDB + "-wal"
			require.FileExists(t, wal)

			stateInfo, err := os.Stat(stateDB)
			require.NoError(t, err)
			archiveWriter := newBlockAfterBytesWriter(512 + stateInfo.Size())
			errCh := make(chan error, 1)
			go func() { errCh <- tt.write(archiveWriter, stateDB) }()

			select {
			case <-archiveWriter.blocked:
			case <-time.After(backgroundWaitTimeout):
				require.FailNow(t, "archive did not finish its first database entry")
			}
			_, checkpointErr := writer.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
			require.NoError(t, checkpointErr)
			require.NoError(t, writer.Close())
			if removeErr := os.Remove(wal); !os.IsNotExist(removeErr) {
				require.NoError(t, removeErr)
			}
			close(archiveWriter.proceed)
			require.NoError(t, <-errCh)

			extracted := t.TempDir()
			_, err = ExtractTarStream(
				context.Background(), bytes.NewReader(archiveWriter.Bytes()), extracted,
			)
			require.NoError(t, err)
			extractedDB, err := safeRemappedRemotePath(extracted, stateDB)
			require.NoError(t, err)
			snapshot, err := sql.Open("sqlite3", extractedDB)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, snapshot.Close()) })
			var title string
			require.NoError(t, snapshot.QueryRow(`
				SELECT title FROM sessions WHERE id = 'database-only'
			`).Scan(&title))
			assert.Equal(t, "Committed in WAL", title)
			assert.NoFileExists(t, extractedDB+"-wal")
			assert.NoFileExists(t, extractedDB+"-shm")
		})
	}
}

func TestWriteArchivePropagatesAdvertisedHermesSnapshotFailure(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "profile", "state.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(stateDB), 0o755))
	writeHermesImportStateDB(t, stateDB)

	wantErr := errors.New("forced sqlite backup failure")
	originalSnapshot := writeHermesSnapshotFile
	writeHermesSnapshotFile = func(dstPath, srcPath string) error {
		assert.NotEmpty(t, dstPath)
		assert.Equal(t, stateDB, srcPath)
		return wantErr
	}
	t.Cleanup(func() { writeHermesSnapshotFile = originalSnapshot })

	var archive bytes.Buffer
	err := WriteArchive(&archive, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentHermes: {stateDB}},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Contains(t, err.Error(), "snapshot hermes database")
}

func hermesTestSidecars(stateDB string) []string {
	return []string{
		stateDB + "-wal",
		stateDB + "-shm",
		stateDB + "-journal",
	}
}

func archiveNameForTest(t *testing.T, p string) string {
	t.Helper()
	name, err := safeRemotePathArchiveName(p)
	require.NoError(t, err)
	return name
}

func TestExtractTarStreamRejectsArchiveWithoutEndMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	invalid := filepath.Join(dir, "invalid\x00path")
	require.NoError(t, os.WriteFile(path, []byte("body"), 0o644))

	var buf bytes.Buffer
	err := WriteArchive(&buf, TargetSet{
		ExtraFiles: []string{path, invalid},
	})
	require.Error(t, err)
	require.False(t, hasTarEndMarker(buf.Bytes()))

	_, err = ExtractTarStream(context.Background(), &buf, t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing tar end marker")
}

func TestExtractTarStreamRejectsArchiveEndingWithZeroFilePayload(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name: "home/wes/.claude/projects/zero.bin",
		Mode: 0o644,
		Size: 1024,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write(make([]byte, 1024))
	require.NoError(t, err)

	_, err = ExtractTarStream(context.Background(), &buf, t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing tar end marker")
}

func TestWriteArchiveFileSkipsDisappearedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("body"), 0o644))
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, writeArchiveFile(tw, path, info))
	require.NoError(t, tw.Close())
	assert.True(t, hasTarEndMarker(buf.Bytes()))
}

func hasTarEndMarker(data []byte) bool {
	const trailerSize = 1024
	if len(data) < trailerSize {
		return false
	}
	for _, b := range data[len(data)-trailerSize:] {
		if b != 0 {
			return false
		}
	}
	return true
}

type blockAfterFirstTarHeaderWriter struct {
	buf           bytes.Buffer
	headerWritten chan struct{}
	proceed       chan struct{}
	mu            sync.Mutex
	total         int
	blocked       bool
}

type blockAfterBytesWriter struct {
	buf     bytes.Buffer
	blocked chan struct{}
	proceed chan struct{}
	blockAt int64
	total   int64
	once    sync.Once
	mu      sync.Mutex
}

func newBlockAfterBytesWriter(blockAt int64) *blockAfterBytesWriter {
	return &blockAfterBytesWriter{
		blocked: make(chan struct{}),
		proceed: make(chan struct{}),
		blockAt: blockAt,
	}
}

func (w *blockAfterBytesWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.total += int64(n)
	shouldBlock := w.total >= w.blockAt
	w.mu.Unlock()
	if shouldBlock {
		w.once.Do(func() {
			close(w.blocked)
			<-w.proceed
		})
	}
	return n, err
}

func (w *blockAfterBytesWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.buf.Bytes())
}

func newBlockAfterFirstTarHeaderWriter() *blockAfterFirstTarHeaderWriter {
	return &blockAfterFirstTarHeaderWriter{
		headerWritten: make(chan struct{}),
		proceed:       make(chan struct{}),
	}
}

func (w *blockAfterFirstTarHeaderWriter) Write(p []byte) (int, error) {
	shouldBlock := false
	w.mu.Lock()
	if !w.blocked {
		w.total += len(p)
		if w.total >= 512 {
			w.blocked = true
			shouldBlock = true
			close(w.headerWritten)
		}
	}
	w.mu.Unlock()
	if shouldBlock {
		<-w.proceed
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *blockAfterFirstTarHeaderWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

func appendFile(path string, value string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(value)
	return err
}

func TestWriteArchivePreservesNanosecondMtime(t *testing.T) {
	srcDir := t.TempDir()
	path := filepath.Join(srcDir, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	mtime := time.Date(2026, 7, 8, 10, 30, 0, 123456789, time.UTC)
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	info, err := os.Stat(path)
	require.NoError(t, err)
	if info.ModTime().Nanosecond() == 0 {
		t.Skip("filesystem does not store nanosecond mtimes")
	}

	var buf bytes.Buffer
	require.NoError(t, WriteArchive(&buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {srcDir}},
	}))

	dstDir := t.TempDir()
	_, err = ExtractTarStream(context.Background(), &buf, dstDir)
	require.NoError(t, err)
	extracted, err := safeRemappedRemotePath(dstDir, path)
	require.NoError(t, err)
	extractedInfo, err := os.Stat(extracted)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime().UnixNano(), extractedInfo.ModTime().UnixNano())
}

func TestWriteArchiveFilesSkipsVanishedAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.jsonl")
	require.NoError(t, os.WriteFile(keep, []byte("k"), 0o644))
	link := filepath.Join(dir, "link.jsonl")
	require.NoError(t, os.Symlink(keep, link))
	gone := filepath.Join(dir, "gone.jsonl")

	var buf bytes.Buffer
	require.NoError(t, WriteArchiveFiles(&buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
	}, []string{gone, link, keep}))

	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
	require.Len(t, names, 1)
	assert.Contains(t, names[0], "keep.jsonl")
}

func TestWriteArchiveFilesSkipsFilesOutsideAllowedRoots(t *testing.T) {
	allowed := t.TempDir()
	inside := filepath.Join(allowed, "s.jsonl")
	require.NoError(t, os.WriteFile(inside, []byte("in"), 0o644))
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, WriteArchiveFiles(&buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {allowed}},
	}, []string{inside, outside}))

	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
	require.Len(t, names, 1, "only the file inside the allowed root is streamed")
	assert.Contains(t, names[0], "s.jsonl")
}

func TestWriteArchiveFilesPreservesNonHermesStateDB(t *testing.T) {
	root := t.TempDir()
	stateDB := filepath.Join(root, "nested", "state.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(stateDB), 0o755))
	require.NoError(t, os.WriteFile(stateDB, []byte("raw non-Hermes state"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, WriteArchiveFiles(&buf, TargetSet{
		Dirs:       map[parser.AgentType][]string{parser.AgentClaude: {root}},
		ExtraFiles: []string{stateDB},
	}, []string{stateDB}))

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, archiveNameForTest(t, stateDB), hdr.Name)
	body, err := io.ReadAll(tr)
	require.NoError(t, err)
	assert.Equal(t, "raw non-Hermes state", string(body))
}

func TestResolveDeltaFilePath(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
		path  string
		want  string
		ok    bool
	}{
		{"exact root", []string{"/srv/extra.jsonl"}, "/srv/extra.jsonl",
			"/srv/extra.jsonl", true},
		{"nested under root", []string{"/srv/claude"}, "/srv/claude/p/s.jsonl",
			"/srv/claude/p/s.jsonl", true},
		{"outside all roots", []string{"/srv/claude"}, "/etc/passwd", "", false},
		{"traversal escapes root", []string{"/srv/claude"},
			"/srv/claude/../secret", "", false},
		{"prefix sibling", []string{"/srv/claude"}, "/srv/claude-evil/x",
			"", false},
		{"no roots", nil, "/srv/claude/p/s.jsonl", "", false},
	}
	fromSlashAll := func(paths []string) []string {
		out := make([]string, len(paths))
		for i, p := range paths {
			out[i] = filepath.FromSlash(p)
		}
		return out
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolveDeltaFilePath(
				fromSlashAll(tt.roots), filepath.FromSlash(tt.path))
			require.Equal(t, tt.ok, ok)
			assert.Equal(t, filepath.FromSlash(tt.want), got)
		})
	}
}
