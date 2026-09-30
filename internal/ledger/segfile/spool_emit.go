package segfile

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/agentsview/internal/ledger"
)

// ErrEmitFailures is run_emit's final error (spool.rs:452-454).
var ErrEmitFailures = errors.New("spool emit finished with per-segment failures (see stderr)")

const spoolEmitPageSize = 256

// beforeSpoolPublish is a test seam that runs after emit's existence check
// and before the writer publishes, to model a file-sync race.
var beforeSpoolPublish = func(string) {}

// EmitReport is one zone's emit result. Failures are the stderr lines, in
// the order jilog prints them.
type EmitReport struct {
	Emitted  int
	Skipped  int
	Cursor   uint64
	Failures []string
}

// Line renders the per-zone summary (spool.rs:447-450).
func (r EmitReport) Line(zone, source string) string {
	return fmt.Sprintf("spool emit [%s]: source=%s emitted=%d skipped=%d cursor=%d",
		zone, source, r.Emitted, r.Skipped, r.Cursor)
}

// CursorPath is {dir}/{zone}/{source}.json (spool.rs:214-220). A directory
// per zone keeps dashed names unambiguous.
func CursorPath(cursorDir, zone, source string) string {
	return filepath.Join(cursorDir, zone, source+".json")
}

type emitCursor struct {
	LastEmittedSeq *uint64 `json:"last_emitted_seq"`
}

// readCursor returns (seq, present, valid). A missing file is (0, false, true).
func readCursor(path string) (uint64, bool, bool) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, true
	}
	if err != nil {
		return 0, true, false
	}
	var c emitCursor
	if err := json.Unmarshal(b, &c); err != nil || c.LastEmittedSeq == nil {
		return 0, true, false
	}
	return *c.LastEmittedSeq, true, true
}

// LoadCursor returns the recorded high-water mark; missing or corrupt reads
// as 0 (spool.rs:222-229). The cursor is status only, never correctness.
func LoadCursor(path string) uint64 {
	seq, _, _ := readCursor(path)
	return seq
}

