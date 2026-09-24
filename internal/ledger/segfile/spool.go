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
	seg, err := ledger.ParseSegmentFile(b)
	if err != nil {
		return ledger.Segment{}, fmt.Errorf("serialization error: %w", err)
	}
	return seg, nil
}

// SpoolWriter publishes sealed segments into <spool>/incoming/ (writer.rs:20-95).
type SpoolWriter struct{ incoming string }

// NewSpoolWriter targets <spoolRoot>/incoming.
func NewSpoolWriter(spoolRoot string) SpoolWriter {
	return SpoolWriter{incoming: filepath.Join(spoolRoot, SpoolIncomingDir)}
}

// Write publishes seg with no-clobber semantics. The source is validated
// before any path is built: the filename derives from it, so this is the
// path-traversal guard (writer.rs:61-70). An identical existing copy is
// AlreadyIdentical; a different one is an error and both files stay intact.
func (w SpoolWriter) Write(seg ledger.Segment) (string, ledger.PublishOutcome, error) {
	if !ledger.ValidSourceName(seg.Source) {
		return "", ledger.Published, &InvalidSourceError{Name: seg.Source}
	}
	if err := os.MkdirAll(w.incoming, 0o755); err != nil {
		return "", ledger.Published, fmt.Errorf("I/O error: %w", err)
	}
	path := filepath.Join(w.incoming, seg.Filename())
	outcome, err := PublishNew(path, seg)
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
// an unreadable incoming/ directory or a failed processed/ creation aborts.
func SpoolIngest(ctx context.Context, spoolDir, zone string, dst Appender) (IngestReport, error) {
	report := IngestReport{Committed: []string{}, Skipped: []string{}, Failed: []IngestFailure{}}
	incoming := filepath.Join(spoolDir, SpoolIncomingDir)
	processed := filepath.Join(spoolDir, SpoolProcessedDir)
	// Rust's Path::exists is false on any stat error (ingester.rs:130-133).
	if !spoolPathExists(incoming) {
		return report, nil
	}
	if err := os.MkdirAll(processed, 0o755); err != nil {
		return report, fmt.Errorf("I/O error: %w", err)
	}
	names, err := spoolJSONNames(incoming, &report)
	if err != nil {
		return report, err
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		path := filepath.Join(incoming, name)
		outcome, seg, err := ingestOne(ctx, path, name, zone, dst)
		if err != nil {
			log.Printf("ledger spool: ingest of %s failed: %v", name, err)
			report.Failed = append(report.Failed, IngestFailure{File: name, Err: err.Error()})
			continue
		}
		if msg, ok := moveNoClobber(path, filepath.Join(processed, name), seg); !ok {
			what := "committed to store"
			if outcome == ingestDuplicate {
				what = "duplicate of store copy"
			}
			log.Printf("ledger spool: failed to move %s to processed/: %s", name, msg)
			report.Failed = append(report.Failed, IngestFailure{File: name, Err: what + " but " + msg})
			continue
		}
		// PR 13's fsyncDirBestEffort (publish.go, same package) logs and
		// continues, as ingester.rs:199-210 does after a move.
		fsyncDirBestEffort(processed)
		fsyncDirBestEffort(incoming)
		if outcome == ingestCommitted {
			report.Committed = append(report.Committed, name)
		} else {
			report.Skipped = append(report.Skipped, name)
		}
	}
	return report, nil
}

func spoolPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// spoolJSONNames lists *.json entries of dir sorted by name, skipping
// file-sync conflict copies with a warning (ingester.rs:139-173). Go's
// ReadDir reports an entry failure as one error after the readable entries;
// it becomes jilog's "(unreadable directory entry)" failure.
func spoolJSONNames(dir string, report *IngestReport) ([]string, error) {
	f, err := os.Open(dir)
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
	ctx context.Context, path, name, zone string, dst Appender,
) (ingestOutcome, ledger.Segment, error) {
	seg, err := readSegmentFile(path)
	if err != nil {
		return 0, seg, fmt.Errorf("ledger error: %w", err)
	}
	if !ledger.ValidSourceName(seg.Source) {
		return 0, seg, &InvalidSourceError{Name: seg.Source}
	}
	if expected := seg.Filename(); name != expected {
		return 0, seg, &IdentityMismatchError{Found: name, Expected: expected}
	}
	valid, err := seg.Verify()
	if err != nil {
		return 0, seg, fmt.Errorf("ledger error: serialization error: %w", err)
	}
	if !valid {
		return 0, seg, &IntegrityError{Src: seg.Source, Seq: seg.SourceSeq, Reason: "checksum mismatch"}
	}
	outcome, err := dst.AppendLedgerSegment(ctx, zone, seg, ledger.OriginSpool)
	switch {
	case err == nil && outcome == ledger.AlreadyIdentical:
		return ingestDuplicate, seg, nil
	case err == nil:
		return ingestCommitted, seg, nil
	case errors.Is(err, ledger.ErrIntegrity):
		return 0, seg, storeConflict(ctx, dst, zone, seg)
	default:
		return 0, seg, fmt.Errorf("ledger error: %w", err)
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

// moveNoClobber ports move_no_clobber (ingester.rs:248-279): hard-link then
// remove, never rename. It returns jilog's message and false on failure.
func moveNoClobber(src, dest string, seg ledger.Segment) (string, bool) {
	err := os.Link(src, dest)
	if err == nil {
		if rerr := os.Remove(src); rerr != nil {
			return fmt.Sprintf("linked into processed/ but failed to remove incoming copy: %v", rerr), false
		}
		return "", true
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Sprintf("rename to processed/ failed: %v", err), false
	}
	prior, perr := readSegmentFile(dest)
	if perr != nil {
		return fmt.Sprintf("processed/ copy exists but is unreadable (%v) — refusing to assume it matches; left in incoming/", perr), false
	}
	if !prior.ContentMatches(seg) {
		return "processed/ already holds DIFFERENT content for this identity — not overwriting; left in incoming/", false
	}
	if rerr := os.Remove(src); rerr != nil {
		return fmt.Sprintf("processed/ already held an identical copy but removing the incoming copy failed: %v", rerr), false
	}
	return "", true
}
