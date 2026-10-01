package segfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/ledger"
)

// Spool layout (ledger-spool lib.rs:10-19): producers publish into
// <spool>/incoming/; the authority moves ingested files to <spool>/processed/,
// the audit trail that also stops producers from re-spooling.
const (
	SpoolIncomingDir  = "incoming"
	SpoolProcessedDir = "processed"
	// syncConflictMarker names the conflict copies some file-sync tools
	// leave beside a file (for example `x.sync-conflict-<stamp>.json`).
	// They can never pass the identity check, so they are skipped with a
	// warning instead of failing every run (ingester.rs:148-160).
	syncConflictMarker = ".sync-conflict-"
)

// afterSpoolClaim is a test seam for producers that publish a replacement
// under the original incoming filename while ingest owns a claimed entry.
var afterSpoolClaim = func(string) {}

// InvalidSourceError ports SpoolError::InvalidSource (error.rs:25-28).
type InvalidSourceError struct{ Name string }

func (e *InvalidSourceError) Error() string {
	return fmt.Sprintf(
		"invalid segment source %s: must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$",
		rustDebugString(e.Name),
	)
}

// IdentityMismatchError ports SpoolError::IdentityMismatch (error.rs:30-34).
type IdentityMismatchError struct{ Found, Expected string }

func (e *IdentityMismatchError) Error() string {
	return fmt.Sprintf(
		"spool filename %s does not match segment identity %s (path-traversal / spoof guard)",
		rustDebugString(e.Found), rustDebugString(e.Expected),
	)
}

// IntegrityError ports SpoolError::IntegrityFailure (error.rs:18-23).
type IntegrityError struct {
	Src    string
	Seq    uint64
	Reason string
}

func (e *IntegrityError) Error() string {
	return fmt.Sprintf("integrity check failed for %s:%d: %s", e.Src, e.Seq, e.Reason)
}

// Unwrap lets callers match spool integrity failures with ledger.ErrIntegrity.
func (e *IntegrityError) Unwrap() error { return ledger.ErrIntegrity }

// readSegmentFile ports Segment::read_from_file (segment.rs:409-414): it
// does not verify. Errors carry LedgerError's display prefixes.
func readSegmentFile(path string) (ledger.Segment, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ledger.Segment{}, fmt.Errorf("I/O error: %w", err)
	}
	seg, err := parseSegmentFile(b)
	if err != nil {
		return ledger.Segment{}, fmt.Errorf("serialization error: %w", err)
	}
	return seg, nil
}

func readSegmentFileFromRoot(root *os.Root, name string) (ledger.Segment, []byte, error) {
	b, err := readSpoolFile(root, name)
	if err != nil {
		return ledger.Segment{}, b, err
	}
	seg, err := parseSegmentFile(b)
	if err != nil {
		return ledger.Segment{}, b, fmt.Errorf("serialization error: %w", err)
	}
	return seg, b, nil
}

func parseSegmentFile(b []byte) (ledger.Segment, error) {
	return ledger.ParseSegmentFile(b)
}

// SpoolWriter publishes sealed segments into <spool>/incoming/ (writer.rs:20-95).
type SpoolWriter struct{ spoolRoot string }

// NewSpoolWriter targets <spoolRoot>/incoming.
func NewSpoolWriter(spoolRoot string) SpoolWriter {
	return SpoolWriter{spoolRoot: spoolRoot}
}

// Write publishes seg with no-clobber semantics. The source is validated
// before any path is built: the filename derives from it, so this is the
// path-traversal guard (writer.rs:61-70). An identical existing copy is
// AlreadyIdentical; a different one is an error and both files stay intact.
func (w SpoolWriter) Write(seg ledger.Segment) (string, ledger.PublishOutcome, error) {
	if !ledger.ValidSourceName(seg.Source) {
		return "", ledger.Published, &InvalidSourceError{Name: seg.Source}
	}
	spoolRoot, err := ensureSpoolRoot(w.spoolRoot)
	if err != nil {
		return "", ledger.Published, fmt.Errorf("I/O error: %w", err)
	}
	defer spoolRoot.Close()
	incoming, err := openSpoolChild(spoolRoot, SpoolIncomingDir, true)
	if err != nil {
		return "", ledger.Published, fmt.Errorf("I/O error: %w", err)
	}
	defer incoming.Close()
	syncSpoolDirBestEffort(spoolRoot)
	path := filepath.Join(w.spoolRoot, SpoolIncomingDir, seg.Filename())
	outcome, err := publishNewInRoot(incoming, seg.Filename(), seg)
	if err != nil {
		return "", ledger.Published, fmt.Errorf("ledger error: %w", err)
	}
	return path, outcome, nil
}

// IngestFailure is one (filename, message) pair of IngestReport.failed
// (ingester.rs:44-45).
type IngestFailure struct {
	File string `json:"file"`
	Err  string `json:"error"`
}