// storeCursor writes serde's pretty form via <file>.json.tmp + rename,
// without fsync (spool.rs:231-239).
func storeCursor(path string, seq uint64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, fmt.Appendf(nil, "{\n  \"last_emitted_seq\": %d\n}", seq), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// listOwnSegments reads every (zone, source) segment in seq order, stopping
// at the first page error.
func listOwnSegments(ctx context.Context, src Lister, zone, source string) ([]ledger.Segment, error) {
	var out []ledger.Segment
	var after uint64
	for {
		page, err := src.ListLedgerSegments(ctx, zone, source, after, spoolEmitPageSize)
		if err != nil {
			return out, fmt.Errorf("listing %s segments after seq %d: %w", source, after, err)
		}
		if len(page) == 0 {
			return out, nil
		}
		out = append(out, page...)
		after = page[len(page)-1].SourceSeq
	}
}

// SpoolEmit copies this source's committed segments into
// <spoolDir>/incoming/ (spool.rs:241-456). Every run examines every own
// segment: a segment missing from both incoming/ and processed/ is
// published, and every existing copy is content-compared, never trusted by
// name. The cursor advances only through contiguous successes. It returns an
// error only for an invalid source or a failed cursor write; per-segment
// failures are in the report.
func SpoolEmit(
	ctx context.Context, src Lister, zone, source, spoolDir, cursorDir string,
) (EmitReport, error) {
	rep := EmitReport{Failures: []string{}}
	if !ledger.ValidSourceName(source) {
		return rep, &InvalidSourceError{Name: source}
	}
	cpath := CursorPath(cursorDir, zone, source)
	rep.Cursor = LoadCursor(cpath)
	segs, listErr := listOwnSegments(ctx, src, zone, source)
	advance := listErr == nil
	if listErr != nil {
		rep.Failures = append(rep.Failures, fmt.Sprintf("spool emit [%s]: %v", zone, listErr))
	}
	fail := func(msg string) {
		rep.Failures = append(rep.Failures, msg)
		advance = false
	}
	var incomingRoot, processedRoot *os.Root
	var spoolDirsErr error
	if len(segs) > 0 {
		var spoolRoot *os.Root
		spoolRoot, spoolDirsErr = ensureSpoolRoot(spoolDir)
		if spoolDirsErr == nil {
			defer spoolRoot.Close()
			incomingRoot, spoolDirsErr = openSpoolChild(spoolRoot, SpoolIncomingDir, true)
			if spoolDirsErr == nil {
				syncSpoolDirBestEffort(spoolRoot)
			}
			if spoolDirsErr == nil {
				processedRoot, spoolDirsErr = openSpoolChild(spoolRoot, SpoolProcessedDir, false)
			}
			if incomingRoot != nil {
				defer incomingRoot.Close()
			}
			if processedRoot != nil {
				defer processedRoot.Close()
			}
		}
	}
	for _, seg := range segs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		seq := seg.SourceSeq
		if !ledger.ValidSourceName(seg.Source) || seg.Source != source {
			fail(fmt.Sprintf("spool emit: %s-%06d identity mismatch: file claims source=%s seq=%d — refusing to spool",
				source, seq, rustDebugString(seg.Source), seg.SourceSeq))
			continue
		}
		valid, err := seg.Verify()
		if err != nil {
			fail(fmt.Sprintf("spool emit: %s-%06d verify error: %v", source, seq, err))
			continue
		}
		if !valid {
			fail(fmt.Sprintf("spool emit: %s-%06d fails checksum verification — refusing to spool corrupt segment", source, seq))
			continue
		}
		fname := seg.Filename()
		if spoolDirsErr != nil {
			fail(fmt.Sprintf("spool emit: opening spool directories failed: %v", spoolDirsErr))
			continue
		}
		var copies []struct {
			root *os.Root
			path string
		}
		badDirectoryEntry := false
		for _, entry := range []struct {
			root *os.Root
			path string
		}{
			{processedRoot, filepath.Join(spoolDir, SpoolProcessedDir, fname)},
			{incomingRoot, filepath.Join(spoolDir, SpoolIncomingDir, fname)},
		} {
			exists, err := spoolFileExists(entry.root, fname)
			if err != nil {
				fail(fmt.Sprintf("spool emit: checking spool copy %s failed: %v", entry.path, err))
				badDirectoryEntry = true
				continue
			}
			if exists {
				copies = append(copies, entry)
			}
		}
		if badDirectoryEntry {
			continue
		}
		if len(copies) == 0 {
			beforeSpoolPublish(filepath.Join(spoolDir, SpoolIncomingDir, fname))
			outcome, err := publishNewInRoot(incomingRoot, fname, seg)
			if err != nil {
				fail(fmt.Sprintf("spool emit: spool-write %s failed: ledger error: %v", fname, err))
				continue
			}
			if outcome == ledger.AlreadyIdentical {
				rep.Skipped++
			} else {
				rep.Emitted++
			}
		} else {
			bad := false
			for _, existingCopy := range copies {
				prior, _, err := readSegmentFileFromRoot(existingCopy.root, fname)
				switch {
				case err != nil:
					fail(fmt.Sprintf("spool emit: spool copy %s unreadable (%v); refusing to assume it matches local %s", existingCopy.path, err, fname))
					bad = true
				case !prior.ContentMatches(seg):
					fail(fmt.Sprintf("spool emit: %s conflicts with local %s: same identity, DIFFERENT content — possible hostname collision or reinitialized ledger; not overwriting", existingCopy.path, fname))
					bad = true
				}
			}
			if bad {
				continue
			}
			rep.Skipped++
		}
		if advance && seq > rep.Cursor {
			rep.Cursor = seq
		}
	}
	if err := storeCursor(cpath, rep.Cursor); err != nil {
		return rep, fmt.Errorf("store spool cursor %s: %w", cpath, err)
	}
	return rep, nil
}
