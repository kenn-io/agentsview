package ledger

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// VerifyStore is what VerifyZone reads and writes. *db.DB and
// *postgres.Store implement it.
type VerifyStore interface {
	LedgerStatus(ctx context.Context, zone string) (ZoneStatus, error)
	LedgerSegmentSeqs(ctx context.Context, zone, source string) ([]uint64, error)
	ListLedgerSegments(ctx context.Context, zone, source string, afterSeq uint64, limit int) ([]Segment, error)
	GetLedgerVerifyState(ctx context.Context, zone, source string) (*VerifyCheckpoint, error)
	SaveLedgerVerifyState(ctx context.Context, zone, source string, c VerifyCheckpoint) error
}

// VerifyZone ports verify_incremental / verify_full (store.rs:401-492)
// over stored segments, one checkpoint per source. full ignores the
// stored checkpoints and rewrites them from the results.
func VerifyZone(ctx context.Context, st VerifyStore, zone string, full bool) (VerifyReport, error) {
	status, err := st.LedgerStatus(ctx, zone)
	if err != nil {
		return VerifyReport{}, err
	}
	var total VerifyReport
	for _, source := range slices.Sorted(maps.Keys(status.Sources)) {
		var ckpt VerifyCheckpoint
		if !full {
			stored, err := st.GetLedgerVerifyState(ctx, zone, source)
			if err != nil {
				return VerifyReport{}, err
			}
			if stored != nil {
				ckpt = *stored
			}
		}
		seqs, err := st.LedgerSegmentSeqs(ctx, zone, source)
		if err != nil {
			return VerifyReport{}, err
		}
		next, rep := VerifySource(source, seqs, ckpt, func(seq uint64) string {
			return checkStoredSegment(ctx, st, zone, source, seq)
		})
		if err := st.SaveLedgerVerifyState(ctx, zone, source, next); err != nil {
			return VerifyReport{}, err
		}
		total.NewlyVerified += rep.NewlyVerified
		total.Skipped += rep.Skipped
		total.Failures = append(total.Failures, rep.Failures...)
	}
	return total, nil
}

// checkStoredSegment returns jilog's verify failure texts (store.rs:346-353).
func checkStoredSegment(ctx context.Context, st VerifyStore, zone, source string, seq uint64) string {
	segs, err := st.ListLedgerSegments(ctx, zone, source, seq-1, 1)
	if err != nil {
		return fmt.Sprintf("read error: %v", err)
	}
	if len(segs) == 0 || segs[0].SourceSeq != seq {
		return "read error: segment not found"
	}
	ok, err := segs[0].Verify()
	if err != nil {
		return fmt.Sprintf("verify error: %v", err)
	}
	if !ok {
		return "checksum mismatch"
	}
	return ""
}
