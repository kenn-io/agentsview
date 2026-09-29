package db

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/serdejson"
)

var ledgerT0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// testLedgerSegment builds a sealed segment with n deterministic events,
// like jilog's sealed_segment helpers (store.rs:593-600).
func testLedgerSegment(t *testing.T, source string, seq uint64, n int) ledger.Segment {
	t.Helper()
	seg := ledger.NewSegment(source, seq, ledgerT0)
	for i := range n {
		obj := "claim:test-claim"
		actor := "person:test-user"
		seg.Append(ledger.Event{
			EventID:     ledger.DeterministicEventID(source, fmt.Sprintf("%d/%d", seq, i)),
			Zone:        "zone-a",
			Source:      source,
			SourceSeq:   uint64(i),
			Timestamp:   ledgerT0.Add(time.Duration(seq)*time.Hour + time.Duration(i)*time.Second),
			ActorRef:    &actor,
			ObjectRef:   &obj,
			EventClass:  ledger.ClassHealth,
			PayloadTier: ledger.TierMetadataOnly,
			Payload:     map[string]any{"action": "test", "i": serdejson.Number(strconv.Itoa(i))},
		})
	}
	require.NoError(t, seg.Seal())
	return seg
}

func mustAppend(t *testing.T, d *DB, zone string, seg ledger.Segment) ledger.PublishOutcome {
	t.Helper()
	out, err := d.AppendLedgerSegment(t.Context(), zone, seg, ledger.OriginLocal)
	require.NoError(t, err)
	return out
}

func TestLedgerSchema(t *testing.T) {
	d := testDB(t)
	for _, name := range []string{
		"ledger_segments", "ledger_events", "ledger_verify_state", "ledger_import_state",
	} {
		var n int
		require.NoError(t, d.getReader().QueryRow(t.Context(),
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n))
		assert.Equal(t, 1, n, name)
	}
	schema, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	assert.Contains(t, string(schema), ledgerEventsNoDeleteTriggerDDL,
		"RebuildLedgerIndex restores the trigger from this constant")

	// test_open_in_memory (db.rs:660)
	st, err := d.LedgerStatus(t.Context(), "zone-a")
	require.NoError(t, err)
	assert.Equal(t, 0, st.Segments)
	assert.Equal(t, 0, st.Events)
	assert.Empty(t, st.Sources)
}

func TestLedgerTablesAreAppendOnly(t *testing.T) {
	d := testDB(t)
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 1, 2))
	tests := []struct {
		name string
		sql  string
	}{
		{"update_segment", `UPDATE ledger_segments SET origin = 'push'`},
		{"delete_segment", `DELETE FROM ledger_segments`},
		{"update_event", `UPDATE ledger_events SET summary = 'x'`},
		{"delete_event", `DELETE FROM ledger_events`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.getWriter().Exec(t.Context(), tt.sql)
			require.Error(t, err)
			assert.ErrorIs(t, mapLedgerWriteError("mutating", err), ledger.ErrAppendOnly)
		})
	}
	st, err := d.LedgerStatus(t.Context(), "zone-a")
	require.NoError(t, err)
	assert.Equal(t, 1, st.Segments)
	assert.Equal(t, 2, st.Events)
}

