package sync

import (
	"sync/atomic"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/signals"
)

// secretScanBytes tracks incremental-only scans; shared full scans keep their
// counter in ingest.
var secretScanBytes atomic.Int64

// SecretScanBytes returns the total scanned byte count so far. Monotonic.
func SecretScanBytes() int64 {
	return ingest.SecretScanBytes() + secretScanBytes.Load()
}

// computeSignalsAndSecrets computes a session's signal update and its secret
// findings from the same message slice, returning the update with the
// secret-leak count and rules version already populated. Every sync write
// path uses this so no site can forget to stamp the rules version.
//
// The inline path scans definite rules only (secrets.ScanDefinite) and stamps
// the definite rules version. This keeps the FP-prone, CPU-heavy candidate
// regexes out of the sync hot path; an explicit secrets scan runs the full
// ruleset to add candidate findings. The two versions differ on purpose so
// backfill re-scans inline-only sessions (see secrets.DefiniteRulesVersion).
func computeSignalsAndSecrets(
	s db.Session, msgs []db.Message,
) (db.SessionSignalUpdate, []db.SecretFinding) {
	return ingest.ComputeSignalsAndSecrets(s, msgs)
}

// computeSignalsAndSecretsWithContentFailures is the staged streaming
// variant: the signal pass receives pre-computed per-call content-failure
// verdicts in place of the placeholder result content, while the secret
// scan is unchanged (staged event findings arrive through the sink's
// Findings instead).
func computeSignalsAndSecretsWithContentFailures(
	s db.Session, msgs []db.Message, failures map[string]bool,
) (db.SessionSignalUpdate, []db.SecretFinding) {
	return ingest.ComputeSignalsAndSecretsWithContentFailures(
		s, msgs, failures,
	)
}

// computeFullSignalsAndSecrets prepares all derived state for a full content
// transaction. The database binds the seed to the revision it commits, so no
// follow-up transaction or revision read is needed. Reuse the tool rows for
// aggregate signals and the incremental seed.
func computeFullSignalsAndSecrets(
	s db.Session, msgs []db.Message, failures map[string]bool,
) (db.SessionSignalUpdate, []db.SecretFinding, error) {
	rows := extractToolCallRows(msgs)
	patchToolCallRowsWithContentFailures(rows, msgs, failures)
	// These rows remain immutable through aggregate calculation and seeding.
	// Cache negative verdicts too: ordinary successful output otherwise gets
	// scanned again by each detector and the incremental-state seed.
	for i := range rows {
		if rows[i].EventStatus == "" {
			rows[i].ContentFailure = signals.IsFailure(rows[i])
			rows[i].ContentFailureKnown = true
		}
	}
	update := computeSignalsFromToolRows(s, msgs, rows)
	findings, leak := scanSecretsFromMessages(s, msgs, secrets.ScanDefinite)
	update.SecretLeakCount = leak
	update.SecretsRulesVersion = secrets.DefiniteRulesVersion()
	if isCodexFormatAgent(parser.AgentType(s.Agent)) {
		state, err := buildSignalStateFromRows(s.ID, msgs, rows, "")
		if err != nil {
			return db.SessionSignalUpdate{}, nil, err
		}
		update.FullState = &state
	}
	return update, findings, nil
}

// attachFullSignalState adds the SQLite-only incremental seed to an aggregate
// update already computed by shared ingestion. It does not rescan messages or
// recompute aggregate signals.
func (e *Engine) attachFullSignalState(
	s db.Session, msgs []db.Message, update db.SessionSignalUpdate,
	failures map[string]bool,
) (db.SessionSignalUpdate, error) {
	if !isCodexFormatAgent(parser.AgentType(s.Agent)) {
		return update, nil
	}
	rows := extractToolCallRows(msgs)
	patchToolCallRowsWithContentFailures(rows, msgs, failures)
	for i := range rows {
		if rows[i].EventStatus == "" {
			rows[i].ContentFailure = signals.IsFailure(rows[i])
			rows[i].ContentFailureKnown = true
		}
	}
	state, err := buildSignalStateFromRows(s.ID, msgs, rows, "")
	if err != nil {
		return db.SessionSignalUpdate{}, err
	}
	update.FullState = &state
	return update, nil
}

// scanSecretsFromMessages detects secrets across a session's message content,
// tool inputs, and canonical tool output (result events when present, else
// result_content) using scan: secrets.ScanDefinite for the fast inline path,
// or secrets.Scan for the full explicit scan. Returns the findings and the
// count of definite findings (the secret_leak_count signal). Pure: no DB
// access.
func scanSecretsFromMessages(
	s db.Session, msgs []db.Message, scan func(string) []secrets.Match,
) (findings []db.SecretFinding, definiteCount int) {
	return ingest.ScanSecretsFromMessages(s, msgs, scan)
}

// computeSignalsAndSecretsForStorage computes signals and findings from the
// messages as the archive will store them. A policy that drops tool payloads
// would otherwise produce content-derived values at write time that a later
// recompute from stored rows cannot reproduce.
func (e *Engine) computeSignalsAndSecretsForStorage(
	s db.Session, msgs []db.Message,
) (db.SessionSignalUpdate, []db.SecretFinding) {
	if e.db.ArchiveContent().OmitsToolContent() {
		s, msgs = e.db.ProjectSessionForStorage(s, msgs)
	}
	return computeSignalsAndSecrets(s, msgs)
}

// computeFullSignalsAndSecretsForStorage seeds full-parse state from the same
// content retained by the archive.
func (e *Engine) computeFullSignalsAndSecretsForStorage(
	s db.Session, msgs []db.Message, failures map[string]bool,
) (db.SessionSignalUpdate, []db.SecretFinding, error) {
	if e.db.ArchiveContent().OmitsToolContent() {
		s, msgs = e.db.ProjectSessionForStorage(s, msgs)
		failures = nil
	}
	return computeFullSignalsAndSecrets(s, msgs, failures)
}
