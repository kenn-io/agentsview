package extract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	recall "go.kenn.io/agentsview/internal/recall"
	"go.kenn.io/agentsview/internal/secrets"
)

const (
	defaultFailureBackoff = time.Hour
	defaultMaxAttempts    = 3
	// maxUnitDistillCalls caps the model calls one unit's overflow recovery
	// may make. A legitimate oversized action unit needs only a handful of
	// halvings to fit the context; a message far larger than any window —
	// user content is never packed, so its size is unbounded — would
	// otherwise fan out one call per split leaf and hold every leaf's
	// entries in memory. The cap is generous enough that no well-formed
	// unit reaches it.
	maxUnitDistillCalls = 256
)

// ManagerConfig assembles one extraction configuration. Its identity-bearing
// parts (Identity, Segmenter, Prompts, Client.Request) are fingerprinted at
// construction, so a Manager is bound to exactly one generation.
type ManagerConfig struct {
	DB        *db.DB
	Client    *Client
	Segmenter TurnsV1
	Prompts   map[PromptRole]string
	Identity  ModelIdentity
	// QuietPeriod excludes sessions that ended too recently from scans, so
	// a session that resumes shortly after ending is not extracted mid-way.
	QuietPeriod time.Duration
	// FailureBackoff delays retrying a failed session so one poisoned
	// transcript cannot monopolize passes. Defaults to one hour.
	FailureBackoff time.Duration
	// MaxAttempts bounds transient retries per model call. Defaults to 3.
	MaxAttempts int
}

// Manager drives extraction for one generation: it scans for eligible
// sessions, distills their units, records resumable progress, and activates
// the generation once everything eligible is done. At most one pass runs at
// a time; TryPass drops instead of queueing so schedulers cannot pile up.
type Manager struct {
	cfg         ManagerConfig
	fingerprint string
	splitFloor  int
	passMu      sync.Mutex
	// watermark bounds discovery for incremental scan passes: only sessions
	// written at or after it are examined for new work, so steady-state
	// passes scale with recent activity instead of the archive. It lags the
	// last completed pass by the quiet period (a session becomes eligible
	// quietPeriod after its final write, and that write is what discovery
	// sees), advances only when an unlimited scan pass completes, and is
	// ignored by full passes — they are the recovery path. Guarded by
	// passMu.
	watermark time.Time
	// fullWatermark bounds the done-revisit arm of full passes the same
	// way: only completed sessions written at or after it are rechecked, so
	// the periodic backstop scales with recent writes instead of every done
	// row. It advances to the pass start when an unlimited full pass
	// completes — no quiet-period lag, because a write landing during pass N
	// carries a local_modified_at at or after N's start and so stays visible
	// to pass N+1. Recovery from a lost watermark is a fresh manager, whose
	// zero value revisits unbounded. Guarded by passMu.
	fullWatermark time.Time
	// reconcileWatermark bounds eligibility retraction the same way: only
	// sessions written at or after it are examined for lost eligibility.
	// It advances on every completed unlimited scan pass — incremental
	// ones included, because with the backstop disabled no full pass ever
	// runs and retraction must not depend on one. Every ineligibility
	// write (trash, automation flag, findings) records a local write, so a
	// write during pass N stays visible to pass N+1. Guarded by passMu.
	reconcileWatermark time.Time
}

// PassOptions selects what one pass covers. SessionID targets a single
// session, bypassing the quiet period but never the privacy filters. Full
// revisits completed sessions so grown transcripts are topped up. Limit
// bounds how many sessions a scan processes (0 = all).
type PassOptions struct {
	SessionID string
	Full      bool
	Limit     int
}

// PassResult summarizes one pass.
type PassResult struct {
	// Sessions completed to done this pass.
	Sessions int
	// Failed sessions marked for later retry.
	Failed int
	// Units distilled this pass.
	Units int
	// Entries newly inserted (replayed units dedupe to zero).
	Entries int
	// Activated reports whether this pass activated the generation.
	Activated bool
}

// Status reports one generation's coverage for CLI display.
type Status struct {
	Fingerprint string                  `json:"fingerprint"`
	Generations []db.ExtractGeneration  `json:"generations"`
	Stats       db.ExtractProgressStats `json:"stats"`
	// EligibleBacklog counts sessions currently eligible but not covered:
	// never extracted, pending, partial, failed, or done with writes past
	// their coverage stamp.
	EligibleBacklog int `json:"eligible_backlog"`
}

// NewManager validates the configuration and computes its fingerprint.
func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("extraction manager requires a database")
	}
	if cfg.Client == nil {
		return nil, fmt.Errorf("extraction manager requires a client")
	}
	if err := cfg.Client.ValidateRequestShape(); err != nil {
		return nil, err
	}
	if cfg.Segmenter.MaxWindowChars <= 0 {
		return nil, fmt.Errorf(
			"extraction manager requires a positive max window, got %d",
			cfg.Segmenter.MaxWindowChars,
		)
	}
	if strings.TrimSpace(cfg.Identity.Model) == "" {
		return nil, fmt.Errorf("extraction manager requires a model identity")
	}
	for _, role := range cfg.Segmenter.PromptRoles() {
		if strings.TrimSpace(cfg.Prompts[role]) == "" {
			return nil, fmt.Errorf(
				"extraction manager is missing the %s prompt required by "+
					"segmenter %s", role, cfg.Segmenter.Name(),
			)
		}
	}
	if cfg.FailureBackoff <= 0 {
		cfg.FailureBackoff = defaultFailureBackoff
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	fingerprint, err := Fingerprint(
		cfg.Identity, cfg.Segmenter, cfg.Prompts, cfg.Client.Request,
	)
	if err != nil {
		return nil, err
	}
	return &Manager{
		cfg:         cfg,
		fingerprint: fingerprint,
		splitFloor:  SplitFloorChars(cfg.Segmenter.MaxWindowChars),
	}, nil
}