func TestAppendLedgerSegment(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, d *DB)
	}{
		{"test_ingest_segment", func(t *testing.T, d *DB) {
			t.Helper()
			assert.Equal(t, ledger.Published, mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 1, 2)))
			st, err := d.LedgerStatus(t.Context(), "zone-a")
			require.NoError(t, err)
			assert.Equal(t, 1, st.Segments)
			assert.Equal(t, 2, st.Events)
		}},
		{"test_ingest_is_idempotent", func(t *testing.T, d *DB) {
			t.Helper()
			seg := testLedgerSegment(t, "host-a", 1, 1)
			mustAppend(t, d, "zone-a", seg)
			assert.Equal(t, ledger.AlreadyIdentical, mustAppend(t, d, "zone-a", seg))
			st, err := d.LedgerStatus(t.Context(), "zone-a")
			require.NoError(t, err)
			assert.Equal(t, 1, st.Events)
		}},
		{"test_store_rejects_duplicates", func(t *testing.T, d *DB) {
			t.Helper()
			original := testLedgerSegment(t, "host-a", 1, 1)
			mustAppend(t, d, "zone-a", original)
			_, err := d.AppendLedgerSegment(t.Context(), "zone-a", testLedgerSegment(t, "host-a", 1, 2), ledger.OriginImport)
			var conflict *ledger.ConflictError
			require.ErrorAs(t, err, &conflict)
			require.ErrorIs(t, err, ledger.ErrIntegrity)
			assert.Contains(t, err.Error(), "DIFFERENT content")
			segs, err := d.ListLedgerSegments(t.Context(), "zone-a", "host-a", 0, 0)
			require.NoError(t, err)
			require.Len(t, segs, 1)
			assert.True(t, segs[0].ContentMatches(original), "the stored row is unchanged")
		}},
		{"test_store_rejects_unsealed", func(t *testing.T, d *DB) {
			t.Helper()
			seg := testLedgerSegment(t, "host-a", 1, 1)
			seg.Checksum = 0
			_, err := d.AppendLedgerSegment(t.Context(), "zone-a", seg, ledger.OriginLocal)
			require.ErrorIs(t, err, ledger.ErrIntegrity)
			assert.Contains(t, err.Error(), "must be sealed")
		}},
		{"tampered_checksum_is_refused", func(t *testing.T, d *DB) {
			t.Helper()
			seg := testLedgerSegment(t, "host-a", 1, 1)
			seg.Checksum++
			_, err := d.AppendLedgerSegment(t.Context(), "zone-a", seg, ledger.OriginImport)
			require.ErrorIs(t, err, ledger.ErrIntegrity)
			assert.Contains(t, err.Error(), "checksum mismatch")
		}},
		{"same_identity_in_another_zone_is_independent", func(t *testing.T, d *DB) {
			t.Helper()
			mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 1, 1))
			assert.Equal(t, ledger.Published, mustAppend(t, d, "zone-b", testLedgerSegment(t, "host-a", 1, 2)))
		}},
		{"duplicate_event_id_counts_once", func(t *testing.T, d *DB) {
			t.Helper()
			first := testLedgerSegment(t, "host-a", 1, 1)
			mustAppend(t, d, "zone-a", first)
			second := ledger.NewSegment("host-a", 2, ledgerT0)
			second.Append(first.Events[0]) // same event_id, new segment
			require.NoError(t, second.Seal())
			mustAppend(t, d, "zone-a", second)
			st, err := d.LedgerStatus(t.Context(), "zone-a")
			require.NoError(t, err)
			assert.Equal(t, 2, st.Segments)
			assert.Equal(t, 1, st.Events, "only inserted rows count (D20)")
		}},
		{"invalid_inputs", func(t *testing.T, d *DB) {
			t.Helper()
			for _, c := range []struct {
				zone, origin string
				seg          ledger.Segment
				want         string
			}{
				{"../z", ledger.OriginLocal, testLedgerSegment(t, "host-a", 1, 1), "invalid ledger zone"},
				{"zone-a", ledger.OriginLocal, testLedgerSegment(t, "a/b", 1, 1), "invalid segment source"},
				{"zone-a", "elsewhere", testLedgerSegment(t, "host-a", 1, 1), "invalid ledger segment origin"},
				{"zone-a", ledger.OriginLocal, testLedgerSegment(t, "host-a", 0, 1), "at least 1"},
				{"zone-a", ledger.OriginLocal, testLedgerSegment(t, "host-a", 1<<63, 1), "i64::MAX"},
			} {
				_, err := d.AppendLedgerSegment(t.Context(), c.zone, c.seg, c.origin)
				require.ErrorContains(t, err, c.want)
			}
			st, err := d.LedgerStatus(t.Context(), "zone-a")
			require.NoError(t, err)
			assert.Equal(t, 0, st.Segments)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, testDB(t)) })
	}
}

func TestLedgerReadBack(t *testing.T) {
	d := testDB(t)
	// test_store_latest_seq (store.rs:732)
	latest, err := d.LatestLedgerSeq(t.Context(), "zone-a", "host-a")
	require.NoError(t, err)
	assert.Equal(t, uint64(0), latest)
	a1 := testLedgerSegment(t, "host-a", 1, 2)
	a2 := testLedgerSegment(t, "host-a", 2, 3)
	mustAppend(t, d, "zone-a", a1)
	mustAppend(t, d, "zone-a", a2)
	latest, err = d.LatestLedgerSeq(t.Context(), "zone-a", "host-a")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), latest)
	other, err := d.LatestLedgerSeq(t.Context(), "zone-a", "host-b")
	require.NoError(t, err)
	assert.Equal(t, uint64(0), other)

	// test_store_read_all_segments / test_roundtrip_event_fidelity
	all, err := d.ListLedgerSegments(t.Context(), "zone-a", "host-a", 0, 0)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.True(t, all[0].ContentMatches(a1))
	assert.True(t, all[1].ContentMatches(a2))
	assert.Len(t, all[1].Events, 3)
	assert.Equal(t, "person:test-user", *all[0].Events[0].ActorRef)

	page, err := d.ListLedgerSegments(t.Context(), "zone-a", "host-a", 1, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, uint64(2), page[0].SourceSeq)

	seqs, err := d.LedgerSegmentSeqs(t.Context(), "zone-a", "host-a")
	require.NoError(t, err)
	assert.Equal(t, []uint64{1, 2}, seqs)
}

