package ssh

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tarEntry describes one entry to write into a test tar archive.
type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
	modTime  time.Time
}

// buildTestTar serializes entries into an in-memory tar archive.
func buildTestTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Mode:     0o644,
			ModTime:  e.modTime,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if e.typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func extract(t *testing.T, data []byte, dst string) (int, error) {
	t.Helper()
	return extractTarStream(t.Context(), bytes.NewReader(data), dst)
}

func TestExtractTarStreamSkipsSelfHardlink(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/wes/good.txt", typeflag: tar.TypeReg, body: "hello"},
		// Self-referential hardlink: the Antigravity case bsdtar
		// reports as "hardlink pointing to itself".
		{
			name:     "home/wes/loop.jsonl",
			typeflag: tar.TypeLink,
			linkname: "home/wes/loop.jsonl",
		},
		{name: "home/wes/after.txt", typeflag: tar.TypeReg, body: "world"},
	})

	skipped, err := extract(t, data, dst)
	require.NoError(t, err)
	assert.Equal(t, 1, skipped)

	good, err := os.ReadFile(filepath.Join(dst, "home/wes/good.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(good))
	after, err := os.ReadFile(filepath.Join(dst, "home/wes/after.txt"))
	require.NoError(t, err)
	assert.Equal(t, "world", string(after))

	_, statErr := os.Lstat(filepath.Join(dst, "home/wes/loop.jsonl"))
	assert.True(t,
		os.IsNotExist(statErr),
		"self-referential hardlink should not be created",
	)
}

func TestExtractTarStreamTruncatedMidBodyFails(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/a.txt", typeflag: tar.TypeReg, body: "aaa"},
		{
			name:     "home/big.txt",
			typeflag: tar.TypeReg,
			body:     strings.Repeat("x", 4096),
		},
	})
	// First entry (1024B) intact; cut inside the second file's body.
	truncated := data[:1024+512+100]

	_, err := extract(t, truncated, dst)
	require.Error(t, err, "truncated transfer must fail, not be accepted")
	assert.NoFileExists(
		t, filepath.Join(dst, "home/big.txt"),
		"a truncated file must not be left as if complete",
	)
}

func TestExtractTarStreamTruncatedMidHeaderFails(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/a.txt", typeflag: tar.TypeReg, body: "aaa"},
		{name: "home/b.txt", typeflag: tar.TypeReg, body: "bbb"},
	})
	// Keep first entry whole; cut partway through the second header.
	truncated := data[:1024+200]

	_, err := extract(t, truncated, dst)
	require.Error(t, err)
}

func TestExtractTarStreamCorruptHeaderFails(t *testing.T) {
	dst := t.TempDir()
	garbage := bytes.Repeat([]byte("A"), 1024)

	_, err := extract(t, garbage, dst)
	require.Error(t, err, "corrupt/unrecognized archive must fail")
}

func TestExtractTarStreamRejectsRelativePathEscape(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "../escape.txt", typeflag: tar.TypeReg, body: "pwned"},
	})

	_, err := extract(t, data, dst)
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(filepath.Dir(dst), "escape.txt"))
}

func TestExtractTarStreamSkipsSymlinks(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/target.txt", typeflag: tar.TypeReg, body: "data"},
		// An in-tree link plus relative and absolute escapes: all
		// are skipped, never created, so none can redirect a later
		// write outside the extraction dir.
		{
			name:     "home/link.txt",
			typeflag: tar.TypeSymlink,
			linkname: "target.txt",
		},
		{
			name:     "home/rel-escape",
			typeflag: tar.TypeSymlink,
			linkname: "../../../../etc",
		},
		{
			name:     "home/abs-escape",
			typeflag: tar.TypeSymlink,
			linkname: "/etc/passwd",
		},
	})

	_, err := extract(t, data, dst)
	require.NoError(t, err)

	// The regular file is still extracted.
	body, err := os.ReadFile(filepath.Join(dst, "home/target.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data", string(body))

	// No symlink is created anywhere.
	for _, name := range []string{
		"home/link.txt", "home/rel-escape", "home/abs-escape",
	} {
		_, statErr := os.Lstat(filepath.Join(dst, name))
		assert.True(t,
			os.IsNotExist(statErr), "%s should be skipped", name,
		)
	}
}

func TestExtractTarStreamNormalHardlink(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/a.txt", typeflag: tar.TypeReg, body: "shared"},
		{name: "home/b.txt", typeflag: tar.TypeLink, linkname: "home/a.txt"},
	})

	skipped, err := extract(t, data, dst)
	require.NoError(t, err)
	assert.Equal(t, 0, skipped)

	b, err := os.ReadFile(filepath.Join(dst, "home/b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "shared", string(b))
}

func TestExtractTarStreamPreservesModTime(t *testing.T) {
	dst := t.TempDir()
	// The incremental skip cache keys on (path, mtime) and the sync
	// engine treats it as authoritative, so extracted files must keep
	// their archived mtime across syncs or nothing is ever skipped.
	want := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	data := buildTestTar(t, []tarEntry{
		{
			name:     "home/wes/.claude/s.jsonl",
			typeflag: tar.TypeReg,
			body:     "session",
			modTime:  want,
		},
	})

	_, err := extract(t, data, dst)
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(dst, "home/wes/.claude/s.jsonl"))
	require.NoError(t, err)
	assert.True(
		t, info.ModTime().Equal(want),
		"extracted mtime = %s, want %s", info.ModTime(), want,
	)
}

func TestExtractTarStreamCreatesDirsAndFiles(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/wes/.claude/", typeflag: tar.TypeDir},
		{
			name:     "home/wes/.claude/s.jsonl",
			typeflag: tar.TypeReg,
			body:     "{}",
		},
	})

	skipped, err := extract(t, data, dst)
	require.NoError(t, err)
	assert.Equal(t, 0, skipped)

	info, err := os.Stat(filepath.Join(dst, "home/wes/.claude"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	body, err := os.ReadFile(filepath.Join(dst, "home/wes/.claude/s.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, "{}", string(body))
}

// TestExtractTarStreamExtractsLegacyRegularType guards against the
// assumption that the extractor must special-case the deprecated
// TypeRegA ('\x00') regular-file marker: tar.Reader.Next normalizes it
// to TypeReg (or TypeDir) before we see the header, so an entry
// authored as TypeRegA must still extract, not be silently skipped.
func TestExtractTarStreamExtractsLegacyRegularType(t *testing.T) {
	dst := t.TempDir()
	body := []byte("legacy regular file")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "home/wes/.codex/old.json",
		Typeflag: tar.TypeRegA, //nolint:staticcheck // testing the deprecated marker
		Mode:     0o644,
		Size:     int64(len(body)),
	}))
	_, err := tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	_, err = extract(t, buf.Bytes(), dst)
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(dst, "home/wes/.codex/old.json"))
	require.NoError(t, err)
	assert.Equal(t, string(body), string(got))
}
