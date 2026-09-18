package remotesync

import (
	"archive/tar"
	"bytes"
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
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dir := filepath.Join(root, "home", "wes", ".claude")
	require.NoError(os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "session.jsonl")
	require.NoError(os.WriteFile(path, []byte("body"), 0o644))
	wantMTime := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	require.NoError(os.Chtimes(path, wantMTime, wantMTime))

	var buf bytes.Buffer
	err := WriteArchive(t.Context(), &buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
	})
	require.NoError(err)

	tr := tar.NewReader(&buf)
	var found *tar.Header
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(err)
		if hdr.Name == archiveNameForTest(t, path) {
			found = hdr
			break
		}
	}
	require.NotNil(found)
	assert.Equal(byte(tar.TypeReg), found.Typeflag)
	assert.Equal(wantMTime.Unix(), found.ModTime.Unix())
}

func TestWriteArchiveDoesNotFollowSymlink(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	require.NoError(os.WriteFile(target, []byte("secret"), 0o644))
	link := filepath.Join(dir, "link.jsonl")
	require.NoError(os.Symlink(target, link))

	var buf bytes.Buffer
	require.NoError(WriteArchive(t.Context(), &buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
	}))

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(err)
		assert.NotEqual(t, archiveNameForTest(t, link), hdr.Name)
	}
}

func TestWriteArchiveIgnoresBytesAppendedAfterHeader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	require.NoError(os.WriteFile(path, []byte("old"), 0o644))

	writer := newBlockAfterFirstTarHeaderWriter()
	errCh := make(chan error, 1)
	go func() {
		errCh <- WriteArchive(t.Context(), writer, TargetSet{
			ExtraFiles: []string{path},
		})
	}()

	select {
	case <-writer.headerWritten:
	case <-time.After(backgroundWaitTimeout):
		require.FailNow("tar header was not written")
	}
	require.NoError(appendFile(path, "new"))
	close(writer.proceed)

	require.NoError(<-errCh)
	tr := tar.NewReader(bytes.NewReader(writer.Bytes()))
	hdr, err := tr.Next()
	require.NoError(err)
	assert.Equal(archiveNameForTest(t, path), hdr.Name)
	body, err := io.ReadAll(tr)
	require.NoError(err)
	assert.Equal("old", string(body))
}

func TestWriteArchiveToleratesMissingExtraFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	missing := filepath.Join(dir, "missing.jsonl")
	require.NoError(os.WriteFile(path, []byte("body"), 0o644))

	var buf bytes.Buffer
	require.NoError(WriteArchive(t.Context(), &buf, TargetSet{
		ExtraFiles: []string{path, missing},
	}))
	archiveBytes := slices.Clone(buf.Bytes())

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	require.NoError(err)
	assert.Equal(archiveNameForTest(t, path), hdr.Name)
	_, err = io.ReadAll(tr)
	require.NoError(err)
	_, err = tr.Next()
	require.ErrorIs(err, io.EOF)
	assert.True(hasTarEndMarker(archiveBytes))
}

