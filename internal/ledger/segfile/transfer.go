package segfile

import (
	"context"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"go.kenn.io/agentsview/internal/ledger"
)

// Appender is the storage ImportDir writes into (*db.DB, *postgres.Store).
type Appender interface {
	AppendLedgerSegment(ctx context.Context, zone string, seg ledger.Segment, origin string) (ledger.PublishOutcome, error)
	LedgerSegmentSeqs(ctx context.Context, zone, source string) ([]uint64, error)
	LedgerStatus(ctx context.Context, zone string) (ledger.ZoneStatus, error)
}

// Lister is the storage ExportZone reads from.
type Lister interface {
	LedgerStatus(ctx context.Context, zone string) (ledger.ZoneStatus, error)
	ListLedgerSegments(ctx context.Context, zone, source string, afterSeq uint64, limit int) ([]ledger.Segment, error)
}

// ImportReport ports IndexRefreshReport (ledger-sqlite db.rs:41-55).
// Failed entries are [source, seq, message]; listing errors have no
// segment identity and are kept apart.
type ImportReport struct {
	EventsIndexed   int         `json:"events_indexed"`
	SegmentsIndexed int         `json:"segments_indexed"`
	Skipped         int         `json:"skipped"`
	Failed          [][3]string `json:"failed"`
	ListingErrors   []string    `json:"listing_errors"`
}

// ExportReport summarizes ExportZone. Failed entries are [file, message].
type ExportReport struct {
	Written   int         `json:"written"`
	Identical int         `json:"identical"`
	Failed    [][2]string `json:"failed"`
}

const exportPage = 500

// ImportDir ports refresh_from_store (db.rs:299-395) with agentsview's
// storage as the index: segments already stored are skipped without being
// read; each new file is read, identity-checked against its name,
// verified and appended with origin "import". Failures are recorded and
// retried on the next run because nothing marks them imported. The
// returned error is only for storage failures that stop the whole run.
func ImportDir(ctx context.Context, dir, zone string, dst Appender) (ImportReport, error) {
	report := ImportReport{Failed: [][3]string{}, ListingErrors: []string{}}
	entries, listing := Store{Dir: dir}.List()
	report.ListingErrors = append(report.ListingErrors, listing...)
	before, err := dst.LedgerStatus(ctx, zone)
	if err != nil {
		return report, err
	}
	known := map[string]map[uint64]bool{}
	fail := func(e Entry, msg string) {
		report.Failed = append(report.Failed, [3]string{e.Source, strconv.FormatUint(e.Seq, 10), msg})
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if e.Seq > math.MaxInt64 {
			fail(e, fmt.Sprintf("source_seq exceeds i64::MAX (%d) — not representable in the ledger tables", int64(math.MaxInt64)))
			continue
		}
		seqs, ok := known[e.Source]
		if !ok {
			stored, err := dst.LedgerSegmentSeqs(ctx, zone, e.Source)
			if err != nil {
				return report, err
			}
			seqs = make(map[uint64]bool, len(stored))
			for _, s := range stored {
				seqs[s] = true
			}
			known[e.Source] = seqs
		}
		if seqs[e.Seq] {
			report.Skipped++
			continue
		}
		seg, err := ReadFile(e.Path)
		if err != nil {
			fail(e, fmt.Sprintf("read error: %v", err))
			continue
		}
		if seg.Source != e.Source || seg.SourceSeq != e.Seq {
			fail(e, fmt.Sprintf("identity mismatch: file claims source=%q seq=%d", seg.Source, seg.SourceSeq))
			continue
		}
		valid, err := seg.Verify()
		if err != nil {
			fail(e, fmt.Sprintf("verify error: %v", err))
			continue
		}
		if !valid {
			fail(e, "checksum mismatch")
			continue
		}
		outcome, err := dst.AppendLedgerSegment(ctx, zone, seg, ledger.OriginImport)
		if err != nil {
			fail(e, fmt.Sprintf("projection error: %v", err))
			continue
		}
		seqs[e.Seq] = true
		if outcome == ledger.AlreadyIdentical {
			report.Skipped++
			continue
		}
		report.SegmentsIndexed++
	}
	after, err := dst.LedgerStatus(ctx, zone)
	if err != nil {
		return report, err
	}
	report.EventsIndexed = after.Events - before.Events
	return report, nil
}

// ExportZone writes a zone's segments as jilog files into dir with
// PublishNew, so a re-export is a no-op and a conflicting file is reported,
// never replaced. source "" exports every source.
func ExportZone(ctx context.Context, src Lister, zone, dir, source string) (ExportReport, error) {
	report := ExportReport{Failed: [][2]string{}}
	var sources []string
	if source != "" {
		if !ledger.ValidSourceName(source) {
			return report, fmt.Errorf("invalid segment source %q: must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", source)
		}
		sources = []string{source}
	} else {
		st, err := src.LedgerStatus(ctx, zone)
		if err != nil {
			return report, err
		}
		sources = slices.Sorted(maps.Keys(st.Sources))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return report, err
	}
	for _, s := range sources {
		var after uint64
		for {
			segs, err := src.ListLedgerSegments(ctx, zone, s, after, exportPage)
			if err != nil {
				return report, err
			}
			for _, seg := range segs {
				outcome, err := PublishNew(filepath.Join(dir, seg.Filename()), seg)
				switch {
				case err != nil:
					report.Failed = append(report.Failed, [2]string{seg.Filename(), err.Error()})
				case outcome == ledger.AlreadyIdentical:
					report.Identical++
				default:
					report.Written++
				}
				after = seg.SourceSeq
			}
			if len(segs) < exportPage {
				break
			}
		}
	}
	return report, nil
}
