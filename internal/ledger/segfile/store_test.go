package segfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/ledger"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// sealedSegment mirrors jilog's store.rs:593-600 helper with
// deterministic event IDs; salt varies content for the same identity.
func sealedSegment(t *testing.T, source string, seq uint64, n int, salt string) ledger.Segment {
	t.Helper()
	seg := ledger.NewSegment(source, seq, t0)
	for i := range n {
		seg.Append(ledger.Event{
			EventID:     ledger.DeterministicEventID(source, fmt.Sprintf("%s/%d/%d", salt, seq, i)),
			Zone:        "test-zone",
			Source:      "test",
			SourceSeq:   uint64(i),
			Timestamp:   t0,
			EventClass:  ledger.ClassHealth,
			PayloadTier: ledger.TierMetadataOnly,
		})
	}
	require.NoError(t, seg.Seal())
	return seg
}

func publish(t *testing.T, dir string, seg ledger.Segment) {
	t.Helper()
	out, err := PublishNew(filepath.Join(dir, seg.Filename()), seg)
	require.NoError(t, err)
	require.Equal(t, ledger.Published, out)
}

func tmpCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			n++
		}
	}
	return n
}

// Ports store.rs:612-730 and segment.rs:528-560,760-769.
func TestStore(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, dir string)
	}{
		{"test_store_write_and_read", func(t *testing.T, dir string) {
			t.Helper()
			publish(t, dir, sealedSegment(t, "host-a", 1, 3, "a"))
			loaded, err := Store{Dir: dir}.Read("host-a", 1)
			require.NoError(t, err)
			assert.Equal(t, "host-a", loaded.Source)
			assert.Equal(t, uint64(1), loaded.SourceSeq)
			assert.Len(t, loaded.Events, 3)
			ok, err := loaded.Verify()
			require.NoError(t, err)
			assert.True(t, ok)
		}},
		{"test_store_list_and_ordering", func(t *testing.T, dir string) {
			t.Helper()
			publish(t, dir, sealedSegment(t, "host-a", 2, 1, "a"))
			publish(t, dir, sealedSegment(t, "host-a", 10, 1, "a"))
			publish(t, dir, sealedSegment(t, "host-b", 1, 1, "a"))
			publish(t, dir, sealedSegment(t, "host-a", 1, 1, "a"))
			entries, errs := Store{Dir: dir}.List()
			assert.Empty(t, errs)
			var got []string
			for _, e := range entries {
				got = append(got, fmt.Sprintf("%s:%d", e.Source, e.Seq))
			}
			assert.Equal(t, []string{"host-a:1", "host-a:2", "host-a:10", "host-b:1"}, got,
				"sorted by (source, seq), not by file name")
		}},
		{"test_list_segments_with_errors_surfaces_bad_names", func(t *testing.T, dir string) {
			t.Helper()
			publish(t, dir, sealedSegment(t, "host-a", 1, 1, "a"))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("{}"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))
			entries, errs := Store{Dir: dir}.List()
			assert.Len(t, entries, 1)
			require.Len(t, errs, 1)
			assert.Contains(t, errs[0], "garbage.json")
		}},
		{"test_list_segments_skips_sync_conflict_artifacts_and_tmp_files", func(t *testing.T, dir string) {
			t.Helper()
			publish(t, dir, sealedSegment(t, "host-a", 1, 1, "a"))
			require.NoError(t, os.WriteFile(filepath.Join(dir,
				"host-b-000002.sync-conflict-20260819-123456-ABCDEF.json"), []byte("{}"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(dir,
				"host-a-000003.json.4242.0.123.tmp"), []byte("partial"), 0o644))
			entries, errs := Store{Dir: dir}.List()
			assert.Len(t, entries, 1)
			assert.Empty(t, errs)
		}},
		{"missing_directory_is_empty", func(t *testing.T, dir string) {
			t.Helper()
			entries, errs := Store{Dir: filepath.Join(dir, "nope")}.List()
			assert.Empty(t, entries)
			assert.Empty(t, errs)
		}},
		{"test_read_nonexistent_file_returns_io_error", func(t *testing.T, dir string) {
			t.Helper()
			_, err := Store{Dir: dir}.Read("host-a", 1)
			require.ErrorIs(t, err, fs.ErrNotExist)
		}},
		{"read_refuses_traversal_source", func(t *testing.T, dir string) {
			t.Helper()
			_, err := Store{Dir: dir}.Read("../evil", 1)
			require.ErrorContains(t, err, "invalid segment source")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, t.TempDir()) })
	}
}

