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
	// EligibleBacklog counts sessions currently eligible but not done.
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
		outcome, err := m.extractSession(ctx, sessionID, staged)
		result.Units += outcome.units
		result.Entries += outcome.entries
		if err != nil {
			return result, err
		}
		switch {
		case outcome.failed:
			result.Failed++
		case outcome.done:
			result.Sessions++
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
	// their local_modified_at already lies past the new watermark.
	if opts.SessionID == "" && opts.Limit == 0 {
		m.watermark = passStart.Add(-m.cfg.QuietPeriod)
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
	changedSince := m.watermark
	if opts.Full {
		// Full passes are the recovery path: they reconcile the whole
		// archive, including sessions a bounded discovery missed.
		changedSince = time.Time{}
	}
	return m.cfg.DB.ExtractCandidates(ctx, db.ExtractCandidateQuery{
		Fingerprint:       m.fingerprint,
		QuietCutoff:       now.Add(-m.cfg.QuietPeriod),
		FailedRetryCutoff: now.Add(-m.cfg.FailureBackoff),
		ScanVersions:      []string{secrets.RulesVersion()},
		IncludeDone:       opts.Full,
		ChangedSince:      changedSince,
		Limit:             opts.Limit,
	})
}

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
		return fmt.Errorf(
			"session %s has %d recorded secret findings and is excluded "+
				"from extraction", sessionID, len(findings),
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
	ctx context.Context, sessionID string, staged bool,
) (sessionOutcome, error) {
	var outcome sessionOutcome
	session, err := m.sessionSnapshot(ctx, sessionID)
	if err != nil {
		return outcome, err
	}
	if session == nil {
		return outcome, fmt.Errorf("session %s not found", sessionID)
	}
	if err := extractableSession(sessionID, session); err != nil {
		return outcome, err
	}
	if err := m.refuseSecretFindings(ctx, sessionID); err != nil {
		return outcome, err
	}
	rows, err := m.cfg.DB.GetAllMessages(ctx, sessionID)
	if err != nil {
		return outcome, err
	}
	// Re-read the session and compare: if sync wrote to it between the
	// eligibility check and the message read, the transcript just loaded may
	// contain content the check never saw. Skip silently — the write bumped
	// local_modified_at, so the next pass retries against a settled view.
	recheck, err := m.sessionSnapshot(ctx, sessionID)
	if err != nil {
		return outcome, err
	}
	if recheck == nil || sessionSnapshotChanged(session, recheck) {
		return outcome, nil
	}
	messages := make([]Message, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, Message{
			Ordinal:  row.Ordinal,
			Role:     row.Role,
			Content:  row.Content,
			IsSystem: row.IsSystem,
		})
	}
	units := m.cfg.Segmenter.Units(messages)
	digest := unitsDigest(units)
	// A digest change means previously extracted units may have different
	// content now (an assistant run that grew re-packs into an existing
	// unit). Entry ids are positional, so stale entries would both linger
	// and block their replacements; rebuild the session's generated
	// corpus instead.
	previous, found, err := m.cfg.DB.ExtractProgress(
		ctx, sessionID, m.fingerprint,
	)
	if err != nil {
		return outcome, err
	}
	if found && previous.ContentDigest != digest {
		if _, err := m.cfg.DB.DeleteExtractedRecallEntries(
			ctx, m.fingerprint, sessionID,
		); err != nil {
			return outcome, err
		}
	}
	progress, err := m.cfg.DB.UpsertExtractProgress(
		ctx, sessionID, m.fingerprint, digest, len(units),
	)
	if err != nil {
		return outcome, err
	}
	if progress.State == db.ExtractProgressDone {
		return outcome, nil
	}
	for i := progress.UnitCursor; i < len(units); i++ {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		unit := units[i]
		entries, err := m.distillSplit(ctx, m.cfg.Prompts[unit.Role], unit.Text)
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown, not a poisoned session: leave the row
				// resumable instead of burning the failure backoff.
				return outcome, err
			}
			if markErr := m.cfg.DB.MarkExtractProgressFailed(ctx, db.ExtractFailure{
				SessionID:      sessionID,
				Fingerprint:    m.fingerprint,
				ExpectedDigest: digest,
				ExpectedCursor: i,
				LastError:      err.Error(),
			}); markErr != nil && !errors.Is(markErr, db.ErrStaleExtractProgress) {
				return outcome, markErr
			}
			outcome.failed = true
			return outcome, nil
		}
		inserted, err := m.cfg.DB.InsertExtractedRecallEntries(
			ctx, m.extractedEntries(session, unit, i, entries, staged),
		)
		if err != nil {
			return outcome, err
		}
		outcome.entries += inserted
		err = m.cfg.DB.AdvanceExtractCursor(
			ctx, sessionID, m.fingerprint, digest, i+1,
		)
		if errors.Is(err, db.ErrStaleExtractProgress) {
			// Another writer reset or took over this session; its view
			// wins and this pass simply stops contributing to it.
			return outcome, nil
		}
		if err != nil {
			return outcome, err
		}
		outcome.units++
	}
	outcome.done = true
	return outcome, nil
}

// distillSplit distills one text, halving it recursively when the model
// rejects it as too large (context overflow) or cannot emit a complete
// response for it (persistent truncation). The split floor stops recursion:
// below it the text is small enough that splitting further would only
// destroy context, so the error surfaces instead.
func (m *Manager) distillSplit(
	ctx context.Context, prompt, text string,
) ([]Entry, error) {
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
	left, err := m.distillSplit(ctx, prompt, string(runes[:mid]))
	if err != nil {
		return nil, err
	}
	right, err := m.distillSplit(ctx, prompt, string(runes[mid:]))
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
	if stats.Done == 0 || stats.Pending > 0 || stats.Partial > 0 {
		return false, nil
	}
	if stats.Entries == 0 {
		// Sessions completed but nothing was extracted: activating would
		// replace whatever is currently active with an empty corpus.
		return false, nil
	}
	backlog, err := m.cfg.DB.ExtractCandidates(ctx, db.ExtractCandidateQuery{
		Fingerprint:  m.fingerprint,
		QuietCutoff:  time.Now().Add(-m.cfg.QuietPeriod),
		ScanVersions: []string{secrets.RulesVersion()},
		Limit:        1,
	})
	if err != nil {
		return false, err
	}
	if len(backlog) > 0 {
		return false, nil
	}
	if err := m.cfg.DB.ActivateExtractGeneration(ctx, m.fingerprint); err != nil {
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
	return m.cfg.DB.ActivateExtractGeneration(ctx, m.fingerprint)
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
