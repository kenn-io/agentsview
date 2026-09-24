package segfile

import (
	"context"
	"fmt"
	"math"
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
		{
			"invalid_source", &InvalidSourceError{Name: "../evil"},
			`invalid segment source "../evil": must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`,
		},
		{
			"identity_mismatch", &IdentityMismatchError{Found: "hostB-000001.json", Expected: "hostA-000001.json"},
			`spool filename "hostB-000001.json" does not match segment identity "hostA-000001.json" (path-traversal / spoof guard)`,
		},
		{
			"integrity", &IntegrityError{Src: "hostA", Seq: 1, Reason: "checksum mismatch"},
			"integrity check failed for hostA:1: checksum mismatch",
		},
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

func TestSpoolIngest(t *testing.T) {
	ctx := t.Context()
	const zone = "test"

	t.Run("test_ingest_from_spool", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		w := NewSpoolWriter(spool)
		_, _, err := w.Write(spoolSegment(t, "hostA", 1, 1))
		require.NoError(t, err)
		_, _, err = w.Write(spoolSegment(t, "hostA", 2, 1))
		require.NoError(t, err)

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		assert.Len(t, report.Committed, 2)
		assert.Empty(t, report.Skipped)
		assert.Empty(t, report.Failed)
		assert.Equal(t, 2, store.count(zone))
		assert.Equal(t, 0, dirCount(t, filepath.Join(spool, "incoming")), "incoming/ should be empty after ingest")
		assert.Equal(t, 2, dirCount(t, filepath.Join(spool, "processed")), "processed/ should have 2 files")
	})

	t.Run("test_ingest_deduplicates", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		seg1 := spoolSegment(t, "hostA", 1, 1)
		_, err := store.AppendLedgerSegment(ctx, zone, seg1, "local")
		require.NoError(t, err)
		w := NewSpoolWriter(spool)
		_, _, err = w.Write(seg1)
		require.NoError(t, err)
		_, _, err = w.Write(spoolSegment(t, "hostA", 2, 1))
		require.NoError(t, err)

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		assert.Len(t, report.Committed, 1, "only segment 2 should be new")
		assert.Len(t, report.Skipped, 1, "segment 1 should be skipped as duplicate")
		assert.Empty(t, report.Failed)
		assert.FileExists(t, filepath.Join(spool, "processed", "hostA-000001.json"))
	})

	t.Run("test_ingest_rejects_traversal_shaped_source", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		path := filepath.Join(spool, "incoming", "innocent.json")
		writeSegmentFile(t, path, spoolSegment(t, "../../evil", 1, 1))

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		require.Len(t, report.Failed, 1, "traversal source must fail")
		assert.Contains(t, report.Failed[0].Err, "invalid segment source")
		assert.FileExists(t, path, "rejected segment stays in incoming/")
		assert.Equal(t, 0, store.count(zone))
	})

	t.Run("test_ingest_rejects_filename_identity_mismatch", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		path := filepath.Join(spool, "incoming", "hostB-000001.json")
		writeSegmentFile(t, path, spoolSegment(t, "hostA", 1, 1))

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		require.Len(t, report.Failed, 1, "identity mismatch must fail")
		assert.Contains(t, report.Failed[0].Err, "does not match segment identity")
		assert.FileExists(t, path)
		assert.Equal(t, 0, store.count(zone))
	})

	t.Run("test_ingest_duplicate_identity_different_content_fails", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		_, err := store.AppendLedgerSegment(ctx, zone, spoolSegment(t, "hostA", 1, 1), "local")
		require.NoError(t, err)
		_, _, err = NewSpoolWriter(spool).Write(spoolSegment(t, "hostA", 1, 1))
		require.NoError(t, err)

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		assert.Empty(t, report.Committed)
		assert.Empty(t, report.Skipped)
		require.Len(t, report.Failed, 1, "conflicting duplicate must fail the run")
		assert.Contains(t, report.Failed[0].Err, "DIFFERENT content")
		assert.FileExists(t, filepath.Join(spool, "incoming", "hostA-000001.json"))
	})

	t.Run("test_ingest_duplicate_with_corrupt_store_copy_fails", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		seg := spoolSegment(t, "hostA", 1, 1)
		rotten := seg
		rotten.Checksum = 99999
		store.put(zone, rotten)
		_, _, err := NewSpoolWriter(spool).Write(seg)
		require.NoError(t, err)

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		assert.Empty(t, report.Skipped, "corrupt store copy must not be trusted")
		require.Len(t, report.Failed, 1)
		assert.Contains(t, report.Failed[0].Err, "fails checksum verification")
		assert.FileExists(t, filepath.Join(spool, "incoming", "hostA-000001.json"),
			"the good copy stays in incoming/ for recovery")
	})

	t.Run("test_ingest_ignores_stray_tmp_files", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		_, _, err := NewSpoolWriter(spool).Write(spoolSegment(t, "hostA", 1, 1))
		require.NoError(t, err)
		tmp := filepath.Join(spool, "incoming", "hostA-000002.json.tmp")
		require.NoError(t, os.WriteFile(tmp, []byte("{ truncated"), 0o644))

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		assert.Len(t, report.Committed, 1)
		assert.Empty(t, report.Failed, "tmp file must not be treated as a segment")
		assert.FileExists(t, tmp, "tmp file is left alone")
	})

	t.Run("test_ingest_skips_syncthing_conflict_artifacts", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		_, _, err := NewSpoolWriter(spool).Write(spoolSegment(t, "hostA", 1, 1))
		require.NoError(t, err)
		conflict := filepath.Join(spool, "incoming",
			"hostA-000002.sync-conflict-20260819-070859-ABCDEFG.json")
		writeSegmentFile(t, conflict, spoolSegment(t, "hostA", 2, 1))

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		assert.Len(t, report.Committed, 1, "the real segment still ingests")
		assert.Empty(t, report.Failed, "a file-sync conflict copy must not redden every run")
		assert.FileExists(t, conflict, "conflict copy is left in incoming/ for the operator")
	})

	t.Run("test_ingest_preexisting_conflicting_processed_copy_fails", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		planted := spoolSegment(t, "hostA", 1, 1)
		writeSegmentFile(t, filepath.Join(spool, "processed", "hostA-000001.json"), planted)
		arriving := spoolSegment(t, "hostA", 1, 1)
		_, _, err := NewSpoolWriter(spool).Write(arriving)
		require.NoError(t, err)

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		assert.Empty(t, report.Committed, "conflicted move must not report committed")
		require.Len(t, report.Failed, 1)
		assert.Contains(t, report.Failed[0].Err, "DIFFERENT content")
		onDisk, err := readSegmentFile(filepath.Join(spool, "processed", "hostA-000001.json"))
		require.NoError(t, err)
		assert.True(t, onDisk.ContentMatches(planted), "processed/ copy must be preserved")
		kept, err := readSegmentFile(filepath.Join(spool, "incoming", "hostA-000001.json"))
		require.NoError(t, err)
		assert.True(t, kept.ContentMatches(arriving), "incoming/ copy must be preserved")
		assert.Equal(t, 1, store.count(zone), "the commit itself landed in the store")
	})

	t.Run("test_ingest_failed_processed_rename_is_reported_failed", func(t *testing.T) {
		skipIfPermissionsIneffective(t)
		spool := t.TempDir()
		store := newMemLedger()
		_, _, err := NewSpoolWriter(spool).Write(spoolSegment(t, "hostA", 1, 1))
		require.NoError(t, err)
		processed := filepath.Join(spool, "processed")
		require.NoError(t, os.MkdirAll(processed, 0o755))
		require.NoError(t, os.Chmod(processed, 0o555))
		t.Cleanup(func() { _ = os.Chmod(processed, 0o755) })

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, os.Chmod(processed, 0o755))
		require.NoError(t, err)
		assert.Empty(t, report.Committed, "rename failure must not count as committed")
		require.Len(t, report.Failed, 1)
		assert.Contains(t, report.Failed[0].Err, "rename to processed/ failed")
		assert.Contains(t, report.Failed[0].Err, "committed to store but ")
		assert.Equal(t, 1, store.count(zone))
		assert.FileExists(t, filepath.Join(spool, "incoming", "hostA-000001.json"))
	})

	t.Run("test_ingest_detects_corruption", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		seg := spoolSegment(t, "hostA", 1, 1)
		path, _, err := NewSpoolWriter(spool).Write(seg)
		require.NoError(t, err)
		tamperChecksum(t, path, seg)

		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		require.Len(t, report.Failed, 1, "corrupt segment should fail")
		assert.Contains(t, report.Failed[0].Err, "checksum")
		assert.FileExists(t, path, "failed segment should stay in incoming/")
	})

	t.Run("test_ingest_empty_spool", func(t *testing.T) {
		report, err := SpoolIngest(ctx, filepath.Join(t.TempDir(), "nonexistent-spool"), zone, newMemLedger())
		require.NoError(t, err)
		assert.Zero(t, report.Total())
	})

	t.Run("test_full_pipeline_writer_to_ingester", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		w := NewSpoolWriter(spool)
		for seq := uint64(1); seq <= 3; seq++ {
			_, _, err := w.Write(spoolSegment(t, "hostA", seq, 1))
			require.NoError(t, err)
		}
		report, err := SpoolIngest(ctx, spool, "fleet", store)
		require.NoError(t, err)
		assert.Len(t, report.Committed, 3)
		assert.Equal(t, 3, report.Total())
		segs, err := store.ListLedgerSegments(ctx, "fleet", "hostA", 0, 10)
		require.NoError(t, err)
		require.Len(t, segs, 3)
		for i, seg := range segs {
			ok, err := seg.Verify()
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, uint64(i+1), seg.SourceSeq, "no gaps")
		}
	})

	// Review Focus 1.
	t.Run("out_of_range_seq_fails_alone", func(t *testing.T) {
		spool := t.TempDir()
		rejecting := &seqCheckingLedger{memLedger: newMemLedger()}
		writeSegmentFile(t, filepath.Join(spool, "incoming", "hostA-000000.json"), spoolSegment(t, "hostA", 0, 1))
		huge := spoolSegment(t, "hostA", 1<<63, 1)
		writeSegmentFile(t, filepath.Join(spool, "incoming", huge.Filename()), huge)
		_, _, err := NewSpoolWriter(spool).Write(spoolSegment(t, "hostA", 1, 1))
		require.NoError(t, err)

		report, err := SpoolIngest(ctx, spool, zone, rejecting)
		require.NoError(t, err)
		assert.Equal(t, []string{"hostA-000001.json"}, report.Committed)
		require.Len(t, report.Failed, 2)
		for _, f := range report.Failed {
			assert.Contains(t, f.Err, "ledger error: ")
			assert.FileExists(t, filepath.Join(spool, "incoming", f.File))
		}
	})

	// Review Focus 2.
	t.Run("json_named_directory_and_dotfile", func(t *testing.T) {
		spool := t.TempDir()
		incoming := filepath.Join(spool, "incoming")
		require.NoError(t, os.MkdirAll(filepath.Join(incoming, "dir.json"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(incoming, ".json"), []byte("{}"), 0o644))

		report, err := SpoolIngest(ctx, spool, zone, newMemLedger())
		require.NoError(t, err)
		require.Len(t, report.Failed, 1)
		assert.Equal(t, "dir.json", report.Failed[0].File)
		assert.Contains(t, report.Failed[0].Err, "ledger error: I/O error: ")
		assert.FileExists(t, filepath.Join(incoming, ".json"), "a bare .json dotfile is not a segment")
	})

	// jilog interop: segments written by Rust (PR 12's generated fixtures,
	// floats formatted by serde_json's zmij) verify and ingest, and the
	// processed/ copies keep the producer's exact bytes.
	t.Run("rust_written_fixtures_ingest_byte_exact", func(t *testing.T) {
		spool := t.TempDir()
		store := newMemLedger()
		names := []string{"fixture-a-000001.json", "fixture-a-000002.json", "host-with-dash-000007.json"}
		want := map[string][]byte{}
		for _, n := range names {
			b, err := os.ReadFile(filepath.Join("..", "testdata", "segments", n))
			require.NoError(t, err)
			want[n] = b
			require.NoError(t, os.MkdirAll(filepath.Join(spool, "incoming"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(spool, "incoming", n), b, 0o644))
		}
		report, err := SpoolIngest(ctx, spool, zone, store)
		require.NoError(t, err)
		assert.ElementsMatch(t, names, report.Committed)
		assert.Empty(t, report.Failed)
		for _, n := range names {
			got, err := os.ReadFile(filepath.Join(spool, "processed", n))
			require.NoError(t, err)
			assert.Equal(t, want[n], got, "processed/ keeps the producer's bytes")
		}
	})

	t.Run("summary_matches_print_summary", func(t *testing.T) {
		r := IngestReport{
			Committed: []string{"a"},
			Skipped:   []string{},
			Failed:    []IngestFailure{{File: "b.json", Err: "checksum mismatch"}},
		}
		assert.Equal(t,
			"Spool ingest: 1 committed, 0 skipped, 1 failed (2 total)\n\nFailures:\n  b.json -- checksum mismatch\n",
			r.Summary())
		assert.Equal(t, "Spool ingest: 0 committed, 0 skipped, 0 failed (0 total)\n",
			IngestReport{}.Summary())
	})
}

// seqCheckingLedger enforces PR 13's storage bounds (source_seq >= 1 and
// <= i64 max, spec §5.5) the way the SQLite and PG stores do.
type seqCheckingLedger struct{ *memLedger }

func (s *seqCheckingLedger) AppendLedgerSegment(
	ctx context.Context, zone string, seg ledger.Segment, origin string,
) (ledger.PublishOutcome, error) {
	if seg.SourceSeq == 0 || seg.SourceSeq > math.MaxInt64 {
		return ledger.Published, fmt.Errorf("source_seq %d out of range", seg.SourceSeq)
	}
	return s.memLedger.AppendLedgerSegment(ctx, zone, seg, origin)
}
