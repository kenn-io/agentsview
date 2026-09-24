package segfile

import (
	"fmt"
	"os"
	"path/filepath"

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