func TestLedgerRustFixtures(t *testing.T) {
	d := testDB(t)
	dir := filepath.Join("..", "ledger", "testdata", "segments")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			require.NoError(t, err)
			seg, err := ledger.ParseSegmentFile(raw)
			require.NoError(t, err)
			_, err = d.AppendLedgerSegment(t.Context(), "default", seg, ledger.OriginImport)
			if strings.HasPrefix(e.Name(), "fixture-bigseq") {
				require.ErrorContains(t, err, "i64::MAX", "db.rs:366 refuses event seqs above i64::MAX")
				return
			}
			require.NoError(t, err)
			back, err := d.ListLedgerSegments(t.Context(), "default", seg.Source, seg.SourceSeq-1, 1)
			require.NoError(t, err)
			require.Len(t, back, 1)
			out, err := ledger.MarshalSegmentFile(back[0])
			require.NoError(t, err)
			assert.Equal(t, string(raw), string(out), "stored rows reproduce the jilog file")
		})
	}
}

func TestLedgerStatusGapsAndVerifyState(t *testing.T) {
	d := testDB(t)
	// test_store_detect_gaps (store.rs:765): seqs 1 and 3.
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 1, 1))
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 3, 1))
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-b", 1, 1))
	require.NoError(t, d.SaveLedgerVerifyState(t.Context(), "zone-a", "host-b", ledger.VerifyCheckpoint{
		VerifiedSeq: 1, Failures: [][3]string{{"host-b", "2", "checksum mismatch"}},
	}))
	st, err := d.LedgerStatus(t.Context(), "zone-a")
	require.NoError(t, err)
	assert.Equal(t, 3, st.Segments)
	assert.Equal(t, map[string]uint64{"host-a": 3, "host-b": 1}, st.Sources)
	assert.Equal(t, [][2]string{{"host-a", "2"}}, st.Gaps)
	assert.Equal(t, [][3]string{{"host-b", "2", "checksum mismatch"}}, st.Failures)

	got, err := d.GetLedgerVerifyState(t.Context(), "zone-a", "host-b")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uint64(1), got.VerifiedSeq)
	none, err := d.GetLedgerVerifyState(t.Context(), "zone-a", "host-c")
	require.NoError(t, err)
	assert.Nil(t, none)

	// A corrupt stored checkpoint degrades to a full verify (store.rs:502-507).
	_, err = d.getWriter().Exec(t.Context(),
		`UPDATE ledger_verify_state SET failures_json = 'not json {' WHERE source = 'host-b'`)
	require.NoError(t, err)
	got, err = d.GetLedgerVerifyState(t.Context(), "zone-a", "host-b")
	require.NoError(t, err)
	assert.Equal(t, &ledger.VerifyCheckpoint{}, got)
}

func TestVerifyZoneOnSQLite(t *testing.T) {
	d := testDB(t)
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 1, 2))
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 2, 3))

	// test_store_verify_all (store.rs:751)
	rep, err := ledger.VerifyZone(t.Context(), d, "zone-a", false)
	require.NoError(t, err)
	assert.Equal(t, 2, rep.NewlyVerified)
	assert.Empty(t, rep.Failures)
	rep, err = ledger.VerifyZone(t.Context(), d, "zone-a", false)
	require.NoError(t, err)
	assert.Equal(t, 0, rep.NewlyVerified)
	assert.Equal(t, 2, rep.Skipped)

	// Bit rot in a stored row (simulated by lifting the guard) is caught by
	// a full verify, which re-reads everything.
	_, err = d.getWriter().Exec(t.Context(), `DROP TRIGGER trg_ledger_segments_no_update`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(),
		`UPDATE ledger_segments SET events_json = replace(events_json, '"action":"test"', '"action":"tost"')
		 WHERE source_seq = 2`)
	require.NoError(t, err)
	rep, err = ledger.VerifyZone(t.Context(), d, "zone-a", false)
	require.NoError(t, err)
	assert.Empty(t, rep.Failures, "incremental verify does not re-read verified segments (store.rs:399-400)")
	rep, err = ledger.VerifyZone(t.Context(), d, "zone-a", true)
	require.NoError(t, err)
	assert.Equal(t, [][3]string{{"host-a", "2", "checksum mismatch"}}, rep.Failures)
	st, err := d.LedgerStatus(t.Context(), "zone-a")
	require.NoError(t, err)
	assert.Equal(t, rep.Failures, st.Failures)
}