// Ports segment.rs:652-757 and adapts
// test_write_concurrent_writers_one_winner_no_stray_tmp (:599) to the
// no-clobber publish, the only writer agentsview has.
func TestPublishNew(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, dir string)
	}{
		{"test_publish_new_no_clobber", func(t *testing.T, dir string) {
			t.Helper()
			a := sealedSegment(t, "host-a", 1, 1, "a")
			b := sealedSegment(t, "host-a", 1, 1, "b")
			path := filepath.Join(dir, a.Filename())
			out, err := PublishNew(path, a)
			require.NoError(t, err)
			assert.Equal(t, ledger.Published, out)
			out, err = PublishNew(path, a)
			require.NoError(t, err)
			assert.Equal(t, ledger.AlreadyIdentical, out)
			_, err = PublishNew(path, b)
			require.ErrorIs(t, err, ledger.ErrIntegrity)
			assert.Contains(t, err.Error(), "DIFFERENT content")
			onDisk, err := ReadFile(path)
			require.NoError(t, err)
			assert.True(t, onDisk.ContentMatches(a), "conflict must not clobber the existing file")
			assert.Equal(t, 0, tmpCount(t, dir))
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Len(t, entries, 1)
		}},
		{"test_create_new_with_retry_never_touches_planted_file", func(t *testing.T, dir string) {
			t.Helper()
			planted := filepath.Join(dir, "collide.0.tmp")
			require.NoError(t, os.WriteFile(planted, []byte("precious planted bytes"), 0o644))
			var generated []string
			chosen, f, err := createNewWithRetry(func(attempt int) string {
				p := filepath.Join(dir, fmt.Sprintf("collide.%d.tmp", attempt))
				generated = append(generated, p)
				return p
			}, 16)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			assert.Equal(t, filepath.Join(dir, "collide.1.tmp"), chosen)
			assert.Len(t, generated, 2)
			b, err := os.ReadFile(planted)
			require.NoError(t, err)
			assert.Equal(t, "precious planted bytes", string(b))

			_, _, err = createNewWithRetry(func(int) string { return planted }, 3)
			require.ErrorContains(t, err, "3 attempts")
			b, err = os.ReadFile(planted)
			require.NoError(t, err)
			assert.Equal(t, "precious planted bytes", string(b))
		}},
		{"test_write_and_read_roundtrip", func(t *testing.T, dir string) {
			t.Helper()
			seg := sealedSegment(t, "host-a", 1, 2, "a")
			publish(t, dir, seg)
			raw, err := os.ReadFile(filepath.Join(dir, seg.Filename()))
			require.NoError(t, err)
			want, err := ledger.MarshalSegmentFile(seg)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(raw))
		}},
		{"concurrent_publishers_one_winner_no_stray_tmp", func(t *testing.T, dir string) {
			t.Helper()
			a := sealedSegment(t, "host-a", 1, 1, "a")
			b := sealedSegment(t, "host-a", 1, 2, "b")
			path := filepath.Join(dir, a.Filename())
			var wg sync.WaitGroup
			for _, seg := range []ledger.Segment{a, b} {
				wg.Go(func() {
					for range 50 {
						_, err := PublishNew(path, seg)
						if err != nil && !errors.Is(err, ledger.ErrIntegrity) {
							assert.NoError(t, err)
						}
					}
				})
			}
			wg.Wait()
			loaded, err := ReadFile(path)
			require.NoError(t, err)
			ok, err := loaded.Verify()
			require.NoError(t, err)
			assert.True(t, ok, "the winner is a complete, valid segment")
			assert.True(t, loaded.ContentMatches(a) || loaded.ContentMatches(b))
			assert.Equal(t, 0, tmpCount(t, dir))
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Len(t, entries, 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, t.TempDir()) })
	}
}
