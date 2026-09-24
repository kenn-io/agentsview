package segfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/ledger"
)

const emitZone = "test-zone"

type emitFixture struct {
	store   *memLedger
	spool   string
	cursors string
}

func newEmitFixture(t *testing.T) emitFixture {
	root := t.TempDir()
	return emitFixture{store: newMemLedger(), spool: filepath.Join(root, "spool"), cursors: filepath.Join(root, "cursors")}
}

func (f emitFixture) emit(t *testing.T) EmitReport {
	t.Helper()
	rep, err := SpoolEmit(t.Context(), f.store, emitZone, "hostA", f.spool, f.cursors)
	require.NoError(t, err)
	return rep
}

func (f emitFixture) cursor() uint64 {
	return LoadCursor(CursorPath(f.cursors, emitZone, "hostA"))
}

func TestSpoolEmit(t *testing.T) {
	t.Run("emit_only_own_source_segments", func(t *testing.T) {
		f := newEmitFixture(t)
		f.store.put(emitZone, spoolSegment(t, "hostA", 1, 1))
		f.store.put(emitZone, spoolSegment(t, "hostB", 1, 1))
		rep := f.emit(t)
		assert.Empty(t, rep.Failures)
		assert.Equal(t, []string{"hostA-000001.json"}, dirNames(t, filepath.Join(f.spool, "incoming")))
	})

	t.Run("emit_is_incremental_and_idempotent", func(t *testing.T) {
		f := newEmitFixture(t)
		f.store.put(emitZone, spoolSegment(t, "hostA", 1, 1))
		f.emit(t)
		assert.Equal(t, 1, dirCount(t, filepath.Join(f.spool, "incoming")))
		rep := f.emit(t)
		assert.Equal(t, 0, rep.Emitted)
		assert.Equal(t, 1, rep.Skipped)
		assert.Equal(t, 1, dirCount(t, filepath.Join(f.spool, "incoming")))
		f.store.put(emitZone, spoolSegment(t, "hostA", 2, 1))
		f.emit(t)
		assert.Equal(t, 2, dirCount(t, filepath.Join(f.spool, "incoming")))
	})

	// Delta: jilog localizes a read failure to seq 2 and still emits seq 3
	// with the cursor at 1. A DB row that cannot be decoded fails the page,
	// so it behaves like a listing error: seq 1 is emitted, nothing after it
	// is listed, and the cursor stays frozen at 0 for the run.
	t.Run("emit_read_failure_keeps_cursor_below_failure", func(t *testing.T) {
		f := newEmitFixture(t)
		for seq := uint64(1); seq <= 3; seq++ {
			f.store.put(emitZone, spoolSegment(t, "hostA", seq, 1))
		}
		f.store.failAt["hostA"] = 2
		rep := f.emit(t)
		require.Len(t, rep.Failures, 1)
		assert.Contains(t, rep.Failures[0], "spool emit [test-zone]: ")
		assert.Equal(t, 1, dirCount(t, filepath.Join(f.spool, "incoming")))
		assert.Equal(t, uint64(0), f.cursor(), "cursor must stay below the failed seq")

		delete(f.store.failAt, "hostA")
		rep = f.emit(t)
		assert.Empty(t, rep.Failures)
		assert.Equal(t, 3, dirCount(t, filepath.Join(f.spool, "incoming")))
		assert.Equal(t, uint64(3), f.cursor())
	})

	t.Run("emit_backfills_lower_seq_despite_higher_cursor", func(t *testing.T) {
		f := newEmitFixture(t)
		f.store.put(emitZone, spoolSegment(t, "hostA", 1, 1))
		f.store.put(emitZone, spoolSegment(t, "hostA", 2, 1))
		f.emit(t)
		assert.Equal(t, uint64(2), f.cursor())
		require.NoError(t, os.Remove(filepath.Join(f.spool, "incoming", "hostA-000001.json")))
		f.emit(t)
		assert.FileExists(t, filepath.Join(f.spool, "incoming", "hostA-000001.json"),
			"backfill must re-emit a lower seq despite the higher cursor")
		assert.Equal(t, uint64(2), f.cursor())
	})

	t.Run("emit_rejects_mislabeled_segment_file", func(t *testing.T) {
		f := newEmitFixture(t)
		evil := spoolSegment(t, "../../evil", 1, 1)
		f.store.put(emitZone, evil)
		// The row is listed under hostA but deserializes to another source.
		f.store.segs[memKey(emitZone, "hostA")] = f.store.segs[memKey(emitZone, "../../evil")]
		rep := f.emit(t)
		require.Len(t, rep.Failures, 1)
		assert.Equal(t,
			`spool emit: hostA-000001 identity mismatch: file claims source="../../evil" seq=1 — refusing to spool`,
			rep.Failures[0])
		assert.Equal(t, 0, dirCount(t, filepath.Join(f.spool, "incoming")))
		assert.Equal(t, uint64(0), f.cursor())
	})

	t.Run("emit_conflicting_spool_copy_is_failure_not_skip", func(t *testing.T) {
		f := newEmitFixture(t)
		local := spoolSegment(t, "hostA", 1, 1)
		f.store.put(emitZone, local)
		f.emit(t)
		planted := spoolSegment(t, "hostA", 1, 3)
		incomingPath := filepath.Join(f.spool, "incoming", "hostA-000001.json")
		writeSegmentFile(t, incomingPath, planted)

		rep := f.emit(t)
		require.Len(t, rep.Failures, 1)
		assert.Contains(t, rep.Failures[0], "same identity, DIFFERENT content")
		onDisk, err := readSegmentFile(incomingPath)
		require.NoError(t, err)
		assert.True(t, onDisk.ContentMatches(planted), "conflict must not overwrite")

		writeSegmentFile(t, incomingPath, local)
		rep = f.emit(t)
		assert.Empty(t, rep.Failures)
	})

	t.Run("emit_conflicting_incoming_fails_even_with_identical_processed_copy", func(t *testing.T) {
		f := newEmitFixture(t)
		good := spoolSegment(t, "hostA", 1, 1)
		f.store.put(emitZone, good)
		f.emit(t)
		writeSegmentFile(t, filepath.Join(f.spool, "processed", "hostA-000001.json"), good)
		require.NoError(t, os.Remove(filepath.Join(f.spool, "incoming", "hostA-000001.json")))
		planted := spoolSegment(t, "hostA", 1, 3)
		writeSegmentFile(t, filepath.Join(f.spool, "incoming", "hostA-000001.json"), planted)

		rep := f.emit(t)
		require.Len(t, rep.Failures, 1)
		processedOnDisk, err := readSegmentFile(filepath.Join(f.spool, "processed", "hostA-000001.json"))
		require.NoError(t, err)
		assert.True(t, processedOnDisk.ContentMatches(good))
		incomingOnDisk, err := readSegmentFile(filepath.Join(f.spool, "incoming", "hostA-000001.json"))
		require.NoError(t, err)
		assert.True(t, incomingOnDisk.ContentMatches(planted))
	})

	t.Run("emit_write_failure_is_recorded_and_batch_continues", func(t *testing.T) {
		skipIfPermissionsIneffective(t)
		f := newEmitFixture(t)
		f.store.put(emitZone, spoolSegment(t, "hostA", 1, 1))
		f.store.put(emitZone, spoolSegment(t, "hostA", 2, 1))
		incoming := filepath.Join(f.spool, "incoming")
		require.NoError(t, os.MkdirAll(incoming, 0o755))
		require.NoError(t, os.Chmod(incoming, 0o555))
		t.Cleanup(func() { _ = os.Chmod(incoming, 0o755) })

		rep := f.emit(t)
		assert.Len(t, rep.Failures, 2, "does not abort on the first write failure")
		assert.Contains(t, rep.Failures[0], "spool emit: spool-write hostA-000001.json failed: ")
		assert.Equal(t, uint64(0), f.cursor(), "cursor must not advance past failed writes")

		require.NoError(t, os.Chmod(incoming, 0o755))
		rep = f.emit(t)
		assert.Empty(t, rep.Failures)
		assert.Equal(t, 2, dirCount(t, incoming))
		assert.Equal(t, uint64(2), f.cursor())
	})

	t.Run("emit_surfaces_ledger_listing_errors_and_freezes_cursor", func(t *testing.T) {
		f := newEmitFixture(t)
		f.store.put(emitZone, spoolSegment(t, "hostA", 1, 1))
		f.store.failAt["hostA"] = 2
		rep := f.emit(t)
		require.Len(t, rep.Failures, 1)
		assert.Equal(t, 1, dirCount(t, filepath.Join(f.spool, "incoming")), "readable segment still emitted")
		assert.Equal(t, uint64(0), f.cursor(), "listing errors must freeze the cursor")
		delete(f.store.failAt, "hostA")
		f.emit(t)
		assert.Equal(t, uint64(1), f.cursor())
	})

	t.Run("emit_refuses_corrupt_local_segment", func(t *testing.T) {
		f := newEmitFixture(t)
		f.store.put(emitZone, spoolSegment(t, "hostA", 1, 1))
		bad := spoolSegment(t, "hostA", 2, 1)
		bad.Checksum = 99999
		f.store.put(emitZone, bad)
		rep := f.emit(t)
		require.Len(t, rep.Failures, 1)
		assert.Equal(t,
			"spool emit: hostA-000002 fails checksum verification — refusing to spool corrupt segment",
			rep.Failures[0])
		assert.Equal(t, []string{"hostA-000001.json"}, dirNames(t, filepath.Join(f.spool, "incoming")))
		assert.Equal(t, uint64(1), f.cursor())
	})

	t.Run("cursor_path_is_unambiguous_for_dashed_names", func(t *testing.T) {
		dir := filepath.Join("cursors")
		assert.NotEqual(t, CursorPath(dir, "team-ops", "x"), CursorPath(dir, "team", "ops-x"))
	})

	t.Run("cursor_file_bytes_match_jilog", func(t *testing.T) {
		f := newEmitFixture(t)
		f.store.put(emitZone, spoolSegment(t, "hostA", 1, 1))
		f.emit(t)
		b, err := os.ReadFile(CursorPath(f.cursors, emitZone, "hostA"))
		require.NoError(t, err)
		assert.Equal(t, "{\n  \"last_emitted_seq\": 1\n}", string(b))
	})

	// jilog interop in the other direction: a segment that came from a Rust
	// producer is re-emitted byte-for-byte (PR 12 guarantees MarshalSegmentFile
	// reproduces serde's to_string_pretty, including zmij floats).
	t.Run("emit_reproduces_rust_bytes", func(t *testing.T) {
		f := newEmitFixture(t)
		raw, err := os.ReadFile(filepath.Join("..", "testdata", "segments", "fixture-a-000001.json"))
		require.NoError(t, err)
		seg, err := ledger.ParseSegmentFile(raw)
		require.NoError(t, err)
		f.store.put(emitZone, seg)
		rep, err := SpoolEmit(t.Context(), f.store, emitZone, seg.Source, f.spool, f.cursors)
		require.NoError(t, err)
		assert.Empty(t, rep.Failures)
		got, err := os.ReadFile(filepath.Join(f.spool, "incoming", "fixture-a-000001.json"))
		require.NoError(t, err)
		assert.Equal(t, raw, got)
	})

	t.Run("line_matches_jilog", func(t *testing.T) {
		assert.Equal(t, "spool emit [z]: source=hostA emitted=2 skipped=1 cursor=3",
			EmitReport{Emitted: 2, Skipped: 1, Cursor: 3}.Line("z", "hostA"))
	})

	// Review Focus 5: an identical copy lands between the existence check
	// and the publish; PublishNew reports AlreadyIdentical and emit skips.
	t.Run("race_identical_copy_counts_as_skip", func(t *testing.T) {
		f := newEmitFixture(t)
		seg := spoolSegment(t, "hostA", 1, 1)
		f.store.put(emitZone, seg)
		restore := beforeSpoolPublish
		t.Cleanup(func() { beforeSpoolPublish = restore })
		beforeSpoolPublish = func(path string) { writeSegmentFile(t, path, seg) }
		rep := f.emit(t)
		assert.Empty(t, rep.Failures)
		assert.Equal(t, 0, rep.Emitted)
		assert.Equal(t, 1, rep.Skipped)
		assert.Equal(t, uint64(1), f.cursor())
	})
}