// Fingerprint returns the generation identity this manager builds.
func (m *Manager) Fingerprint() string { return m.fingerprint }

// EntryID derives the deterministic id for one extracted entry: the same
// generation, session, unit, and entry position always map to the same id,
// so replaying a unit after a crash or digest reset dedupes instead of
// duplicating.
func EntryID(fingerprint, sessionID string, unit, entry int) string {
	encoded, err := json.Marshal(
		[]any{"recall-extract", fingerprint, sessionID, unit, entry},
	)
	if err != nil {
		// Marshaling strings and ints cannot fail; guard anyway so a
		// future field change cannot silently produce colliding ids.
		panic(fmt.Sprintf("encoding extract entry id: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// RunPass runs one extraction pass, waiting for any in-flight pass first.
func (m *Manager) RunPass(
	ctx context.Context, opts PassOptions,
) (PassResult, error) {
	m.passMu.Lock()
	defer m.passMu.Unlock()
	return m.runPassLocked(ctx, opts)
}

// TryPass runs a pass only if none is in flight, reporting whether it ran.
// Schedulers use it so backstop ticks and event bursts drop instead of
// queueing behind a slow pass.
func (m *Manager) TryPass(
	ctx context.Context, opts PassOptions,
) (bool, PassResult, error) {
	if !m.passMu.TryLock() {
		return false, PassResult{}, nil
	}
	defer m.passMu.Unlock()
	result, err := m.runPassLocked(ctx, opts)
	return true, result, err
}

func (m *Manager) runPassLocked(
	ctx context.Context, opts PassOptions,
) (PassResult, error) {
	var result PassResult
	passStart := time.Now()
	if err := m.ensureGeneration(ctx); err != nil {
		return result, err
	}
	// Every scheduled pass reconciles eligibility loss before any model
	// work: sessions since trashed, flagged automated, or carrying secret
	// findings get their generated entries deleted (across all registered
	// generations — a retired generation keeps serving until the next
	// activation) and their progress rows removed, so an excluded
	// session's corpus stops serving and a lingering pending or partial
	// row cannot block activation forever. Running it first means privacy
	// retraction cannot be deferred by extraction failures — an
	// endpoint-scoped abort with a persistently broken endpoint would
	// otherwise defer it indefinitely — and it is not gated on full
	// passes: with the backstop disabled only incremental passes run, and
	// retraction must not be schedulable away.
	if opts.SessionID == "" {
		if _, _, err := m.cfg.DB.ReconcileIneligibleExtractSessions(
			ctx, m.reconcileWatermark,
		); err != nil {
			return result, err
		}
	}
	sessionIDs, err := m.passSessions(ctx, opts)
	if err != nil {
		return result, err
	}
	generation, ok, err := m.generation(ctx)
	if err != nil {
		return result, err
	}
	if !ok {
		return result, fmt.Errorf(
			"extract generation %s disappeared during pass", m.fingerprint,
		)
	}
	// While the generation is still building, its entries are staged as
	// archived so an unfinished corpus never serves; activation promotes
	// them atomically.
	staged := generation.State != db.ExtractGenerationActive
	for _, sessionID := range sessionIDs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		outcome, err := m.extractSession(
			ctx, sessionID, staged, opts.SessionID != "")
		result.Units += outcome.units
		result.Entries += outcome.entries
		if outcome.failed {
			result.Failed++
		}
		if err != nil {
			// Ineligibility at the first snapshot is drift: selection
			// only returned eligible sessions, so this one was excluded
			// concurrently and the reconciliation below (and the next
			// pass) own it. Aborting would drop the pass's remaining
			// candidates. An explicit run keeps the error — the caller
			// named the session and must hear why it was refused.
			var ineligible *ineligibleSessionError
			if opts.SessionID == "" && errors.As(err, &ineligible) {
				continue
			}
			return result, err
		}
		if outcome.done {
			result.Sessions++
		}
	}
	// A second reconciliation after the loop catches eligibility lost
	// while units were at the model — the mid-extraction discard reopens
	// the row, and this removes it so a restored session rediscovers
	// through the no-progress discovery arm. Reachable only when the loop
	// completed; an aborted pass is covered by the next pass's pre-loop
	// reconciliation, which runs even against a persistently broken
	// endpoint.
	if opts.SessionID == "" {
		if _, _, err := m.cfg.DB.ReconcileIneligibleExtractSessions(
			ctx, m.reconcileWatermark,
		); err != nil {
			return result, err
		}
	}
	activated, err := m.maybeActivate(ctx)
	if err != nil {
		return result, err
	}
	result.Activated = activated
	// Only a scan pass that covered everything it found may advance the
	// watermark: an explicit or limited pass leaves eligible sessions
	// behind that a bounded discovery would then never see. Sessions this
	// pass skipped on a snapshot mismatch were concurrently written, so
	// their local_modified_at already lies past the new watermark. The
	// advance is a ratchet — a long quiet period must not drag an already
	// higher watermark backward.
	if opts.SessionID == "" && opts.Limit == 0 {
		if w := passStart.Add(-m.cfg.QuietPeriod); w.After(m.watermark) {
			m.watermark = w
		}
		if opts.Full && passStart.After(m.fullWatermark) {
			m.fullWatermark = passStart
		}
		if passStart.After(m.reconcileWatermark) {
			m.reconcileWatermark = passStart
		}
	}
	return result, nil
}

func (m *Manager) ensureGeneration(ctx context.Context) error {
	params, err := json.Marshal(m.cfg.Segmenter.Params())
	if err != nil {
		return fmt.Errorf("encoding segmenter params: %w", err)
	}
	_, err = m.cfg.DB.EnsureExtractGeneration(ctx, db.ExtractGeneration{
		Fingerprint: m.fingerprint,
		Model:       m.cfg.Identity.Model,
		Segmenter:   m.cfg.Segmenter.Name(),
		ParamsJSON:  string(params),
	})
	return err
}

func (m *Manager) passSessions(
	ctx context.Context, opts PassOptions,
) ([]string, error) {
	if opts.SessionID != "" {
		session, err := m.cfg.DB.GetSession(ctx, opts.SessionID)
		if err != nil {
			return nil, err
		}
		if session == nil {
			return nil, fmt.Errorf("session %s not found", opts.SessionID)
		}
		if err := extractableSession(opts.SessionID, session); err != nil {
			return nil, err
		}
		if err := m.refuseSecretFindings(ctx, opts.SessionID); err != nil {
			return nil, err
		}
		return []string{opts.SessionID}, nil
	}
	now := time.Now()
	// Every scheduled pass — full ones included — bounds discovery by the
	// watermark, and a full pass additionally bounds its done revisits by
	// the full watermark, so background work scales with recent writes
	// rather than the archive. Unbounded reconciliation is the
	// fresh-manager path: a daemon restart or a manual CLI run starts with
	// zero watermarks.
	q := db.ExtractCandidateQuery{
		Fingerprint:       m.fingerprint,
		QuietCutoff:       now.Add(-m.cfg.QuietPeriod),
		FailedRetryCutoff: now.Add(-m.cfg.FailureBackoff),
		ScanVersions:      []string{secrets.RulesVersion()},
		IncludeDone:       opts.Full,
		ChangedSince:      m.watermark,
		Limit:             opts.Limit,
	}
	if opts.Full {
		q.DoneChangedSince = m.fullWatermark
	}
	return m.cfg.DB.ExtractCandidates(ctx, q)
}

// ineligibleSessionError marks a failure of the pre-extraction eligibility
// phase: the session row is missing, a privacy predicate refuses it, or it
// has recorded secret findings. Candidate selection only returns eligible
// sessions, so reaching this from a scheduled pass means the session was
// excluded concurrently — drift the pass skips, matching the later bracket
// checks — while an explicit run surfaces the message unchanged.
type ineligibleSessionError struct{ err error }

func (e *ineligibleSessionError) Error() string { return e.err.Error() }
func (e *ineligibleSessionError) Unwrap() error { return e.err }

// refuseSecretFindings excludes sessions with recorded secret findings of any
// confidence. The leak count only counts definite findings; a candidate
// finding (a JWT, a high-entropy blob) is exactly the material that must not
// reach the model either.
func (m *Manager) refuseSecretFindings(
	ctx context.Context, sessionID string,
) error {
	findings, err := m.cfg.DB.SessionSecretFindings(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		return &ineligibleSessionError{err: fmt.Errorf(
			"session %s has %d recorded secret findings and is excluded "+
				"from extraction", sessionID, len(findings),
		)}
	}
	return nil
}

// settledPastQuietPeriod refuses a session whose ended_at falls inside
// the quiet period as of now. An unparseable ended_at fails closed: a
// value the archive cannot interpret cannot prove the session settled.
func (m *Manager) settledPastQuietPeriod(
	id string, s *db.Session,
) error {
	endedAt, err := time.Parse(time.RFC3339Nano, *s.EndedAt)
	if err != nil {
		return fmt.Errorf(
			"session %s has an unparseable ended_at %q: %w",
			id, *s.EndedAt, err,
		)
	}
	if cutoff := time.Now().Add(-m.cfg.QuietPeriod); endedAt.After(cutoff) {
		return fmt.Errorf(
			"session %s ended %s ago, inside the %s quiet period",
			id, time.Since(endedAt).Round(time.Second), m.cfg.QuietPeriod,
		)
	}
	return nil
}

// extractableSession enforces the extraction privacy boundary for explicit
// single-session runs. The scan path enforces the same predicates in SQL;
// keeping both in lockstep means no path can feed an excluded session to
// the model. Callers must have checked s for nil already.
func extractableSession(id string, s *db.Session) error {
	switch {
	case s.DeletedAt != nil:
		return fmt.Errorf("session %s is trashed", id)
	case s.IsAutomated:
		return fmt.Errorf(
			"session %s is automated and excluded from extraction", id,
		)
	case s.SecretLeakCount > 0:
		return fmt.Errorf(
			"session %s has %d secret findings and is excluded from "+
				"extraction", id, s.SecretLeakCount,
		)
	case !currentScanVersion(s.SecretsRulesVersion):
		return fmt.Errorf(
			"session %s has no secret scan under the current rules; run "+
				"'agentsview secrets scan --backfill' first", id,
		)
	case s.MessageCount == 0:
		return fmt.Errorf("session %s has no messages", id)
	case s.EndedAt == nil || *s.EndedAt == "":
		return fmt.Errorf("session %s has not ended", id)
	}
	return nil
}

// currentScanVersion reports whether version is the current *full* secret-scan
// rules version. The definite-only inline sync scan does not qualify: it never
// looks for candidate-confidence secrets, so a session it cleared may still
// carry them. An unscanned session ("") never qualifies either: the privacy
// boundary fails closed.
func currentScanVersion(version string) bool {
	return version == secrets.RulesVersion()
}

// sessionSnapshotChanged reports whether two reads of a session row describe
// different transcript or scan states. The manager brackets its message read
// with session reads and discards the work when they differ, so eligibility
// is always judged against the transcript actually sent to the model — sync
// writes messages, scan stamps, and counts in one transaction, so a stable
// bracket means a consistent view.
func sessionSnapshotChanged(before, after *db.Session) bool {
	return before.MessageCount != after.MessageCount ||
		!stringPtrEqual(before.TranscriptRevision, after.TranscriptRevision) ||
		before.SecretsRulesVersion != after.SecretsRulesVersion ||
		before.SecretLeakCount != after.SecretLeakCount ||
		!stringPtrEqual(before.EndedAt, after.EndedAt) ||
		!stringPtrEqual(before.LocalModifiedAt, after.LocalModifiedAt)
}

// extractionBracketStable reports whether the second snapshot read matches
// the first and still passes eligibility. The comparison alone is not
// enough: trash and automation flags can flip without touching any field
// sessionSnapshotChanged watches, and content extracted under that stale
// view would outlive the session's exclusion.
func extractionBracketStable(id string, before, after *db.Session) bool {
	return after != nil && !sessionSnapshotChanged(before, after) &&
		extractableSession(id, after) == nil
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

type sessionOutcome struct {
	units   int
	entries int
	done    bool
	failed  bool
}

// sessionSnapshot reads the session row with its full column set. The
// snapshot bracket around the message load depends on local_modified_at,
// which the standard GetSession column list does not carry — loading it nil
// on both sides would blind the comparison to metadata-only writes such as
// a findings replace under an unchanged rules version.
func (m *Manager) sessionSnapshot(
	ctx context.Context, sessionID string,
) (*db.Session, error) {
	return m.cfg.DB.GetSessionFull(ctx, sessionID)
}

func (m *Manager) extractSession(
	ctx context.Context, sessionID string, staged, explicit bool,
) (sessionOutcome, error) {
	var outcome sessionOutcome
	// The transcript-read cutoff is captured before anything is read: a
	// write landing anywhere in this function still compares as after it,
	// so the done-revisit gate re-opens the session.
	readCutoff := time.Now()
	session, err := m.sessionSnapshot(ctx, sessionID)
	if err != nil {
		return outcome, err
	}
	if session == nil {
		return outcome, &ineligibleSessionError{
			err: fmt.Errorf("session %s not found", sessionID)}
	}
	if err := extractableSession(sessionID, session); err != nil {
		return outcome, &ineligibleSessionError{err: err}
	}
	// The quiet period is rechecked here, not only at selection: the
	// backlog is materialized at pass start, so a session that ends again
	// while queued would otherwise be extracted mid-settling. Parsed-time
	// comparison, unlike the selection query's indexed string range, is
	// exact across the RFC3339Nano precision variants ended_at carries.
	// Explicit runs bypass the quiet period by contract; ended_at drift
	// after this check trips the snapshot bracket and the commit guard.
	if !explicit {
		if err := m.settledPastQuietPeriod(sessionID, session); err != nil {
			return outcome, &ineligibleSessionError{err: err}
		}
	}
	if err := m.refuseSecretFindings(ctx, sessionID); err != nil {
		return outcome, err
	}
	rows, err := m.cfg.DB.GetAllMessages(ctx, sessionID)
	if err != nil {
		return outcome, err
	}
	// Re-read the session and recheck: if sync wrote to it between the
	// eligibility check and the message read, the transcript just loaded may
	// contain content the check never saw, and a session trashed or flagged
	// automated in that window is no longer eligible at all. Skip silently —
	// a concurrent write bumped local_modified_at so the next pass retries
	// against a settled view, and a newly ineligible session must simply
	// not be extracted.
	recheck, err := m.sessionSnapshot(ctx, sessionID)
	if err != nil {
		return outcome, err
	}
	if !extractionBracketStable(sessionID, session, recheck) {
		return outcome, nil
	}
	// The sync loop writes the session row before the transcript, so
	// mid-write the row can claim more (or fewer) messages than are
	// stored. Eligibility was judged from the row; a transcript that does
	// not match it contains content the check never approved, so it must
	// not reach the model. The snapshot bracket was stable, so this is not
	// a caught-mid-write race either: recorded below as a retryable
	// failure, because a silent skip would let the discovery watermarks
	// advance past the session's writes and exclude it forever.
	countMismatch := len(rows) != session.MessageCount
	// The stamp the eligibility check trusted is a claim recorded by a past
	// write, and archives written by older binaries can carry a
	// current-looking clean stamp over content the scan never saw (an
	// incremental append whose deferred rescan crashed before it landed).
	// Re-scanning the content this function is about to send makes the
	// boundary independent of that history.
	secretMatches := transcriptSecretMatches(rows)
	messages := make([]Message, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, Message{
			Ordinal:  row.Ordinal,
			Role:     row.Role,
			Content:  row.Content,
			IsSystem: row.IsSystem,
		})
	}
	// Per-message scanning misses a secret whose structure spans messages:
	// a PEM block split across adjacent messages (joined inside one unit) or
	// across separate units the endpoint receives and can correlate. Scan
	// the aggregate of exactly the model-visible content — the same rows the
	// segmenter keeps (system and unsupported roles dropped, each trimmed) —
	// so an intervening system message or boundary whitespace cannot break a
	// token in the scan that the endpoint still receives contiguously. Raw
	// contents, not formatted unit texts, so interposed formatting cannot
	// push a straddling key under the scanner's payload-purity gate.
	secretMatches += aggregateModelSecretMatches(messages)
	units := m.cfg.Segmenter.Units(messages)
	digest := unitsDigest(units)
	// A digest change means previously extracted units may have different
	// content now (an assistant run that grew re-packs into an existing
	// unit). Entry ids are positional, so stale entries would both linger
	// and block their replacements; the upsert below deletes them in the
	// same transaction that resets the row.
	previous, found, err := m.cfg.DB.ExtractProgress(
		ctx, sessionID, m.fingerprint,
	)
	if err != nil {
		return outcome, err
	}
	if secretMatches > 0 || countMismatch {
		// The failure transition must own any stamp movement. Writing the
		// opening upsert first would advance a same-digest done row's
		// coverage stamp in its own committed transaction; a crash or
		// cancellation before the reopen below then leaves invalid
		// coverage — possibly from a secret-bearing transcript — stamped
		// current, unselectable by the done-revisit predicate and
		// eligible for activation. A row already carrying this digest
		// needs no upsert at all; a missing or digest-changed row is
		// created or reset to pending, which a crash leaves retryable.
		cursor := 0
		if found && previous.ContentDigest == digest {
			cursor = previous.UnitCursor
		} else {
			created, err := m.cfg.DB.UpsertExtractProgress(
				ctx, db.ExtractProgressUpsert{
					SessionID:     sessionID,
					Fingerprint:   m.fingerprint,
					ContentDigest: digest,
					UnitsTotal:    len(units),
					StampedAt:     readCutoff,
				},
			)
			if err != nil {
				return outcome, err
			}
			cursor = created.UnitCursor
		}
		if secretMatches > 0 {
			// Fail closed: entries distilled from this transcript are
			// suspect, so they are dropped with the row reopened behind
			// the failure backoff. Recording findings stays the scanner's
			// job — a rescan either lands the findings (excluding the
			// session from discovery outright) or re-stamps a genuinely
			// clean transcript, and either way the retry settles.
			if derr := m.discardSessionOutput(
				ctx, sessionID, digest, cursor,
				fmt.Sprintf(
					"transcript matches %d secret rule pattern(s) despite "+
						"a current scan stamp; run 'agentsview secrets "+
						"scan --backfill'", secretMatches,
				),
			); derr != nil {
				return outcome, derr
			}
			outcome.failed = true
			return outcome, nil
		}
		// A same-digest upsert preserves done and settles the stamp,
		// which would claim the inconsistent state as covered forever.
		// Reopen demotes such a row back into the retry queue.
		if markErr := m.cfg.DB.MarkExtractProgressFailed(ctx, db.ExtractFailure{
			SessionID:      sessionID,
			Fingerprint:    m.fingerprint,
			ExpectedDigest: digest,
			ExpectedCursor: cursor,
			LastError: fmt.Sprintf(
				"transcript has %d messages but the session row claims %d",
				len(rows), session.MessageCount,
			),
			Reopen: true,
		}); markErr != nil && !errors.Is(markErr, db.ErrStaleExtractProgress) {
			return outcome, markErr
		}
		outcome.failed = true
		return outcome, nil
	}
	var progress db.ExtractProgress
	// Zero-unit sessions take the upsert path even on a same-digest
	// revisit: they hold no entries for the refresh to maintain, and a row
	// reopened as failed (say, by a corrected count mismatch) can only be
	// repaired here — the loop below never advances a cursor over an empty
	// unit list, and the upsert lands zero-unit rows in done by
	// construction.
	if found && previous.ContentDigest == digest && len(units) > 0 {
		// A same-digest revisit skips the model, but its entries still
		// need work: context fields copied at insert time go stale on
		// metadata-only updates, and evidence digests cover rows the units
		// digest ignores, so the reconciler can revoke provenance while
		// the extraction output is unchanged. Context sync, evidence
		// rebind, and the coverage stamp land in one guarded transaction —
		// a transcript write mid-refresh must not see stale entries
		// rebound to it, marked verified, and stamped covered.
		progress, err = m.cfg.DB.RefreshExtractedSessionCoverage(
			ctx, db.ExtractCoverageRefresh{
				Fingerprint:  m.fingerprint,
				Digest:       digest,
				StampedAt:    readCutoff,
				ScanVersions: []string{secrets.RulesVersion()},
				Session:      session,
			},
		)
		switch {
		case errors.Is(err, db.ErrExtractSessionDrifted):
			eligible, _, rerr := m.recheckExtraction(ctx, sessionID, session)
			if rerr != nil {
				return outcome, rerr
			}
			if !eligible {
				if derr := m.discardIneligibleSession(
					ctx, sessionID, digest, previous.UnitCursor,
				); derr != nil {
					return outcome, derr
				}
				outcome.failed = true
			}
			// Still eligible: a concurrent write bumped the snapshot, so
			// the next pass retries against a settled view.
			return outcome, nil
		case errors.Is(err, db.ErrStaleExtractProgress):
			// Another writer reset or took over the row; its view wins.
			return outcome, nil
		case err != nil:
			return outcome, err
		}
	} else {
		progress, err = m.cfg.DB.UpsertExtractProgress(
			ctx, db.ExtractProgressUpsert{
				SessionID:     sessionID,
				Fingerprint:   m.fingerprint,
				ContentDigest: digest,
				UnitsTotal:    len(units),
				StampedAt:     readCutoff,
			},
		)
		if err != nil {
			return outcome, err
		}
	}
	if progress.State == db.ExtractProgressDone {
		return outcome, nil
	}
	for i := progress.UnitCursor; i < len(units); i++ {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		// Eligibility can be lost while earlier units distill — a trash, an
		// automation flag, a secret scan landing findings — so it is
		// re-validated before every model call and again before persisting
		// the call's output. Losing it fails closed: this pass's generated
		// entries are discarded and coverage restarts from scratch if the
		// session ever becomes eligible again.
		eligible, unchanged, err := m.recheckExtraction(ctx, sessionID, session)
		if err != nil {
			return outcome, err
		}
		if !eligible {
			if err := m.discardIneligibleSession(
				ctx, sessionID, digest, i,
			); err != nil {
				return outcome, err
			}
			outcome.failed = true
			return outcome, nil
		}
		if !unchanged {
			return outcome, nil
		}
		unit := units[i]
		entries, err := m.distillSplit(ctx, m.cfg.Prompts[unit.Role], unit.Text)
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown, not a poisoned session: leave the row
				// resumable instead of burning the failure backoff.
				return outcome, err
			}
			if endpointScopedRejection(err) {
				// The endpoint, not this transcript, refused: abort the
				// pass instead of marching every remaining session into
				// the same rejection, and leave the row resumable — it
				// picks up where it stopped once the configuration works.
				return outcome, err
			}
			if markErr := m.cfg.DB.MarkExtractProgressFailed(ctx, db.ExtractFailure{
				SessionID:      sessionID,
				Fingerprint:    m.fingerprint,
				ExpectedDigest: digest,
				ExpectedCursor: i,
				LastError:      boundedLastError(err),
			}); markErr != nil && !errors.Is(markErr, db.ErrStaleExtractProgress) {
				return outcome, markErr
			}
			outcome.failed = true
			var transient *transientError
			if errors.As(err, &transient) {
				// An exhausted retry ladder means the endpoint is down or
				// saturated, not that this unit is poisoned: every
				// remaining session would burn its own full ladder
				// against the same outage, stalling the pass for hours on
				// a large backlog. Abort — the failure mark above keeps
				// this session behind its backoff, so one pathological
				// unit cannot re-abort every pass, and the rest of the
				// backlog stays untouched and resumable.
				return outcome, err
			}
			return outcome, nil
		}
		// The unit's output is committed under an in-transaction guard: the
		// session snapshot, eligibility, and absence of findings are
		// re-verified atomically with the insert and cursor advance, and
		// the evidence is bound to the host transcript (content digest,
		// stable endpoint UUIDs) so the evidence reconciler can re-verify
		// provenance instead of revoking it. The pre-call recheck above
		// only saves a wasted model call; this is the enforcement point.
		inserted, err := m.cfg.DB.CommitExtractedUnit(ctx, db.ExtractUnitCommit{
			SessionID:          sessionID,
			Fingerprint:        m.fingerprint,
			Digest:             digest,
			Cursor:             i,
			ScanVersions:       []string{secrets.RulesVersion()},
			MessageCount:       session.MessageCount,
			TranscriptRevision: session.TranscriptRevision,
			LocalModifiedAt:    session.LocalModifiedAt,
			EndedAt:            session.EndedAt,
			Entries:            m.extractedEntries(session, unit, i, entries, staged),
		})
		switch {
		case errors.Is(err, db.ErrExtractSessionDrifted):
			eligible, unchanged, rerr := m.recheckExtraction(
				ctx, sessionID, session,
			)
			if rerr != nil {
				return outcome, rerr
			}
			if !eligible {
				if derr := m.discardIneligibleSession(
					ctx, sessionID, digest, i,
				); derr != nil {
					return outcome, derr
				}
				outcome.failed = true
				return outcome, nil
			}
			if unchanged {
				// The session re-reads exactly as bracketed, so the
				// refusal is deterministic (e.g. an evidence range the
				// transcript cannot verify), not a concurrent write:
				// retrying next pass would spend another model call to
				// fail the same way. Record it so the backoff applies.
				if markErr := m.cfg.DB.MarkExtractProgressFailed(
					ctx, db.ExtractFailure{
						SessionID:      sessionID,
						Fingerprint:    m.fingerprint,
						ExpectedDigest: digest,
						ExpectedCursor: i,
						LastError:      boundedLastError(err),
					},
				); markErr != nil &&
					!errors.Is(markErr, db.ErrStaleExtractProgress) {
					return outcome, markErr
				}
				outcome.failed = true
				return outcome, nil
			}
			// A concurrent write bumped the snapshot: the next pass
			// retries against a settled view.
			return outcome, nil
		case errors.Is(err, db.ErrStaleExtractProgress):
			// Another writer reset or took over this session; its view
			// wins and this pass simply stops contributing to it.
			return outcome, nil
		case err != nil:
			return outcome, err
		}
		outcome.entries += inserted
		outcome.units++
	}
	outcome.done = true
	return outcome, nil
}

// recheckExtraction re-reads the session mid-extraction and reports whether
// it is still eligible (present, extractable, and free of secret findings of
// any confidence) and whether its snapshot still matches the bracket's first
// read. The findings query is separate because a scan under an unchanged
// rules version can land candidate findings without touching any snapshot
// field.
func (m *Manager) recheckExtraction(
	ctx context.Context, sessionID string, before *db.Session,
) (eligible, unchanged bool, err error) {
	recheck, err := m.sessionSnapshot(ctx, sessionID)
	if err != nil {
		return false, false, err
	}
	if recheck == nil || extractableSession(sessionID, recheck) != nil {
		return false, false, nil
	}
	findings, err := m.cfg.DB.SessionSecretFindings(ctx, sessionID)
	if err != nil {
		return false, false, err
	}
	if len(findings) > 0 {
		return false, false, nil
	}
	return true, !sessionSnapshotChanged(before, recheck), nil
}

// discardIneligibleSession fails a session closed mid-extraction: its
// generated entries are deleted and its progress row is reopened at cursor
// zero, atomically — a delete that landed without the cursor reset would
// let a later resume skip units whose entries no longer exist. A stale
// guard means another writer took over; its view wins and nothing is
// discarded.
func (m *Manager) discardIneligibleSession(
	ctx context.Context, sessionID, digest string, cursor int,
) error {
	return m.discardSessionOutput(
		ctx, sessionID, digest, cursor,
		"session became ineligible during extraction",
	)
}

// maxStoredErrorBytes caps externally derived error text persisted into
// failure rows: a row is written per session, so a hostile endpoint must
// not grow the archive by megabytes per pass through error strings.
const maxStoredErrorBytes = 2048

func boundedLastError(err error) string {
	msg := err.Error()
	const marker = " …(truncated)"
	if len(msg) <= maxStoredErrorBytes {
		return msg
	}
	cut := maxStoredErrorBytes - len(marker)
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + marker
}

func (m *Manager) discardSessionOutput(
	ctx context.Context, sessionID, digest string, cursor int,
	lastError string,
) error {
	err := m.cfg.DB.DiscardExtractedSessionOutput(ctx, db.ExtractFailure{
		SessionID:      sessionID,
		Fingerprint:    m.fingerprint,
		ExpectedDigest: digest,
		ExpectedCursor: cursor,
		LastError:      lastError,
		Reopen:         true,
	})
	if err != nil && !errors.Is(err, db.ErrStaleExtractProgress) {
		return err
	}
	return nil
}

// transcriptSecretMatches counts matches from the full secret ruleset over
// the message content extraction sends to the model. Only Content reaches
// the segmenter, so scanning it covers exactly the outbound material; tool
// inputs and results stay the stored scan's concern.
// aggregateModelSecretMatches scans the concatenation of exactly the
// model-visible contents (see VisibleContents), in transcript order. See the
// call site for why per-message scanning alone is not enough, and why the
// raw contents — not the formatted unit texts — are the right aggregate to
// scan. Two joins are scanned: newline-preserving keeps the structure of
// multi-line secrets (a PEM block), and separator-free reconstructs a
// single-token credential split mid-token across messages, which the newline
// would otherwise break so a regex needing contiguous characters could not
// match. Either matching means the material is present.
func aggregateModelSecretMatches(messages []Message) int {
	texts := VisibleContents(messages)
	matches := len(secrets.Scan(strings.Join(texts, "\n")))
	matches += len(secrets.Scan(strings.Join(texts, "")))
	return matches
}

func transcriptSecretMatches(rows []db.Message) int {
	matches := 0
	for _, row := range rows {
		matches += len(secrets.Scan(row.Content))
	}
	return matches
}

// distillSplit distills one text, halving it recursively when the model
// rejects it as too large (context overflow) or cannot emit a complete
// response for it (persistent truncation). The split floor stops recursion:
// below it the text is small enough that splitting further would only
// destroy context, so the error surfaces instead.
func (m *Manager) distillSplit(
	ctx context.Context, prompt, text string,
) ([]Entry, error) {
	calls := 0
	return m.distillSplitBounded(ctx, prompt, text, &calls)
}

// distillSplitBounded is distillSplit with a shared call counter so one
// unit's recovery cannot fan out without limit. calls counts every model
// request the recovery makes across the whole recursion; once it reaches the
// budget the unit fails closed with ErrSplitBudgetExceeded rather than
// splitting an unbounded message into ever more leaves.
func (m *Manager) distillSplitBounded(
	ctx context.Context, prompt, text string, calls *int,
) ([]Entry, error) {
	if *calls >= maxUnitDistillCalls {
		return nil, fmt.Errorf(
			"unit recovery reached %d model calls: %w",
			maxUnitDistillCalls, ErrSplitBudgetExceeded)
	}
	*calls++
	entries, _, err := m.cfg.Client.DistillWithRecovery(
		ctx, prompt, text, m.cfg.MaxAttempts,
	)
	if err == nil {
		return entries, nil
	}
	if !errors.Is(err, ErrContextOverflow) &&
		!errors.Is(err, ErrPersistentTruncation) {
		return nil, err
	}
	runes := []rune(text)
	if len(runes) <= m.splitFloor {
		return nil, err
	}
	mid := len(runes) / 2
	left, err := m.distillSplitBounded(ctx, prompt, string(runes[:mid]), calls)
	if err != nil {
		return nil, err
	}
	right, err := m.distillSplitBounded(ctx, prompt, string(runes[mid:]), calls)
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

func (m *Manager) extractedEntries(
	session *db.Session, unit Unit, unitIndex int, entries []Entry, staged bool,
) []db.RecallEntry {
	// Staged entries carry the archived status until activation promotes
	// them: an unfinished generation must not serve a partial corpus.
	status := recall.StatusAccepted
	if staged {
		status = recall.StatusArchived
	}
	rows := make([]db.RecallEntry, 0, len(entries))
	for i, entry := range entries {
		body := entry.Body
		if len(entry.Entities) > 0 {
			body += "\nEntities: " + strings.Join(entry.Entities, "; ")
		}
		rows = append(rows, db.RecallEntry{
			ID:              EntryID(m.fingerprint, session.ID, unitIndex, i),
			Type:            entry.Type,
			Scope:           recall.ScopeProject,
			Status:          status,
			ReviewState:     recall.ReviewStateUnreviewedAuto,
			Title:           entry.Title,
			Body:            body,
			Project:         session.Project,
			CWD:             session.Cwd,
			GitBranch:       session.GitBranch,
			Agent:           session.Agent,
			SourceSessionID: session.ID,
			SourceRunID:     m.fingerprint,
			ExtractorMethod: m.cfg.Segmenter.Name(),
			Model:           m.cfg.Identity.Model,
			ProvenanceOK:    true,
			Evidence: []db.RecallEvidence{{
				SessionID:           session.ID,
				MessageStartOrdinal: unit.OrdinalStart,
				MessageEndOrdinal:   unit.OrdinalEnd,
			}},
		})
	}
	return rows
}

// maybeActivate promotes the generation from building to active once it has
// produced a corpus and nothing eligible remains unprocessed. Failed
// sessions do not block activation: they retry on later passes and top the
// corpus up after the fact.
func (m *Manager) maybeActivate(ctx context.Context) (bool, error) {
	generation, ok, err := m.generation(ctx)
	if err != nil {
		return false, err
	}
	if !ok || generation.State != db.ExtractGenerationBuilding {
		return false, nil
	}
	stats, err := m.cfg.DB.ExtractProgressStats(ctx, m.fingerprint)
	if err != nil {
		return false, err
	}
	// Raw pending/partial counts are not consulted: a row whose session
	// turned transiently ineligible (reopened, scan stamp lost) can never
	// finish, and refusing on it would stall activation until the session
	// happens to settle. The backlog probe below sees pending and partial
	// rows of eligible sessions, and the activation transaction re-checks
	// coverage and clears unfinishable rows itself.
	if stats.Done == 0 {
		return false, nil
	}
	if stats.Entries == 0 {
		// Sessions completed but nothing was extracted: activating would
		// replace whatever is currently active with an empty corpus.
		return false, nil
	}
	// IncludeDone: a completed session whose transcript changed since its
	// unit snapshot is uncovered work — activating over it would promote a
	// corpus that is already stale. Failed sessions stay excluded (zero
	// FailedRetryCutoff): they retry later and top the corpus up.
	backlog, err := m.cfg.DB.ExtractCandidates(ctx, db.ExtractCandidateQuery{
		Fingerprint:  m.fingerprint,
		QuietCutoff:  time.Now().Add(-m.cfg.QuietPeriod),
		ScanVersions: []string{secrets.RulesVersion()},
		IncludeDone:  true,
		Limit:        1,
	})
	if err != nil {
		return false, err
	}
	if len(backlog) > 0 {
		return false, nil
	}
	err = m.cfg.DB.ActivateExtractGeneration(
		ctx, m.fingerprint, []string{secrets.RulesVersion()},
		time.Now().Add(-m.cfg.QuietPeriod),
	)
	if errors.Is(err, db.ErrExtractActivationBlocked) {
		// Coverage moved between the checks above and the activation
		// transaction's own guards; the next pass re-extracts and retries.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Activate promotes this manager's generation explicitly, refusing when it
// has produced nothing: an empty active generation would serve an empty
// corpus while looking healthy.
func (m *Manager) Activate(ctx context.Context) error {
	stats, err := m.cfg.DB.ExtractProgressStats(ctx, m.fingerprint)
	if err != nil {
		return err
	}
	if stats.Done == 0 {
		return fmt.Errorf(
			"refusing to activate generation %s: no completed sessions",
			m.fingerprint,
		)
	}
	if stats.Entries == 0 {
		return fmt.Errorf(
			"refusing to activate generation %s: no extracted entries — "+
				"activating would serve an empty corpus", m.fingerprint,
		)
	}
	return m.cfg.DB.ActivateExtractGeneration(
		ctx, m.fingerprint, []string{secrets.RulesVersion()},
		time.Now().Add(-m.cfg.QuietPeriod),
	)
}

// Status reports this generation's coverage and the current backlog.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	status := Status{Fingerprint: m.fingerprint}
	generations, err := m.cfg.DB.ExtractGenerations(ctx)
	if err != nil {
		return status, err
	}
	status.Generations = generations
	stats, err := m.cfg.DB.ExtractProgressStats(ctx, m.fingerprint)
	if err != nil {
		return status, err
	}
	status.Stats = stats
	now := time.Now()
	backlog, err := m.cfg.DB.ExtractCandidates(ctx, db.ExtractCandidateQuery{
		Fingerprint:       m.fingerprint,
		QuietCutoff:       now.Add(-m.cfg.QuietPeriod),
		FailedRetryCutoff: now.Add(-m.cfg.FailureBackoff),
		ScanVersions:      []string{secrets.RulesVersion()},
		IncludeDone:       true,
	})
	if err != nil {
		return status, err
	}
	status.EligibleBacklog = len(backlog)
	return status, nil
}

func (m *Manager) generation(
	ctx context.Context,
) (db.ExtractGeneration, bool, error) {
	generations, err := m.cfg.DB.ExtractGenerations(ctx)
	if err != nil {
		return db.ExtractGeneration{}, false, err
	}
	for _, generation := range generations {
		if generation.Fingerprint == m.fingerprint {
			return generation, true, nil
		}
	}
	return db.ExtractGeneration{}, false, nil
}

// unitsDigest fingerprints a session's derived unit list so growth or
// re-segmentation is detected as a content change. Hashing the units rather
// than raw messages means digest stability tracks exactly what the model
// would see.
func unitsDigest(units []Unit) string {
	h := sha256.New()
	for _, unit := range units {
		fmt.Fprintf(h, "%s\x1f%d\x1f%d\x1f%d\x1f",
			unit.Role, unit.OrdinalStart, unit.OrdinalEnd,
			utf8.RuneCountInString(unit.Text),
		)
		h.Write([]byte(unit.Text))
		h.Write([]byte{0x1e})
	}
	return hex.EncodeToString(h.Sum(nil))
}