func TestWriteArchiveSkipsDirectoryValuedExtraFile(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	extraDir := filepath.Join(root, "state.db-wal")
	require.NoError(os.Mkdir(extraDir, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(extraDir, "credential.txt"), []byte("secret"), 0o600,
	))

	var buf bytes.Buffer
	require.NoError(WriteArchive(t.Context(), &buf, TargetSet{
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
				return WriteArchive(t.Context(), w, TargetSet{
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
			assert := assert.New(t)
			require := require.New(t)

			stateDB := filepath.Join(t.TempDir(), "profile", "state.db")
			require.NoError(os.MkdirAll(filepath.Dir(stateDB), 0o755))
			writeHermesImportStateDB(t, stateDB)
			writer, err := sql.Open("sqlite3", stateDB)
			require.NoError(err)
			t.Cleanup(func() { _ = writer.Close() })
			var journalMode string
			require.NoError(writer.QueryRowContext(t.Context(), `PRAGMA journal_mode = WAL`).Scan(&journalMode))
			assert.Equal("wal", journalMode)
			_, err = writer.ExecContext(t.Context(), `PRAGMA wal_autocheckpoint = 0`)
			require.NoError(err)
			_, err = writer.ExecContext(t.Context(), `
				UPDATE sessions
				SET title = 'Committed in WAL'
				WHERE id = 'database-only'
			`)
			require.NoError(err)
			wal := stateDB + "-wal"
			require.FileExists(wal)

			stateInfo, err := os.Stat(stateDB)
			require.NoError(err)
			archiveWriter := newBlockAfterBytesWriter(512 + stateInfo.Size())
			errCh := make(chan error, 1)
			go func() { errCh <- tt.write(archiveWriter, stateDB) }()

			select {
			case <-archiveWriter.blocked:
			case <-time.After(backgroundWaitTimeout):
				require.FailNow("archive did not finish its first database entry")
			}
			_, checkpointErr := writer.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
			require.NoError(checkpointErr)
			require.NoError(writer.Close())
			if removeErr := os.Remove(wal); !os.IsNotExist(removeErr) {
				require.NoError(removeErr)
			}
			close(archiveWriter.proceed)
			require.NoError(<-errCh)

			extracted := t.TempDir()
			_, err = ExtractTarStream(
				t.Context(), bytes.NewReader(archiveWriter.Bytes()), extracted,
			)
			require.NoError(err)
			extractedDB, err := safeRemappedRemotePath(extracted, stateDB)
			require.NoError(err)
			snapshot, err := sql.Open("sqlite3", extractedDB)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(snapshot.Close()) })
			var title string
			require.NoError(snapshot.QueryRowContext(t.Context(), `
				SELECT title FROM sessions WHERE id = 'database-only'
			`).Scan(&title))
			assert.Equal("Committed in WAL", title)
			assert.NoFileExists(extractedDB + "-wal")
			assert.NoFileExists(extractedDB + "-shm")
		})
	}
}

func TestWriteArchiveExcludesRemoteSyncExcludedAgentState(t *testing.T) {
	root := t.TempDir()
	chatDB := filepath.Join(root, "chat.db")
	require.NoError(t, os.WriteFile(chatDB, []byte("authentication state"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "credentials.json"), []byte("secret"), 0o600,
	))

	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentTrae: {root},
		},
		Files: map[parser.AgentType][]string{
			parser.AgentTrae: {chatDB},
		},
	}
	for _, tt := range []struct {
		name  string
		write func(io.Writer) error
	}{
		{
			name: "full",
			write: func(w io.Writer) error {
				return WriteArchive(t.Context(), w, targets)
			},
		},
		{
			name: "delta",
			write: func(w io.Writer) error {
				return WriteArchiveFiles(w, targets, []string{chatDB})
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var archive bytes.Buffer
			require.NoError(t, tt.write(&archive))
			_, err := tar.NewReader(&archive).Next()
			assert.ErrorIs(t, err, io.EOF,
				"a remote-sync-excluded agent's state must never enter a remote archive")
		})
	}
}

func TestWriteArchivePrunesForbiddenRootNestedInAllowedRoot(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "sessions")
	forbidden := filepath.Join(allowed, ".forbidden-provider")
	keep := filepath.Join(allowed, "session.jsonl")
	secret := filepath.Join(forbidden, "chat.db")
	require.NoError(t, os.MkdirAll(forbidden, 0o755))
	require.NoError(t, os.WriteFile(keep, []byte("session"), 0o644))
	require.NoError(t, os.WriteFile(secret, []byte("authentication state"), 0o600))

	targets := TargetSet{
		Dirs:           map[parser.AgentType][]string{parser.AgentClaude: {allowed}},
		ForbiddenRoots: []string{forbidden},
	}
	for _, tt := range []struct {
		name  string
		write func(io.Writer) error
	}{
		{
			name: "full archive",
			write: func(w io.Writer) error {
				return WriteArchive(t.Context(), w, targets)
			},
		},
		{
			name: "delta archive",
			write: func(w io.Writer) error {
				return WriteArchiveFiles(w, targets, []string{keep, secret})
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			var archive bytes.Buffer
			require.NoError(tt.write(&archive))

			var names []string
			tr := tar.NewReader(&archive)
			for {
				hdr, err := tr.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(err)
				names = append(names, hdr.Name)
			}
			assert.Contains(names, archiveNameForTest(t, keep))
			assert.NotContains(names, archiveNameForTest(t, secret),
				"a forbidden nested root must not enter the transfer artifact")
		})
	}
}

