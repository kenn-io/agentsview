package sync

import (
	"context"
	"errors"
	"log"
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
	s db.Session, msgs []db.Message, failures map[string]db.StagedToolVerdict,
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
	s db.Session, msgs []db.Message, failures map[string]db.StagedToolVerdict,
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
	failures map[string]db.StagedToolVerdict,
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
	} else {
		msgs, _ = e.db.ProjectToolResultImagesWithPolicy(
			msgs, e.toolResultImages,
		)
	}
	return computeSignalsAndSecrets(s, msgs)
}

// computeFullSignalsAndSecretsForStorage seeds full-parse state from the same
// content retained by the archive.
func (e *Engine) computeFullSignalsAndSecretsForStorage(
	s db.Session, msgs []db.Message, failures map[string]db.StagedToolVerdict,
) (db.SessionSignalUpdate, []db.SecretFinding, error) {
	if e.db.ArchiveContent().OmitsToolContent() {
		s, msgs = e.db.ProjectSessionForStorage(s, msgs)
		failures = nil
	} else {
		msgs, _ = e.db.ProjectToolResultImagesWithPolicy(
			msgs, e.toolResultImages,
		)
	}
	return computeFullSignalsAndSecrets(s, msgs, failures)
}

const scanProgressInterval = 50

// SecretScanInput parameterises ScanSecrets.
type SecretScanInput struct {
	Backfill bool
	Project  string
	Agent    string
	DateFrom string
	DateTo   string
}

// SecretScanProgress is one progress tick.
type SecretScanProgress struct {
	Scanned int `json:"scanned"`
	Total   int `json:"total"`
}

// SecretScanSummary is the final result of a scan.
type SecretScanSummary struct {
	Scanned int `json:"scanned"`
	// WithSecrets counts sessions with ≥1 definite finding. It does NOT
	// include sessions whose findings are all candidate-tier; the
	// presence of those is implied by CandidateFindings > 0 when
	// DefiniteFindings is 0.
	WithSecrets       int `json:"with_secrets"`
	TotalFindings     int `json:"total_findings"`
	DefiniteFindings  int `json:"definite_findings"`
	CandidateFindings int `json:"candidate_findings"`
}

// ScanSecrets scans candidate sessions and persists their findings, invoking
// progress periodically. Resumable: each scanned session records the current
// rules version, so an interrupted backfill resumes by skipping sessions
// already at that version.
func (e *Engine) ScanSecrets(
	ctx context.Context, in SecretScanInput,
	progress func(SecretScanProgress),
) (SecretScanSummary, error) {
	if e.refuseWriteInForceParse("ScanSecrets") {
		return SecretScanSummary{}, errors.New(
			"ScanSecrets refused on report-only parse-diff engine",
		)
	}
	ver := secrets.RulesVersion()
	ids, err := e.db.SecretScanCandidates(ctx, db.SecretScanCandidateFilter{
		CurrentVersion: ver, OnlyStale: in.Backfill,
		Project: in.Project, Agent: in.Agent,
		DateFrom: in.DateFrom, DateTo: in.DateTo,
	})
	if err != nil {
		return SecretScanSummary{}, err
	}
	var sum SecretScanSummary
	total := len(ids)
	for i, id := range ids {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		nf, leak, ok := e.scanOneSession(ctx, id, ver)
		if ok {
			sum.Scanned++
			sum.TotalFindings += nf
			sum.DefiniteFindings += leak
			sum.CandidateFindings += nf - leak
			if leak > 0 {
				sum.WithSecrets++
			}
		}
		// A cancellation during the scan must end the run with an error,
		// not a partial success. This covers both a failed scan and a
		// successful final session whose context was canceled mid-scan,
		// since scanOneSession does CPU work and a non-context-aware
		// persist after its context-aware reads. That session is already
		// counted above: its findings are committed, and callers use the
		// summary to decide whether session eligibility changed.
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if !ok {
			continue
		}
		if progress != nil && scanShouldReport(i, total) {
			progress(SecretScanProgress{Scanned: sum.Scanned, Total: total})
		}
	}
	return sum, nil
}

// scanOneSession scans one session and persists its findings at ver. Returns
// the finding count, the definite-leak count, and ok=false when the session
// could not be loaded or persisted (skipped, not fatal to the whole run).
//
// Holds syncMu so the read/compute/write path is atomic against a concurrent
// sync replacing this session's messages: otherwise a sync could write fresh
// findings for new messages and then have this scan overwrite them with
// results from a stale snapshot while marking the session current. The lock is
// taken per session, not for the whole scan, so a long backfill does not stall
// the file watcher and periodic sync.
func (e *Engine) scanOneSession(
	ctx context.Context, id, ver string,
) (int, int, bool) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	sess, err := e.db.GetSessionFull(ctx, id)
	if err != nil || sess == nil {
		return 0, 0, false
	}
	msgs, err := e.db.GetAllMessages(ctx, id)
	if err != nil {
		return 0, 0, false
	}
	projectedSession, msgs := e.db.ProjectSessionForStorage(*sess, msgs)
	findings, leak := scanSecretsFromMessages(projectedSession, msgs, secrets.Scan)
	if err := e.db.ReplaceSessionSecretFindings(ctx, id, findings, leak, ver); err != nil {
		log.Printf("secrets scan: persist %s: %v", id, err)
		return 0, 0, false
	}
	return len(findings), leak, true
}

func scanShouldReport(i, total int) bool {
	return (i+1)%scanProgressInterval == 0 || i+1 == total
}
