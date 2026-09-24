package segfile_test

import (
	"fmt"
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
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func sealed(t *testing.T, source string, seq uint64, classes ...ledger.EventClass) ledger.Segment {
	t.Helper()
	seg := ledger.NewSegment(source, seq, t0)
	for i, c := range classes {
		seg.Append(ledger.Event{
			EventID:     ledger.DeterministicEventID(source, fmt.Sprintf("%d/%d", seq, i)),
			Zone:        "test",
			Source:      "test",
			SourceSeq:   uint64(i + 1),
			Timestamp:   t0,
			EventClass:  c,
			PayloadTier: ledger.TierMetadataOnly,
		})
	}
	require.NoError(t, seg.Seal())
	return seg
}

func writeFile(t *testing.T, path string, seg ledger.Segment) {
	t.Helper()
	b, err := ledger.MarshalSegmentFile(seg)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, b, 0o644))
}

func failedMessages(r segfile.ImportReport) []string {
	var out []string
	for _, f := range r.Failed {
		out = append(out, f[0]+":"+f[1]+" "+f[2])
	}
	return out
}

// Ports ledger-sqlite db.rs:784-954 (refresh_from_store) against the
// agentsview store.
func TestImportDir(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, dir string)
	}{
		{"test_refresh_skips_bad_segments_and_recovers", func(t *testing.T, dir string) {
			t.Helper()
			d := dbtest.OpenTestDB(t)
			seg1 := sealed(t, "host-a", 1, ledger.ClassHealth)
			seg2 := sealed(t, "host-a", 2, ledger.ClassIngest, ledger.ClassIngest)
			seg3 := sealed(t, "host-a", 3, ledger.ClassApproval)
			writeFile(t, filepath.Join(dir, seg3.Filename()), seg3)
			require.NoError(t, os.WriteFile(filepath.Join(dir, seg1.Filename()), []byte("not json {"), 0o644))
			tampered := seg2
			tampered.Checksum = 99999
			writeFile(t, filepath.Join(dir, seg2.Filename()), tampered)

			r, err := segfile.ImportDir(t.Context(), dir, "zone-a", d)
			require.NoError(t, err)
			assert.Equal(t, 1, r.SegmentsIndexed, "seg3 still indexes after two bad ones")
			assert.Equal(t, 1, r.EventsIndexed)
			msgs := failedMessages(r)
			require.Len(t, msgs, 2)
			assert.True(t, strings.HasPrefix(msgs[0], "host-a:1 read error"), msgs[0])
			assert.Equal(t, "host-a:2 checksum mismatch", msgs[1])

			writeFile(t, filepath.Join(dir, seg1.Filename()), seg1)
			writeFile(t, filepath.Join(dir, seg2.Filename()), seg2)
			r, err = segfile.ImportDir(t.Context(), dir, "zone-a", d)
			require.NoError(t, err)
			assert.Equal(t, 2, r.SegmentsIndexed)
			assert.Equal(t, 3, r.EventsIndexed)
			assert.Empty(t, r.Failed)
			assert.Equal(t, 1, r.Skipped)

			r, err = segfile.ImportDir(t.Context(), dir, "zone-a", d)
			require.NoError(t, err)
			assert.Equal(t, 0, r.SegmentsIndexed)
			assert.Equal(t, 3, r.Skipped)
			assert.Empty(t, r.Failed)
		}},
		{"test_refresh_surfaces_listing_errors", func(t *testing.T, dir string) {
			t.Helper()
			d := dbtest.OpenTestDB(t)
			good := sealed(t, "host-a", 1, ledger.ClassHealth)
			writeFile(t, filepath.Join(dir, good.Filename()), good)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("{}"), 0o644))
			r, err := segfile.ImportDir(t.Context(), dir, "zone-a", d)
			require.NoError(t, err)
			assert.Equal(t, 1, r.SegmentsIndexed)
			assert.Empty(t, r.Failed, "listing errors are not per-segment failures")
			require.Len(t, r.ListingErrors, 1)
			assert.Contains(t, r.ListingErrors[0], "garbage.json")
		}},
		{"test_refresh_rejects_mislabeled_file", func(t *testing.T, dir string) {
			t.Helper()
			d := dbtest.OpenTestDB(t)
			good := sealed(t, "host-a", 1, ledger.ClassHealth)
			writeFile(t, filepath.Join(dir, good.Filename()), good)
			liar := sealed(t, "host-b", 7, ledger.ClassIngest)
			writeFile(t, filepath.Join(dir, "host-a-000002.json"), liar)
			r, err := segfile.ImportDir(t.Context(), dir, "zone-a", d)
			require.NoError(t, err)
			assert.Equal(t, 1, r.SegmentsIndexed)
			assert.Equal(t, []string{`host-a:2 identity mismatch: file claims source="host-b" seq=7`}, failedMessages(r))
		}},
		{"test_refresh_skips_out_of_range_seq_and_continues", func(t *testing.T, dir string) {
			t.Helper()
			d := dbtest.OpenTestDB(t)
			huge := sealed(t, "host-a", ^uint64(0), ledger.ClassHealth)
			writeFile(t, filepath.Join(dir, huge.Filename()), huge)
			badInner := ledger.NewSegment("host-a", 1, t0)
			badInner.Append(ledger.Event{
				EventID: ledger.DeterministicEventID("host-a", "inner"), Zone: "test", Source: "test",
				SourceSeq: ^uint64(0), Timestamp: t0, EventClass: ledger.ClassIngest, PayloadTier: ledger.TierMetadataOnly,
			})
			require.NoError(t, badInner.Seal())
			writeFile(t, filepath.Join(dir, badInner.Filename()), badInner)
			good := sealed(t, "host-a", 2, ledger.ClassApproval)
			writeFile(t, filepath.Join(dir, good.Filename()), good)
			r, err := segfile.ImportDir(t.Context(), dir, "zone-a", d)
			require.NoError(t, err)
			assert.Equal(t, 1, r.SegmentsIndexed)
			assert.Equal(t, 1, r.EventsIndexed)
			require.Len(t, r.Failed, 2)
			for _, f := range r.Failed {
				assert.Contains(t, f[2], "i64::MAX")
			}
		}},
		{"conflicting_copy_of_stored_identity_is_a_failure", func(t *testing.T, dir string) {
			t.Helper()
			d := dbtest.OpenTestDB(t)
			stored := sealed(t, "host-a", 1, ledger.ClassHealth)
			_, err := d.AppendLedgerSegment(t.Context(), "zone-a", stored, ledger.OriginLocal)
			require.NoError(t, err)
			other := sealed(t, "host-a", 1, ledger.ClassHealth, ledger.ClassRoute)
			writeFile(t, filepath.Join(dir, other.Filename()), other)
			r, err := segfile.ImportDir(t.Context(), dir, "zone-a", d)
			require.NoError(t, err)
			assert.Equal(t, 1, r.Skipped, "a stored identity is not re-read (db.rs:325-327)")
			assert.Empty(t, r.Failed)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, t.TempDir()) })
	}
}

