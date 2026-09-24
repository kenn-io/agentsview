package postgres

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/storage"
)

const (
	// ledgerPushWatermarkKey holds the newest pushed ingested_at, scoped
	// per target like last_push_at.
	ledgerPushWatermarkKey = "pg_ledger_push_watermark_v1"
	// LedgerPushStatusKeyPrefix + target holds the last ledger phase
	// outcome as JSON for `agentsview ledger status`.
	LedgerPushStatusKeyPrefix = "pg_ledger_push_status_v1:"
	// ledgerPushLookback re-reads recently ingested segments so a clock
	// step or a concurrent append at the watermark instant is never
	// missed; re-pushing them is a no-op.
	ledgerPushLookback = 10 * time.Minute
	ledgerPushPage     = 200
)

// LedgerPushZoneCounts counts one zone's segments in one push.
type LedgerPushZoneCounts struct {
	Pushed    int `json:"pushed"`
	Identical int `json:"identical"`
	HeldBack  int `json:"held_back"`
}

// LedgerPushStatus is the last ledger phase outcome for one target.
// Failures are [zone, source, seq, message]: local segments that fail
// their checksum, and identities the hub already holds with different
// content. They are retried on every push until they succeed.
type LedgerPushStatus struct {
	At       string                          `json:"at"`
	Zones    map[string]LedgerPushZoneCounts `json:"zones"`
	Failures [][4]string                     `json:"failures"`
}

// DecodeLedgerPushStatus parses a stored LedgerPushStatus.
func DecodeLedgerPushStatus(value string) (LedgerPushStatus, error) {
	var st LedgerPushStatus
	if err := json.Unmarshal([]byte(value), &st); err != nil {
		return LedgerPushStatus{}, fmt.Errorf("decoding ledger push status: %w", err)
	}
	return st, nil
}

// syncLedgerSegments is the ledger push phase (spec §12.4). It replaces
// jilog's spool emit + ingest (commands/spool.rs:16-69,241-561): every
// segment ingested since the per-target watermark is verified again and
// inserted with ON CONFLICT DO NOTHING; an existing identity with different
// content is reported and never overwritten. Segments in zones with
// replicate = false, and segments holding confidential-tier events unless
// replicate_confidential is set, stay local. Like the cursor usage phase it
// is global, so project-filtered pushes skip it.
func (s *Sync) syncLedgerSegments(ctx context.Context, full bool) error {
	policy := s.ledgerPolicy
	if policy == nil || s.isFiltered() {
		return nil
	}
	state := s.effectiveSyncState()
	since := ""
	if !full {
		watermark, err := state.GetSyncState(ctx, ledgerPushWatermarkKey)
		if err != nil {
			return fmt.Errorf("reading %s: %w", ledgerPushWatermarkKey, err)
		}
		if t, err := time.Parse(time.RFC3339Nano, watermark); err == nil {
			since = ledger.StorageTimestamp(t.Add(-ledgerPushLookback))
		}
	}
	statusKey := LedgerPushStatusKeyPrefix + s.syncStateTarget
	prior := LedgerPushStatus{}
	if value, err := s.local.GetSyncState(ctx, statusKey); err == nil && value != "" {
		prior, _ = DecodeLedgerPushStatus(value)
	}

	run := ledgerPushRun{
		sync:      s,
		policy:    policy,
		status:    LedgerPushStatus{At: ledger.StorageTimestamp(time.Now()), Zones: map[string]LedgerPushZoneCounts{}},
		replicate: map[string]bool{},
		seen:      map[string]bool{},
	}
	for _, z := range policy.Zones {
		run.replicate[z] = true
	}

	var cursor *db.LedgerPushCursor
	newest := ""
	for {
		page, err := s.local.ListLedgerSegmentsForPush(ctx, since, cursor, ledgerPushPage)
		if err != nil {
			return err
		}
		if err := run.push(ctx, page); err != nil {
			return err
		}
		if len(page) > 0 {
			last := page[len(page)-1]
			c := last.Cursor()
			cursor = &c
			newest = last.IngestedAt
		}
		if len(page) < ledgerPushPage {
			break
		}
	}
	for _, f := range prior.Failures {
		if run.seen[f[0]+"\x00"+f[1]+"\x00"+f[2]] {
			continue
		}
		seq, err := strconv.ParseUint(f[2], 10, 64)
		if err != nil || seq == 0 {
			continue
		}
		segs, err := s.local.ListLedgerSegments(ctx, f[0], f[1], seq-1, 1)
		if err != nil {
			return err
		}
		if len(segs) == 1 && segs[0].SourceSeq == seq {
			if err := run.push(ctx, []db.LedgerPushSegment{{Zone: f[0], Segment: segs[0]}}); err != nil {
				return err
			}
		}
	}

	if newest != "" {
		t, err := time.Parse("2006-01-02T15:04:05.000000Z", newest)
		if err != nil {
			return fmt.Errorf("parsing ledger ingested_at %q: %w", newest, err)
		}
		if err := state.SetSyncState(ctx, ledgerPushWatermarkKey, t.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("saving %s: %w", ledgerPushWatermarkKey, err)
		}
	}
	slices.SortFunc(run.status.Failures, func(a, b [4]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]), cmp.Compare(a[2], b[2]))
	})
	b, err := json.Marshal(run.status)
	if err != nil {
		return err
	}
	if err := s.local.SetSyncState(ctx, statusKey, string(b)); err != nil {
		return fmt.Errorf("saving ledger push status: %w", err)
	}
	pushed, identical, held := 0, 0, 0
	for _, c := range run.status.Zones {
		pushed += c.Pushed
		identical += c.Identical
		held += c.HeldBack
	}
	if pushed > 0 || len(run.status.Failures) > 0 {
		log.Printf("pgsync: ledger: %d segment(s) pushed, %d already present, %d held back, %d refused",
			pushed, identical, held, len(run.status.Failures))
	}
	for _, f := range run.status.Failures {
		log.Printf("pgsync: ledger: %s %s-%s refused: %s", f[0], f[1], f[2], f[3])
	}
	return nil
}

