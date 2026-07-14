package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestWriteArchiveDoesNotFinalizeAfterPathError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	missing := filepath.Join(dir, "missing.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("body"), 0o644))

	var buf bytes.Buffer
	err := WriteArchive(&buf, TargetSet{
		ExtraFiles: []string{path, missing},
	})

	require.Error(t, err)
	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, archiveNameForTest(t, path), hdr.Name)
	_, err = io.ReadAll(tr)
	require.NoError(t, err)
	assert.False(t, hasTarEndMarker(buf.Bytes()))
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
	missing := filepath.Join(dir, "missing.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("body"), 0o644))

	var buf bytes.Buffer
	err := WriteArchive(&buf, TargetSet{
		ExtraFiles: []string{path, missing},
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
	require.NoError(t, WriteArchiveFiles(&buf, []string{dir}, []string{gone, link, keep}))

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
	require.NoError(t, WriteArchiveFiles(&buf, []string{allowed}, []string{inside, outside}))

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