func TestExportZoneReproducesRustFiles(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	src := filepath.Join("..", "testdata", "segments")
	r, err := segfile.ImportDir(t.Context(), src, "default", d)
	require.NoError(t, err)
	require.Len(t, r.Failed, 1, "only the event seq above i64::MAX is refused")
	assert.Equal(t, "fixture-bigseq", r.Failed[0][0])

	out := t.TempDir()
	exp, err := segfile.ExportZone(t.Context(), d, "default", out, "")
	require.NoError(t, err)
	assert.Empty(t, exp.Failed)
	entries, err := os.ReadDir(out)
	require.NoError(t, err)
	assert.Len(t, entries, exp.Written)
	for _, e := range entries {
		want, err := os.ReadFile(filepath.Join(src, e.Name()))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(out, e.Name()))
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got), e.Name())
	}

	again, err := segfile.ExportZone(t.Context(), d, "default", out, "")
	require.NoError(t, err)
	assert.Equal(t, 0, again.Written)
	assert.Equal(t, exp.Written, again.Identical)

	one := t.TempDir()
	only, err := segfile.ExportZone(t.Context(), d, "default", one, "host-with-dash")
	require.NoError(t, err)
	assert.Equal(t, 1, only.Written)

	conflictDir := t.TempDir()
	stranger := sealed(t, "host-with-dash", 7, ledger.ClassHealth)
	writeFile(t, filepath.Join(conflictDir, stranger.Filename()), stranger)
	clash, err := segfile.ExportZone(t.Context(), d, "default", conflictDir, "host-with-dash")
	require.NoError(t, err)
	require.Len(t, clash.Failed, 1)
	assert.Contains(t, clash.Failed[0][1], "DIFFERENT content")
}