type ledgerPushRun struct {
	sync      *Sync
	policy    *storage.LedgerPushPolicy
	status    LedgerPushStatus
	replicate map[string]bool
	seen      map[string]bool
}

func (r *ledgerPushRun) fail(zone string, seg ledger.Segment, msg string) {
	r.status.Failures = append(r.status.Failures,
		[4]string{zone, seg.Source, strconv.FormatUint(seg.SourceSeq, 10), msg})
}

// push inserts one page in one transaction. Per-segment problems are
// recorded as failures; only database errors abort the phase.
func (r *ledgerPushRun) push(ctx context.Context, page []db.LedgerPushSegment) error {
	var prepared []ledger.PreparedSegment
	for _, p := range page {
		r.seen[p.Zone+"\x00"+p.Segment.Source+"\x00"+strconv.FormatUint(p.Segment.SourceSeq, 10)] = true
		counts := r.status.Zones[p.Zone]
		if !r.replicate[p.Zone] || (!r.policy.ReplicateConfidential && holdsConfidential(p.Segment)) {
			counts.HeldBack++
			r.status.Zones[p.Zone] = counts
			continue
		}
		prep, err := ledger.PrepareAppend(p.Zone, p.Segment, ledger.OriginPush)
		if err != nil {
			r.fail(p.Zone, p.Segment, err.Error())
			continue
		}
		prepared = append(prepared, prep)
	}
	if len(prepared) == 0 {
		return nil
	}
	tx, err := r.sync.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning ledger push tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, prep := range prepared {
		outcome, err := appendPreparedLedgerSegmentTx(ctx, tx, prep)
		counts := r.status.Zones[prep.Zone]
		switch {
		case err == nil && outcome == ledger.AlreadyIdentical:
			counts.Identical++
		case err == nil:
			counts.Pushed++
		case errors.Is(err, ledger.ErrIntegrity):
			r.status.Failures = append(r.status.Failures,
				[4]string{prep.Zone, prep.Source, strconv.FormatInt(prep.Seq, 10), err.Error()})
		default:
			return err
		}
		r.status.Zones[prep.Zone] = counts
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing ledger push: %w", err)
	}
	return nil
}

func holdsConfidential(seg ledger.Segment) bool {
	for _, e := range seg.Events {
		if e.PayloadTier == ledger.TierConfidential {
			return true
		}
	}
	return false
}