func TestWriteArchivePropagatesAdvertisedHermesSnapshotFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	stateDB := filepath.Join(t.TempDir(), "profile", "state.db")
	require.NoError(os.MkdirAll(filepath.Dir(stateDB), 0o755))
	writeHermesImportStateDB(t, stateDB)

	wantErr := errors.New("forced sqlite backup failure")
	originalSnapshot := writeSQLiteSnapshotFile
	writeSQLiteSnapshotFile = func(dstPath, srcPath string) error {
		assert.NotEmpty(dstPath)
		assert.Equal(stateDB, srcPath)
		return wantErr
	}
	t.Cleanup(func() { writeSQLiteSnapshotFile = originalSnapshot })

	var archive bytes.Buffer
	err := WriteArchive(t.Context(), &archive, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentHermes: {stateDB}},
	})
	require.Error(err)
	require.ErrorIs(err, wantErr)
	assert.Contains(err.Error(), "snapshot sqlite database")
}

// A database that passes the identity probe but vanishes before the
// online backup is a deletion race, not an archive failure: the entry
// is omitted so the next manifest evicts the mirror's stale copy. The
// propagation test above pins the complementary branch — a backup
// failure with a still-usable source stays fatal.
func TestWriteArchiveOmitsSnapshotWhenSourceVanishesMidBackup(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "profile", "state.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(stateDB), 0o755))
	writeHermesImportStateDB(t, stateDB)

	originalSnapshot := writeSQLiteSnapshotFile
	writeSQLiteSnapshotFile = func(dstPath, srcPath string) error {
		require.NoError(t, os.Remove(srcPath))
		return errors.New("open sqlite snapshot source: no such file")
	}
	t.Cleanup(func() { writeSQLiteSnapshotFile = originalSnapshot })

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentHermes: {stateDB}},
	}))
	assert.Empty(t, archiveEntries(t, archive.Bytes()))
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
	require := require.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	invalid := filepath.Join(dir, "invalid\x00path")
	require.NoError(os.WriteFile(path, []byte("body"), 0o644))

	var buf bytes.Buffer
	err := WriteArchive(t.Context(), &buf, TargetSet{
		ExtraFiles: []string{path, invalid},
	})
	require.Error(err)
	require.False(hasTarEndMarker(buf.Bytes()))

	_, err = ExtractTarStream(t.Context(), &buf, t.TempDir())
	require.Error(err)
	assert.Contains(t, err.Error(), "missing tar end marker")
}

func TestExtractTarStreamRejectsArchiveEndingWithZeroFilePayload(t *testing.T) {
	require := require.New(t)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name: "home/wes/.claude/projects/zero.bin",
		Mode: 0o644,
		Size: 1024,
	}
	require.NoError(tw.WriteHeader(hdr))
	_, err := tw.Write(make([]byte, 1024))
	require.NoError(err)

	_, err = ExtractTarStream(t.Context(), &buf, t.TempDir())
	require.Error(err)
	assert.Contains(t, err.Error(), "missing tar end marker")
}

