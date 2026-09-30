package segfile_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/ledger/segfile"
	"go.kenn.io/agentsview/internal/ledger/spoolrun"
)

// spoolSealed is named apart from PR 13's sealed in transfer_test.go.
func spoolSealed(t *testing.T, source string, seq uint64, n int) ledger.Segment {
	t.Helper()
	seg := ledger.NewSegment(source, seq, time.Now())
	for range n {
		seg.Append(ledger.Event{
			EventID: ledger.NewEventID(), Zone: "test-zone", Source: source, SourceSeq: seq,
			Timestamp: time.Now().UTC(), EventClass: ledger.ClassHealth, PayloadTier: ledger.TierMetadataOnly,
		})
	}
	require.NoError(t, seg.Seal())
	return seg
}

func assertNoSQLiteUnder(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		for _, suffix := range []string{".sqlite", ".db", "-wal", "-shm", "-journal"} {
			assert.False(t, strings.HasSuffix(name, suffix), "SQLite artifact %s inside the spool tree", path)
		}
		return nil
	}))
}

func TestSpoolRoundTripSQLite(t *testing.T) {
	ctx := t.Context()
	producer := dbtest.OpenTestDB(t)
	authority := dbtest.OpenTestDB(t)
	root := t.TempDir()
	spool := filepath.Join(root, "spool")
	cursors := filepath.Join(root, "cursors")
	zone := spoolrun.Zone{ID: "test-zone", SpoolRoot: spool, Authority: true}

	for _, seg := range []ledger.Segment{spoolSealed(t, "hostA", 1, 2), spoolSealed(t, "hostA", 2, 1)} {
		_, err := producer.AppendLedgerSegment(ctx, "test-zone", seg, "local")
		require.NoError(t, err)
	}

	t.Run("emit_then_ingest_round_trip_with_dedup", func(t *testing.T) {
		var out, errOut bytes.Buffer
		require.NoError(t, spoolrun.EmitZones(ctx, &out, &errOut, []spoolrun.Zone{zone}, producer, "hostA", cursors))
		assert.Equal(t, "spool emit [test-zone]: source=hostA emitted=2 skipped=0 cursor=2\n", out.String())
		assert.Len(t, mustReadDir(t, filepath.Join(spool, "incoming")), 2)

		run, err := spoolrun.IngestZones(ctx, []spoolrun.Zone{zone}, authority)
		require.NoError(t, err)
		out.Reset()
		require.NoError(t, run.Render(&out, &errOut))
		assert.Equal(t, "spool ingest [test-zone]: Spool ingest: 2 committed, 0 skipped, 0 failed (2 total)\n", out.String())

		status, err := authority.LedgerStatus(ctx, "test-zone")
		require.NoError(t, err)
		assert.Equal(t, 2, status.Segments)
		assert.Equal(t, 3, status.Events, "the archive holds all events")
		assert.Empty(t, mustReadDir(t, filepath.Join(spool, "incoming")))
		assert.Len(t, mustReadDir(t, filepath.Join(spool, "processed")), 2)
		assertNoSQLiteUnder(t, spool)

		// A wiped cursor must not re-spool already-processed segments.
		require.NoError(t, os.RemoveAll(cursors))
		out.Reset()
		require.NoError(t, spoolrun.EmitZones(ctx, &out, &errOut, []spoolrun.Zone{zone}, producer, "hostA", cursors))
		assert.Empty(t, mustReadDir(t, filepath.Join(spool, "incoming")))

		// An identical duplicate is skipped by the store-level dedup.
		segs, err := producer.ListLedgerSegments(ctx, "test-zone", "hostA", 0, 1)
		require.NoError(t, err)
		_, _, err = segfile.NewSpoolWriter(spool).Write(segs[0])
		require.NoError(t, err)
		run, err = spoolrun.IngestZones(ctx, []spoolrun.Zone{zone}, authority)
		require.NoError(t, err)
		assert.Equal(t, []string{"hostA-000001.json"}, run.Zones[0].Skipped)
		status, err = authority.LedgerStatus(ctx, "test-zone")
		require.NoError(t, err)
		assert.Equal(t, 2, status.Segments)
		assert.Equal(t, 3, status.Events, "index unchanged after dedup")
		assertNoSQLiteUnder(t, spool)
	})
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	return entries
}