// IngestReport ports IngestReport (ingester.rs:36-79).
type IngestReport struct {
	Committed []string
	Skipped   []string
	Failed    []IngestFailure
}

// Total is committed + skipped + failed (ingester.rs:57-60).
func (r IngestReport) Total() int {
	return len(r.Committed) + len(r.Skipped) + len(r.Failed)
}

// Summary renders print_summary (ingester.rs:63-78) byte-for-byte,
// including each println's trailing newline.
func (r IngestReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Spool ingest: %d committed, %d skipped, %d failed (%d total)\n",
		len(r.Committed), len(r.Skipped), len(r.Failed), r.Total())
	if len(r.Failed) > 0 {
		b.WriteString("\nFailures:\n")
		for _, f := range r.Failed {
			fmt.Fprintf(&b, "  %s -- %s\n", f.File, f.Err)
		}
	}
	return b.String()
}

type ingestOutcome int

const (
	ingestCommitted ingestOutcome = iota
	ingestDuplicate
)

// SpoolIngest commits every valid segment in <spoolDir>/incoming/ into dst
// (origin "spool") and moves each ingested file to <spoolDir>/processed/
// (ingester.rs:102-246). It processes all files and collects failures; only
// a missing incoming/ directory is a no-op. Invalid spool directories abort.
func SpoolIngest(ctx context.Context, spoolDir, zone string, dst Appender) (IngestReport, error) {
	report := IngestReport{Committed: []string{}, Skipped: []string{}, Failed: []IngestFailure{}}
	spoolRoot, err := openSpoolRoot(spoolDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Rust's Path::exists is false for a missing incoming path
			// (ingester.rs:130-133).
			return report, nil
		}
		return report, fmt.Errorf("I/O error: %w", err)
	}
	defer spoolRoot.Close()
	incoming, err := openSpoolChild(spoolRoot, SpoolIncomingDir, false)
	if err != nil {
		return report, fmt.Errorf("I/O error: %w", err)
	}
	if incoming == nil {
		return report, nil
	}
	defer incoming.Close()
	processed, err := openSpoolChild(spoolRoot, SpoolProcessedDir, true)
	if err != nil {
		return report, fmt.Errorf("I/O error: %w", err)
	}
	defer processed.Close()
	if err := recoverSpoolClaims(incoming, &report); err != nil {
		return report, err
	}
	names, err := spoolJSONNames(incoming, &report)
	if err != nil {
		return report, err
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		claim, err := claimSpoolFile(incoming, name)
		if err != nil {
			report.Failed = append(report.Failed, IngestFailure{File: name, Err: err.Error()})
			continue
		}
		syncSpoolDirBestEffort(incoming)
		afterSpoolClaim(claim)
		outcome, seg, data, err := ingestOne(ctx, incoming, claim, name, zone, dst)
		if err != nil {
			log.Printf("ledger spool: ingest of %s failed: %v", name, err)
			if restoreErr := restoreSpoolClaim(incoming, claim, name); restoreErr != nil {
				err = fmt.Errorf("%w; incoming claim preserved at %s: %w", err, claim, restoreErr)
			}
			report.Failed = append(report.Failed, IngestFailure{File: name, Err: err.Error()})
			continue
		}
		if msg, ok := moveNoClobber(incoming, processed, claim, name, seg, data); !ok {
			what := "committed to store"
			if outcome == ingestDuplicate {
				what = "duplicate of store copy"
			}
			log.Printf("ledger spool: failed to move %s to processed/: %s", name, msg)
			if restoreErr := restoreSpoolClaim(incoming, claim, name); restoreErr != nil {
				msg += fmt.Sprintf("; incoming claim preserved at %s: %v", claim, restoreErr)
			}
			report.Failed = append(report.Failed, IngestFailure{File: name, Err: what + " but " + msg})
			continue
		}
		// Directory sync failures are best-effort, as in ingester.rs:199-210.
		syncSpoolDirBestEffort(processed)
		syncSpoolDirBestEffort(incoming)
		if outcome == ingestCommitted {
			report.Committed = append(report.Committed, name)
		} else {
			report.Skipped = append(report.Skipped, name)
		}
	}
	return report, nil
}