func TestWriteArchiveFileSkipsDisappearedFile(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	require.NoError(os.WriteFile(path, []byte("body"), 0o644))
	info, err := os.Lstat(path)
	require.NoError(err)
	require.NoError(os.Remove(path))

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(writeArchiveFile(tw, path, info))
	require.NoError(tw.Close())
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
	require := require.New(t)

	srcDir := t.TempDir()
	path := filepath.Join(srcDir, "session.jsonl")
	require.NoError(os.WriteFile(path, []byte("{}\n"), 0o644))
	mtime := time.Date(2026, 7, 8, 10, 30, 0, 123456789, time.UTC)
	require.NoError(os.Chtimes(path, mtime, mtime))
	info, err := os.Stat(path)
	require.NoError(err)
	if info.ModTime().Nanosecond() == 0 {
		t.Skip("filesystem does not store nanosecond mtimes")
	}

	var buf bytes.Buffer
	require.NoError(WriteArchive(t.Context(), &buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {srcDir}},
	}))

	dstDir := t.TempDir()
	_, err = ExtractTarStream(t.Context(), &buf, dstDir)
	require.NoError(err)
	extracted, err := safeRemappedRemotePath(dstDir, path)
	require.NoError(err)
	extractedInfo, err := os.Stat(extracted)
	require.NoError(err)
	assert.Equal(t, info.ModTime().UnixNano(), extractedInfo.ModTime().UnixNano())
}

func TestWriteArchiveFilesSkipsVanishedAndSymlinks(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.jsonl")
	require.NoError(os.WriteFile(keep, []byte("k"), 0o644))
	link := filepath.Join(dir, "link.jsonl")
	require.NoError(os.Symlink(keep, link))
	gone := filepath.Join(dir, "gone.jsonl")

	var buf bytes.Buffer
	require.NoError(WriteArchiveFiles(&buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
	}, []string{gone, link, keep}))

	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(err)
		names = append(names, hdr.Name)
	}
	require.Len(names, 1)
	assert.Contains(t, names[0], "keep.jsonl")
}

func TestWriteArchiveFilesSkipsFilesOutsideAllowedRoots(t *testing.T) {
	require := require.New(t)

	allowed := t.TempDir()
	inside := filepath.Join(allowed, "s.jsonl")
	require.NoError(os.WriteFile(inside, []byte("in"), 0o644))
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	require.NoError(os.WriteFile(outside, []byte("secret"), 0o644))

	var buf bytes.Buffer
	require.NoError(WriteArchiveFiles(&buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {allowed}},
	}, []string{inside, outside}))

	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(err)
		names = append(names, hdr.Name)
	}
	require.Len(names, 1, "only the file inside the allowed root is streamed")
	assert.Contains(t, names[0], "s.jsonl")
}

func TestWriteArchiveFilesPreservesNonHermesStateDB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	stateDB := filepath.Join(root, "nested", "state.db")
	require.NoError(os.MkdirAll(filepath.Dir(stateDB), 0o755))
	require.NoError(os.WriteFile(stateDB, []byte("raw non-Hermes state"), 0o644))

	var buf bytes.Buffer
	require.NoError(WriteArchiveFiles(&buf, TargetSet{
		Dirs:       map[parser.AgentType][]string{parser.AgentClaude: {root}},
		ExtraFiles: []string{stateDB},
	}, []string{stateDB}))

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	require.NoError(err)
	assert.Equal(archiveNameForTest(t, stateDB), hdr.Name)
	body, err := io.ReadAll(tr)
	require.NoError(err)
	assert.Equal("raw non-Hermes state", string(body))
}

func TestResolveDeltaFilePath(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
		path  string
		want  string
		ok    bool
	}{
		{
			"exact root",
			[]string{"/srv/extra.jsonl"},
			"/srv/extra.jsonl",
			"/srv/extra.jsonl", true,
		},
		{
			"nested under root",
			[]string{"/srv/claude"},
			"/srv/claude/p/s.jsonl",
			"/srv/claude/p/s.jsonl", true,
		},
		{"outside all roots", []string{"/srv/claude"}, "/etc/passwd", "", false},
		{
			"traversal escapes root",
			[]string{"/srv/claude"},
			"/srv/claude/../secret", "", false,
		},
		{
			"prefix sibling",
			[]string{"/srv/claude"},
			"/srv/claude-evil/x",
			"", false,
		},
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
				fromSlashAll(tt.roots), forbiddenRootMatcher{}, filepath.FromSlash(tt.path))
			require.Equal(t, tt.ok, ok)
			assert.Equal(t, filepath.FromSlash(tt.want), got)
		})
	}
}
