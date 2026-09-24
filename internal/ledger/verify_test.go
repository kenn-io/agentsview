package ledger

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSource models one source's segments for the verify ports: seq ->
// "" (clean) or a failure message.
type fakeSource map[uint64]string

func (f fakeSource) seqs() []uint64 {
	out := make([]uint64, 0, len(f))
	for s := range f {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func (f fakeSource) verify(ckpt VerifyCheckpoint) (VerifyCheckpoint, VerifyReport) {
	return VerifySource("host-a", f.seqs(), ckpt, func(seq uint64) string { return f[seq] })
}

// Ports jilog store.rs:765-967 onto the per-source pure function.
func TestVerifySource(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"test_verify_incremental_first_run_checks_all_and_writes_checkpoint", func(t *testing.T) {
			t.Helper()
			src := fakeSource{1: "", 2: ""}
			ckpt, rep := src.verify(VerifyCheckpoint{})
			assert.Equal(t, 2, rep.NewlyVerified)
			assert.Equal(t, 0, rep.Skipped)
			assert.Empty(t, rep.Failures)
			assert.Equal(t, uint64(2), ckpt.VerifiedSeq)
		}},
		{"test_verify_incremental_skips_previously_verified", func(t *testing.T) {
			t.Helper()
			src := fakeSource{1: ""}
			ckpt, _ := src.verify(VerifyCheckpoint{})
			_, rep := src.verify(ckpt)
			assert.Equal(t, 0, rep.NewlyVerified)
			assert.Equal(t, 1, rep.Skipped)
			assert.Empty(t, rep.Failures)
		}},
		{"test_verify_incremental_checks_only_new_segments", func(t *testing.T) {
			t.Helper()
			src := fakeSource{1: "", 2: ""}
			ckpt, _ := src.verify(VerifyCheckpoint{})
			src[3] = ""
			_, rep := src.verify(ckpt)
			assert.Equal(t, 1, rep.NewlyVerified)
			assert.Equal(t, 2, rep.Skipped)
		}},
		{"test_verify_incremental_reports_corrupt_new_segment", func(t *testing.T) {
			t.Helper()
			src := fakeSource{1: ""}
			ckpt, _ := src.verify(VerifyCheckpoint{})
			src[2] = "checksum mismatch"
			_, rep := src.verify(ckpt)
			assert.Equal(t, 0, rep.NewlyVerified)
			require.Len(t, rep.Failures, 1)
			assert.Equal(t, [3]string{"host-a", "2", "checksum mismatch"}, rep.Failures[0])
		}},
		{"test_verify_incremental_rechecks_failure_until_repaired", func(t *testing.T) {
			t.Helper()
			src := fakeSource{1: ""}
			ckpt, _ := src.verify(VerifyCheckpoint{})
			src[2] = "checksum mismatch"
			ckpt, _ = src.verify(ckpt)
			ckpt, rep := src.verify(ckpt)
			assert.Len(t, rep.Failures, 1, "remembered and re-checked")
			assert.Equal(t, 0, rep.NewlyVerified)
			src[2] = ""
			_, rep = src.verify(ckpt)
			assert.Empty(t, rep.Failures)
			assert.Equal(t, 1, rep.NewlyVerified)
		}},
		{"test_verify_incremental_corrupt_checkpoint_reverifies_all", func(t *testing.T) {
			t.Helper()
			// A corrupt stored checkpoint is decoded as the zero value by
			// the store (PR 13); the zero value re-verifies everything.
			src := fakeSource{1: "", 2: ""}
			_, rep := src.verify(VerifyCheckpoint{})
			assert.Equal(t, 2, rep.NewlyVerified)
			assert.Equal(t, 0, rep.Skipped)
		}},
		{"test_verify_incremental_verifies_backfilled_gap_segment", func(t *testing.T) {
			t.Helper()
			src := fakeSource{1: "", 3: ""}
			ckpt, _ := src.verify(VerifyCheckpoint{})
			assert.Equal(t, [][2]string{{"host-a", "2"}}, ckpt.Missing)
			src[2] = ""
			ckpt, rep := src.verify(ckpt)
			assert.Equal(t, 1, rep.NewlyVerified)
			assert.Equal(t, 2, rep.Skipped)
			assert.Empty(t, rep.Failures)
			assert.Empty(t, ckpt.Missing)

			other := fakeSource{1: "", 3: ""}
			ckpt2, _ := other.verify(VerifyCheckpoint{})
			other[2] = "checksum mismatch"
			_, rep = other.verify(ckpt2)
			require.Len(t, rep.Failures, 1)
			assert.Equal(t, "2", rep.Failures[0][1])
		}},
		{"test_checkpoint_without_missing_field_still_parses", func(t *testing.T) {
			t.Helper()
			src := fakeSource{1: ""}
			_, rep := src.verify(VerifyCheckpoint{VerifiedSeq: 1})
			assert.Equal(t, 0, rep.NewlyVerified, "old checkpoint must still be honored")
			assert.Equal(t, 1, rep.Skipped)
		}},
		{"test_verify_full_rechecks_everything_and_resets_checkpoint", func(t *testing.T) {
			t.Helper()
			src := fakeSource{1: "", 2: ""}
			_, _ = src.verify(VerifyCheckpoint{})
			ckpt, rep := src.verify(VerifyCheckpoint{}) // full = zero checkpoint
			assert.Equal(t, 2, rep.NewlyVerified)
			assert.Equal(t, 0, rep.Skipped)
			_, rep = src.verify(ckpt)
			assert.Equal(t, 0, rep.NewlyVerified)
			assert.Equal(t, 2, rep.Skipped)
		}},
		{"watermark_passes_a_failed_seq_like_jilog", func(t *testing.T) {
			t.Helper()
			src := fakeSource{1: "", 2: "read error: gone", 3: ""}
			ckpt, rep := src.verify(VerifyCheckpoint{})
			assert.Equal(t, uint64(3), ckpt.VerifiedSeq)
			assert.Len(t, rep.Failures, 1)
			assert.Equal(t, ckpt.Failures, rep.Failures)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestDetectGaps(t *testing.T) {
	tests := []struct {
		name string
		seqs []uint64
		want []uint64
	}{
		{"test_store_detect_gaps", []uint64{1, 3}, []uint64{2}},
		{"leading_gap_starts_at_1", []uint64{3}, []uint64{1, 2}},
		{"none", []uint64{1, 2, 3}, nil},
		{"empty", nil, nil},
		{"unsorted_and_duplicates", []uint64{4, 1, 4}, []uint64{2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DetectGaps(tt.seqs))
		})
	}
	t.Run("huge_seq_is_capped", func(t *testing.T) {
		got := DetectGaps([]uint64{1, 1 << 40})
		assert.Len(t, got, MaxGapsPerSource)
		assert.Equal(t, uint64(2), got[0])
	})
}