func TestRebuildLedgerIndex(t *testing.T) {
	// test_rebuild_from_store (db.rs:956)
	d := testDB(t)
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 1, 2))
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 2, 1))
	mustAppend(t, d, "zone-b", testLedgerSegment(t, "host-b", 1, 4))

	events, segments, err := d.RebuildLedgerIndex(t.Context(), "zone-a")
	require.NoError(t, err)
	assert.Equal(t, 3, events)
	assert.Equal(t, 2, segments)
	a, err := d.LedgerStatus(t.Context(), "zone-a")
	require.NoError(t, err)
	assert.Equal(t, 3, a.Events)
	b, err := d.LedgerStatus(t.Context(), "zone-b")
	require.NoError(t, err)
	assert.Equal(t, 4, b.Events, "other zones are untouched")

	_, err = d.getWriter().Exec(t.Context(), `DELETE FROM ledger_events`)
	require.ErrorContains(t, err, "ledger is append-only", "the guard is restored")
}

func TestCopyLedgerFrom(t *testing.T) {
	src := testDB(t)
	a := testLedgerSegment(t, "host-a", 1, 2)
	mustAppend(t, src, "zone-a", a)
	require.NoError(t, src.SaveLedgerVerifyState(t.Context(), "zone-a", "host-a", ledger.VerifyCheckpoint{VerifiedSeq: 1}))
	require.NoError(t, src.SaveLedgerImportState(t.Context(), "zone-a", "/imports/x", `{"segments_indexed":1}`))
	srcPath := src.Path()
	require.NoError(t, src.Close())

	dst := testDB(t)
	require.NoError(t, dst.CopyLedgerFrom(srcPath))
	segs, err := dst.ListLedgerSegments(t.Context(), "zone-a", "host-a", 0, 0)
	require.NoError(t, err)
	require.Len(t, segs, 1)
	assert.True(t, segs[0].ContentMatches(a))
	st, err := dst.LedgerStatus(t.Context(), "zone-a")
	require.NoError(t, err)
	assert.Equal(t, 2, st.Events)
	ckpt, err := dst.GetLedgerVerifyState(t.Context(), "zone-a", "host-a")
	require.NoError(t, err)
	require.NotNil(t, ckpt)
	imp, err := dst.GetLedgerImportState(t.Context(), "zone-a", "/imports/x")
	require.NoError(t, err)
	require.NotNil(t, imp)
	assert.JSONEq(t, `{"segments_indexed":1}`, imp.LastReportJSON)
}

func TestLedgerWriterConcurrentAppendsNeverReuseSeq(t *testing.T) {
	d := testDB(t)
	tests := []struct {
		name      string
		exclusive func(func() error) error
	}{
		{"with_exclusive", func() func(func() error) error {
			var mu sync.Mutex
			return func(work func() error) error { mu.Lock(); defer mu.Unlock(); return work() }
		}()},
		{"without_exclusive_retries_lost_races", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zone := "zone-" + strings.ReplaceAll(tt.name, "_", "-")
			const writers = 4
			const perWriter = 5
			var wg sync.WaitGroup
			errs := make(chan error, writers*perWriter)
			for range writers {
				w := ledger.NewWriter(d, zone, "host-a", tt.exclusive)
				wg.Go(func() {
					for range perWriter {
						_, err := w.Append(t.Context(), []ledger.Event{{
							EventClass: ledger.ClassHealth, PayloadTier: ledger.TierMetadataOnly,
						}})
						errs <- err
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil && !errors.Is(err, ledger.ErrIntegrity) {
					require.NoError(t, err)
				}
			}
			seqs, err := d.LedgerSegmentSeqs(t.Context(), zone, "host-a")
			require.NoError(t, err)
			seen := map[uint64]bool{}
			for _, s := range seqs {
				assert.False(t, seen[s], "seq %d reused", s)
				seen[s] = true
			}
			if tt.exclusive != nil {
				assert.Len(t, seqs, writers*perWriter)
			}
		})
	}
}

// An archive created before the ledger existed, opened read-only (no
// migration), has no ledger tables; reads answer empty instead of failing.
func TestLedgerReadsTolerateMissingTables(t *testing.T) {
	d := testDB(t)
	for _, table := range []string{"ledger_events", "ledger_segments", "ledger_verify_state", "ledger_import_state"} {
		_, err := d.getWriter().Exec(t.Context(), "DROP TABLE "+table)
		require.NoError(t, err)
	}
	st, err := d.LedgerStatus(t.Context(), "default")
	require.NoError(t, err)
	assert.Equal(t, 0, st.Segments)
	segs, err := d.ListLedgerSegments(t.Context(), "default", "host-a", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, segs)
	seq, err := d.LatestLedgerSeq(t.Context(), "default", "host-a")
	require.NoError(t, err)
	assert.Zero(t, seq)
	ckpt, err := d.GetLedgerVerifyState(t.Context(), "default", "host-a")
	require.NoError(t, err)
	assert.Nil(t, ckpt)
	imp, err := d.GetLedgerImportState(t.Context(), "default", "/x")
	require.NoError(t, err)
	assert.Nil(t, imp)
}