// spoolJSONNames lists *.json entries of dir sorted by name, skipping
// file-sync conflict copies with a warning (ingester.rs:139-173). Go's
// ReadDir reports an entry failure as one error after the readable entries;
// it becomes jilog's "(unreadable directory entry)" failure.
func spoolJSONNames(dir *os.Root, report *IngestReport) ([]string, error) {
	f, err := dir.Open(".")
	if err != nil {
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	defer f.Close()
	entries, readErr := f.ReadDir(-1)
	if readErr != nil {
		report.Failed = append(report.Failed, IngestFailure{
			File: "(unreadable directory entry)",
			Err:  fmt.Sprintf("read_dir entry error in incoming/: %v", readErr),
		})
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !hasRustJSONExtension(name) {
			continue
		}
		if strings.Contains(name, syncConflictMarker) {
			log.Printf("ledger spool: ignoring file-sync conflict copy %s in incoming/ — inspect and remove it manually", name)
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// hasRustJSONExtension matches Rust's `path.extension() == Some("json")`:
// the extension follows the last '.', and a name whose only '.' is the
// leading one (".json") has no extension.
func hasRustJSONExtension(name string) bool {
	i := strings.LastIndexByte(name, '.')
	return i > 0 && name[i+1:] == "json"
}

func ingestOne(
	ctx context.Context, dir *os.Root, storedName, name, zone string, dst Appender,
) (ingestOutcome, ledger.Segment, []byte, error) {
	data, err := readSpoolFile(dir, storedName)
	if err != nil {
		return 0, ledger.Segment{}, data, fmt.Errorf("ledger error: %w", err)
	}
	seg, err := parseSegmentFile(data)
	if err != nil {
		return 0, seg, data, fmt.Errorf("ledger error: serialization error: %w", err)
	}
	if !ledger.ValidSourceName(seg.Source) {
		return 0, seg, data, &InvalidSourceError{Name: seg.Source}
	}
	if expected := seg.Filename(); name != expected {
		return 0, seg, data, &IdentityMismatchError{Found: name, Expected: expected}
	}
	valid, err := seg.Verify()
	if err != nil {
		return 0, seg, data, fmt.Errorf("ledger error: serialization error: %w", err)
	}
	if !valid {
		return 0, seg, data, &IntegrityError{Src: seg.Source, Seq: seg.SourceSeq, Reason: "checksum mismatch"}
	}
	outcome, err := dst.AppendLedgerSegment(ctx, zone, seg, ledger.OriginSpool)
	switch {
	case err == nil && outcome == ledger.AlreadyIdentical:
		return ingestDuplicate, seg, data, nil
	case err == nil:
		return ingestCommitted, seg, data, nil
	case errors.Is(err, ledger.ErrIntegrity):
		return 0, seg, data, storeConflict(ctx, dst, zone, seg)
	default:
		return 0, seg, data, fmt.Errorf("ledger error: %w", err)
	}
}

// storeConflict ports the DuplicateSegment branch (ingester.rs:327-357): a
// store copy that fails its own verify is reported as corrupt instead of
// being trusted as "different".
func storeConflict(ctx context.Context, dst Appender, zone string, seg ledger.Segment) error {
	if lister, ok := dst.(Lister); ok && seg.SourceSeq > 0 {
		copies, err := lister.ListLedgerSegments(ctx, zone, seg.Source, seg.SourceSeq-1, 1)
		if err != nil {
			return fmt.Errorf("ledger error: %w", err)
		}
		if len(copies) == 1 && copies[0].SourceSeq == seg.SourceSeq {
			valid, err := copies[0].Verify()
			if err != nil {
				return fmt.Errorf("ledger error: serialization error: %w", err)
			}
			if !valid {
				return &IntegrityError{
					Src: seg.Source, Seq: seg.SourceSeq,
					Reason: "store copy of this identity fails checksum verification — corrupt store copy; left in incoming/",
				}
			}
		}
	}
	return &IntegrityError{
		Src: seg.Source, Seq: seg.SourceSeq,
		Reason: "duplicate identity with DIFFERENT content — possible hostname collision or corrupt store copy; left in incoming/",
	}
}

// moveNoClobber copies the exact segment bytes into a new destination entry
// relative to the pinned processed root, then removes the incoming entry. An
// existing destination is content-checked and never overwritten.
func moveNoClobber(src, dest *os.Root, sourceName, name string, seg ledger.Segment, data []byte) (string, bool) {
	err := writeNoClobberInRoot(dest, name, data)
	if err == nil {
		if rerr := src.Remove(sourceName); rerr != nil {
			return fmt.Sprintf("copied into processed/ but failed to remove incoming copy: %v", rerr), false
		}
		return "", true
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Sprintf("rename to processed/ failed: %v", err), false
	}
	prior, _, perr := readSegmentFileFromRoot(dest, name)
	if perr != nil {
		return fmt.Sprintf("processed/ copy exists but is unreadable (%v) — refusing to assume it matches; left in incoming/", perr), false
	}
	if !prior.ContentMatches(seg) {
		return "processed/ already holds DIFFERENT content for this identity — not overwriting; left in incoming/", false
	}
	if rerr := src.Remove(sourceName); rerr != nil {
		return fmt.Sprintf("processed/ already held an identical copy but removing the incoming copy failed: %v", rerr), false
	}
	return "", true
}
