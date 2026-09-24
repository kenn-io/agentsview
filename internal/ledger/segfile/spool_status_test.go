package segfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpoolStatus(t *testing.T) {
	t.Run("status_healthy_and_disabled_zones_exit_zero", func(t *testing.T) {
		f := newEmitFixture(t)
		f.store.put(emitZone, spoolSegment(t, "hostA", 1, 1))
		f.emit(t)
		line, healthy := SpoolStatus(f.spool, f.cursors, emitZone, "hostA")
		assert.True(t, healthy)
		assert.Equal(t, "incoming=1 processed=0 cursor[hostA]=1", line)
	})

	t.Run("status_unreadable_spool_dir_exits_nonzero", func(t *testing.T) {
		skipIfPermissionsIneffective(t)
		spool := t.TempDir()
		incoming := filepath.Join(spool, "incoming")
		require.NoError(t, os.MkdirAll(incoming, 0o755))
		require.NoError(t, os.Chmod(incoming, 0o000))
		t.Cleanup(func() { _ = os.Chmod(incoming, 0o755) })
		line, healthy := SpoolStatus(spool, t.TempDir(), emitZone, "hostA")
		assert.False(t, healthy)
		assert.Contains(t, line, "incoming=unreadable")
	})

	t.Run("status_corrupt_cursor_exits_nonzero", func(t *testing.T) {
		cursors := t.TempDir()
		cpath := CursorPath(cursors, emitZone, "hostA")
		require.NoError(t, os.MkdirAll(filepath.Dir(cpath), 0o755))
		require.NoError(t, os.WriteFile(cpath, []byte("not json {"), 0o644))
		line, healthy := SpoolStatus(t.TempDir(), cursors, emitZone, "hostA")
		assert.False(t, healthy)
		assert.Equal(t, "incoming=0 processed=0 cursor[hostA]=unreadable/corrupt (emit will restart from 0)", line)
	})

	t.Run("missing_source_is_unhealthy", func(t *testing.T) {
		line, healthy := SpoolStatus(t.TempDir(), t.TempDir(), emitZone, "")
		assert.False(t, healthy)
		assert.Equal(t, "incoming=0 processed=0 cursor[unknown]=cannot determine own ledger source — pass --source", line)
	})

	t.Run("counts_only_json_entries", func(t *testing.T) {
		spool := t.TempDir()
		processed := filepath.Join(spool, "processed")
		require.NoError(t, os.MkdirAll(processed, 0o755))
		for _, n := range []string{"a-000001.json", "a-000002.json.tmp", "notes.txt"} {
			require.NoError(t, os.WriteFile(filepath.Join(processed, n), []byte("{}"), 0o644))
		}
		line, healthy := SpoolStatus(spool, t.TempDir(), emitZone, "a")
		assert.True(t, healthy)
		assert.Equal(t, "incoming=0 processed=1 cursor[a]=0", line)
	})
}
