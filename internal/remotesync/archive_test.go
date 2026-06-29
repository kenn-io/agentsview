package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
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
	case <-time.After(time.Second):
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
