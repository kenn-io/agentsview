package segfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/ledger"
)

func TestRustDebugString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"hostA", `"hostA"`},
		{"../../evil", `"../../evil"`},
		{"a\"b\\c", `"a\"b\\c"`},
		{"tab\tnl\ncr\r", `"tab\tnl\ncr\r"`},
		{"host\x00", `"host\0"`},
		{"bell\x07", `"bell\u{7}"`},
		{"héllo", `"héllo"`},
	} {
		assert.Equal(t, tc.want, rustDebugString(tc.in), "input %q", tc.in)
	}
}

func TestSpoolErrorsMatchJilogText(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"invalid_source", &InvalidSourceError{Name: "../evil"},
			`invalid segment source "../evil": must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`},
		{"identity_mismatch", &IdentityMismatchError{Found: "hostB-000001.json", Expected: "hostA-000001.json"},
			`spool filename "hostB-000001.json" does not match segment identity "hostA-000001.json" (path-traversal / spoof guard)`},
		{"integrity", &IntegrityError{Src: "hostA", Seq: 1, Reason: "checksum mismatch"},
			"integrity check failed for hostA:1: checksum mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}

func TestSpoolWriter(t *testing.T) {
	t.Run("test_write_to_spool", func(t *testing.T) {
		dir := t.TempDir()
		writer := NewSpoolWriter(dir)
		seg := spoolSegment(t, "hostA", 1, 1)

		path, outcome, err := writer.Write(seg)
		require.NoError(t, err)
		assert.Equal(t, ledger.Published, outcome)
		assert.Equal(t, filepath.Join(dir, "incoming", "hostA-000001.json"), path)

		loaded, err := readSegmentFile(path)
		require.NoError(t, err)
		assert.Equal(t, "hostA", loaded.Source)
		ok, err := loaded.Verify()
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("test_write_rejects_traversal_source", func(t *testing.T) {
		dir := t.TempDir()
		writer := NewSpoolWriter(dir)
		for _, bad := range []string{"../../evil", "/etc/cron.d/x", "a/b", ".hidden"} {
			_, _, err := writer.Write(spoolSegment(t, bad, 1, 1))
			var invalid *InvalidSourceError
			require.ErrorAs(t, err, &invalid, "source %q", bad)
		}
		_, err := os.Stat(filepath.Join(dir, "incoming"))
		assert.ErrorIs(t, err, os.ErrNotExist, "rejected write must not create files")
	})

	t.Run("test_write_conflicting_existing_is_error_and_preserves_both", func(t *testing.T) {
		dir := t.TempDir()
		writer := NewSpoolWriter(dir)
		first := spoolSegment(t, "hostA", 1, 1)
		_, _, err := writer.Write(first)
		require.NoError(t, err)

		second := spoolSegment(t, "hostA", 1, 2)
		_, _, err = writer.Write(second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DIFFERENT content")

		onDisk, err := readSegmentFile(filepath.Join(dir, "incoming", "hostA-000001.json"))
		require.NoError(t, err)
		assert.True(t, onDisk.ContentMatches(first), "existing file must be preserved")
		assert.Equal(t, 1, dirCount(t, filepath.Join(dir, "incoming")), "no extra files after the conflict")
	})

	t.Run("test_write_is_idempotent", func(t *testing.T) {
		dir := t.TempDir()
		writer := NewSpoolWriter(dir)
		seg := spoolSegment(t, "hostA", 1, 1)
		_, _, err := writer.Write(seg)
		require.NoError(t, err)
		path, outcome, err := writer.Write(seg)
		require.NoError(t, err)
		assert.Equal(t, ledger.AlreadyIdentical, outcome,
			"second identical write must report a skip, not a fresh write")
		assert.FileExists(t, path)
	})
}
