package duckdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const localSyncTimestampLayout = "2006-01-02T15:04:05.000Z"

// Sync manages push-only mirroring from the SQLite primary archive to DuckDB.
type Sync struct {
	duck            *sql.DB
	local           *db.DB
	machine         string
	projects        []string
	excludeProjects []string
	maintenance     duckDBMaintenance

	closeOnce sync.Once
	closeErr  error
}

type duckDBConnectionKind int

const (
	duckDBBaseConnection duckDBConnectionKind = iota
	duckDBQuackClientConnection
)

// SyncOptions holds optional DuckDB push-scope filters.
type SyncOptions struct {
	Projects        []string
	ExcludeProjects []string
}

// PushResult summarizes a DuckDB push operation.
type PushResult struct {
	SessionsPushed int
	MessagesPushed int
	Errors         int
	Duration       time.Duration
	Diagnostics    PushDiagnostics
}

// PushDiagnostics summarizes how a DuckDB push selected sessions.
type PushDiagnostics struct {
	Full bool
	// RebuildReason is the human-readable reason a rebuild was chosen
	// instead of an incremental push (see rebuildReason); empty for an
	// incremental push (Full is false).
	RebuildReason            string
	Cutoff                   string
	LocalSessionCount        int
	CandidateSessions        PushSessionCounts
	SkippedUnchangedSessions PushSessionCounts
	PushedSessions           PushSessionCounts
	DeletedStaleSessions     int
	// CurationRefreshed reports whether this push actually rewrote
	// starred_sessions/pinned_messages, as opposed to skipping the refresh
	// because the local in-scope curation state's fingerprint matched what
	// was already recorded in the mirror (see refreshCurationIfChanged).
	CurationRefreshed bool
}

// PushSessionCounts summarizes a set of sessions without exposing content.
type PushSessionCounts struct {
	Total   int
	ByAgent map[string]int
}

// PushProgress is reported after each attempted session.
type PushProgress struct {
	SessionsDone  int
	SessionsTotal int
	MessagesDone  int
	Errors        int
}

// SyncStatus holds summary information about the DuckDB mirror, read from
// the target's own sync_metadata (see readMachineStatus) rather than any
// local watermark.
type SyncStatus struct {
	Machine         string `json:"machine"`
	LastPushAt      string `json:"last_push_at"`
	LastPushMachine string `json:"last_push_machine"`
	SchemaVersion   int    `json:"schema_version"`
	DataVersion     int    `json:"data_version"`
	Scope           string `json:"scope"`
	DuckDBSessions  int    `json:"duckdb_sessions"`
	DuckDBMessages  int    `json:"duckdb_messages"`
}

// New opens a DuckDB mirror file and returns a Sync instance. It never
// creates or migrates schema: callers reach New only from rebuildMirror
// (which creates schema itself on a fresh file) and incrementalPush (which
// requires an already-valid mirror, verified by ProbeMirror beforehand).
func New(
	path string, local *db.DB, machine string, opts SyncOptions,
) (*Sync, error) {
	if err := validateSyncInputs(local, machine); err != nil {
		return nil, err
	}
	duck, err := Open(path)
	if err != nil {
		return nil, err
	}
	return &Sync{
		duck:            duck,
		local:           local,
		machine:         machine,
		projects:        opts.Projects,
		excludeProjects: opts.ExcludeProjects,
		maintenance:     duckDBCheckpointMaintenance{},
	}, nil
}

func validateSyncInputs(local *db.DB, machine string) error {
	if local == nil {
		return fmt.Errorf("local db is required")
	}
	if machine == "" {
		return fmt.Errorf("machine name must not be empty")
	}
	return nil
}

// DB returns the underlying DuckDB connection.
func (s *Sync) DB() *sql.DB { return s.duck }

// Close closes the DuckDB connection.
func (s *Sync) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.duck.Close()
	})
	return s.closeErr
}

func (s *Sync) isFiltered() bool {
	return len(s.projects) > 0 || len(s.excludeProjects) > 0
}

// Push builds or updates the local DuckDB mirror. It probes the existing
// file read-only, rebuilds from scratch when full is set or the probe
// demands it (missing/damaged file, schema or data version drift, scope
// change, cross-process lock conflict, or a deletion cursor the local
// archive can no longer explain), and otherwise runs a bounded
// session-replace incremental push. Every rebuild logs and records its
// trigger in Diagnostics.RebuildReason, since a rebuild silently substituted
// for a requested incremental push (for example because a live 'duckdb
// serve' holds the mirror open) is otherwise invisible to the operator.
func Push(
	ctx context.Context, path string, local *db.DB, machine string,
	opts SyncOptions, full bool, onProgress func(PushProgress),
) (PushResult, error) {
	scope := canonicalPushScope(opts.Projects, opts.ExcludeProjects)
	probe, err := ProbeMirror(ctx, path)
	if err != nil {
		return PushResult{}, err
	}
	localDeletionRevision, err := local.SessionDeletionPublicationRevision(ctx)
	if err != nil {
		return PushResult{}, err
	}

	reason := rebuildReason(
		probe, scope, db.CurrentDataVersion(), full, localDeletionRevision,
	)
	if reason == "" {
		return incrementalPush(ctx, path, local, machine, opts, probe, onProgress)
	}
	log.Printf("duckdbsync: rebuilding mirror: %s", reason)
	result, err := rebuildMirror(ctx, path, local, machine, opts, onProgress)
	result.Diagnostics.RebuildReason = reason
	return result, err
}

// incrementalPush applies a bounded session-replace update against an
// already-valid mirror: apply the deletion journal delta, push sessions
// whose fingerprint changed within [probe.LastPushCutoff, cutoff], refresh
// curation and identity publication, then advance mirror metadata only if
// nothing failed.
func incrementalPush(
	ctx context.Context, path string, local *db.DB, machine string,
	opts SyncOptions, probe MirrorProbe, onProgress func(PushProgress),
) (PushResult, error) {
	s, err := New(path, local, machine, opts)
	if err != nil {
		return PushResult{}, err
	}
	defer func() { _ = s.Close() }()
	return s.runIncrementalPush(ctx, opts, probe, onProgress)
}

// runIncrementalPush is incrementalPush's algorithm, split out as a *Sync
// method so tests can construct a Sync with a stubbed maintenance policy
// (see checkpointSpy in sync_fastpath_test.go) and drive it directly instead
// of only through the free Push entry point.
func (s *Sync) runIncrementalPush(
	ctx context.Context, opts SyncOptions, probe MirrorProbe,
	onProgress func(PushProgress),
) (PushResult, error) {
	start := time.Now()
	var result PushResult

	if err := s.syncModelPricing(ctx); err != nil {
		return result, err
	}
	if err := s.syncCursorUsageEvents(ctx); err != nil {
		return result, err
	}

	through, err := s.local.SessionDeletionPublicationRevision(ctx)
	if err != nil {
		return result, err
	}
	if err := s.applyDeletionDelta(ctx, probe.DeletionRevision, through, &result); err != nil {
		return result, err
	}

	result.Diagnostics.LocalSessionCount, err = s.local.CountSessionsForMirrorScope(
		ctx, s.projects, s.excludeProjects,
	)
	if err != nil {
		return result, err
	}

	pushed, err := s.pushChangedSessions(ctx, probe, onProgress, &result)
	if err != nil {
		return result, err
	}

	identityRevision := probe.IdentityRevision
	if result.Errors == 0 {
		if err := s.replaceCuration(ctx); err != nil {
			return result, err
		}
		identityRevision, err = s.syncProjectIdentityObservations(
			ctx, probe.IdentityRevision, false,
		)
		if err != nil {
			return result, err
		}
	} else {
		log.Printf(
			"duckdbsync: skipping curation and identity refresh after %d session push errors",
			result.Errors,
		)
	}

	if len(pushed) > 0 || result.Diagnostics.DeletedStaleSessions > 0 {
		if err := s.checkpointAfterMutatingPush(ctx); err != nil {
			return result, err
		}
	}

	if result.Errors == 0 {
		if err := s.finalizeIncrementalPush(
			ctx, opts, result.Diagnostics.Cutoff, through, identityRevision,
		); err != nil {
			return result, err
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

// pushChangedSessions selects candidates in [probe.LastPushCutoff, cutoff],
// splits them into changed/unchanged by comparing local and mirror
// fingerprints, and pushes the changed ones in batches.
func (s *Sync) pushChangedSessions(
	ctx context.Context, probe MirrorProbe, onProgress func(PushProgress),
	result *PushResult,
) ([]db.Session, error) {
	cutoff := time.Now().UTC().Format(localSyncTimestampLayout)
	result.Diagnostics.Cutoff = cutoff
	candidates, err := s.local.ListSessionsForMirrorWindow(
		ctx, probe.LastPushCutoff, cutoff, s.projects, s.excludeProjects,
	)
	if err != nil {
		return nil, fmt.Errorf("listing sessions for duckdb incremental push: %w", err)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	result.Diagnostics.CandidateSessions = countPushSessions(candidates)

	changed, unchanged, fingerprints, err := s.selectChangedSessions(ctx, candidates)
	if err != nil {
		return nil, err
	}
	result.Diagnostics.SkippedUnchangedSessions = countPushSessions(unchanged)

	pushed := make([]db.Session, 0, len(changed))
	for batchStart := 0; batchStart < len(changed); batchStart += duckSessionPushBatchSize {
		end := min(batchStart+duckSessionPushBatchSize, len(changed))
		if err := s.pushSessionBatchForMode(
			ctx, changed[batchStart:end], batchStart, len(changed),
			result, &pushed, onProgress, fingerprints,
		); err != nil {
			return nil, err
		}
	}
	result.Diagnostics.PushedSessions = countPushSessions(pushed)
	return pushed, nil
}

// selectChangedSessions compares each candidate's freshly computed local
// fingerprint against what the mirror currently stores. A missing mirror
// row reads back as "", which never equals a real fingerprint, so a session
// whose mirror row disappeared (deleted directly, corrupted, never pushed)
// is treated as changed and repaired here instead of needing a separate
// orphan-repair pass.
func (s *Sync) selectChangedSessions(
	ctx context.Context, candidates []db.Session,
) (changed, unchanged []db.Session, fingerprints map[string]string, err error) {
	fingerprints, err = s.sessionFingerprints(ctx, candidates)
	if err != nil {
		return nil, nil, nil, err
	}
	mirrorFPs, err := s.readMirrorFingerprints(ctx, sessionIDs(candidates))
	if err != nil {
		return nil, nil, nil, err
	}
	changed = make([]db.Session, 0, len(candidates))
	unchanged = make([]db.Session, 0, len(candidates))
	for _, sess := range candidates {
		if fingerprints[sess.ID] != mirrorFPs[sess.ID] {
			changed = append(changed, sess)
		} else {
			unchanged = append(unchanged, sess)
		}
	}
	return changed, unchanged, fingerprints, nil
}

// readMirrorFingerprints fetches stored fingerprints for exactly the
// candidate IDs, in batches of 500, so lookup cost tracks the candidate
// window rather than mirror size.
func (s *Sync) readMirrorFingerprints(
	ctx context.Context, ids []string,
) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		if err := s.readMirrorFingerprintBatch(ctx, ids[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Sync) readMirrorFingerprintBatch(
	ctx context.Context, batch []string, out map[string]string,
) error {
	placeholders := make([]string, len(batch))
	args := make([]any, len(batch))
	for i, id := range batch {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.duck.QueryContext(ctx,
		`SELECT id, agentsview_push_fingerprint FROM sessions WHERE id IN (`+
			strings.Join(placeholders, ",")+`)`, args...,
	)
	if err != nil {
		return fmt.Errorf("reading duckdb mirror fingerprints: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var fp sql.NullString
		if err := rows.Scan(&id, &fp); err != nil {
			return fmt.Errorf("scanning duckdb mirror fingerprint: %w", err)
		}
		out[id] = fp.String
	}
	return rows.Err()
}

// finalizeIncrementalPush advances mirror metadata to reflect a completed
// push. Callers must only invoke this after confirming result.Errors == 0:
// advancing the cutoff/revisions past a partially failed push would let the
// failed sessions silently fall out of the next incremental window.
func (s *Sync) finalizeIncrementalPush(
	ctx context.Context, opts SyncOptions, cutoff string,
	deletionRevision, identityRevision int64,
) error {
	return writeMirrorMetadata(ctx, s.duck, mirrorMetadata{
		SchemaVersion:    SchemaVersion,
		DataVersion:      db.CurrentDataVersion(),
		Scope:            canonicalPushScope(opts.Projects, opts.ExcludeProjects),
		LastPushCutoff:   cutoff,
		LastPushAt:       time.Now().UTC().Format(time.RFC3339),
		LastPushMachine:  s.machine,
		DeletionRevision: deletionRevision,
		IdentityRevision: identityRevision,
	})
}

func (s *Sync) checkpointAfterMutatingPush(ctx context.Context) error {
	if s.maintenance == nil {
		return nil
	}
	return s.maintenance.checkpointAfterPush(ctx, s.duck)
}

func (s *Sync) withDuckTx(
	ctx context.Context, label string, fn func(*sql.Tx) error,
) error {
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin duckdb tx for %s: %w", label, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit duckdb tx for %s: %w", label, err)
	}
	return nil
}

const duckSessionPushBatchSize = 100

func (s *Sync) pushSessionBatch(
	ctx context.Context,
	sessions []db.Session,
	offset int,
	total int,
	result *PushResult,
	pushed *[]db.Session,
	onProgress func(PushProgress),
) error {
	return s.pushSessionBatchForMode(
		ctx, sessions, offset, total, result, pushed, onProgress, nil,
	)
}

func (s *Sync) pushSessionBatchForMode(
	ctx context.Context,
	sessions []db.Session,
	offset int,
	total int,
	result *PushResult,
	pushed *[]db.Session,
	onProgress func(PushProgress),
	fingerprints map[string]string,
) error {
	return pushSessionBatchWith(
		ctx, sessions, offset, total, result, pushed, onProgress,
		func(ctx context.Context, sessions []db.Session) ([]int, error) {
			return s.tryPushSessionBatch(ctx, sessions, fingerprints)
		},
		func(ctx context.Context, sess db.Session) (int, error) {
			return s.pushSingleSession(ctx, sess, fingerprints[sess.ID])
		},
	)
}

func pushSessionBatchWith(
	ctx context.Context,
	sessions []db.Session,
	offset int,
	total int,
	result *PushResult,
	pushed *[]db.Session,
	onProgress func(PushProgress),
	tryBatch func(context.Context, []db.Session) ([]int, error),
	pushSingle func(context.Context, db.Session) (int, error),
) error {
	messagesBySession, err := tryBatch(ctx, sessions)
	if err == nil {
		for i, sess := range sessions {
			result.SessionsPushed++
			result.MessagesPushed += messagesBySession[i]
			*pushed = append(*pushed, sess)
			reportDuckPushProgress(
				offset+i+1, total, result, onProgress,
			)
		}
		return nil
	}
	if err := fatalDuckPushError(ctx, err); err != nil {
		return err
	}
	log.Printf(
		"duckdbsync: session batch starting at %d failed; retrying sessions individually: %v",
		offset, err,
	)
	for i, sess := range sessions {
		if err := ctx.Err(); err != nil {
			return abandonDuckPushFallback(
				err, len(sessions)-i, offset+len(sessions),
				total, result, onProgress,
			)
		}
		messages, err := pushSingle(ctx, sess)
		if err != nil {
			if err := fatalDuckPushError(ctx, err); err != nil {
				return abandonDuckPushFallback(
					err, len(sessions)-i, offset+len(sessions),
					total, result, onProgress,
				)
			}
			result.Errors++
			log.Printf("duckdbsync: skipping session %s after push error: %v", sess.ID, err)
		} else {
			result.SessionsPushed++
			result.MessagesPushed += messages
			*pushed = append(*pushed, sess)
		}
		reportDuckPushProgress(offset+i+1, total, result, onProgress)
	}
	return nil
}

func abandonDuckPushFallback(
	err error,
	abandoned int,
	done int,
	total int,
	result *PushResult,
	onProgress func(PushProgress),
) error {
	if abandoned > 0 {
		result.Errors += abandoned
		log.Printf(
			"duckdbsync: abandoning %d sessions after context cancellation during individual retry: %v",
			abandoned, err,
		)
		reportDuckPushProgress(done, total, result, onProgress)
	}
	return err
}

func fatalDuckPushError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func reportDuckPushProgress(
	done int,
	total int,
	result *PushResult,
	onProgress func(PushProgress),
) {
	if onProgress == nil {
		return
	}
	onProgress(PushProgress{
		SessionsDone:  done,
		SessionsTotal: total,
		MessagesDone:  result.MessagesPushed,
		Errors:        result.Errors,
	})
}

func (s *Sync) tryPushSessionBatch(
	ctx context.Context, sessions []db.Session, fingerprints map[string]string,
) ([]int, error) {
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin duckdb session batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	messagesBySession := make([]int, len(sessions))

	for i, sess := range sessions {
		messages, err := s.pushSession(ctx, tx, sess, fingerprints[sess.ID])
		if err != nil {
			return nil, fmt.Errorf("pushing duckdb session %s: %w", sess.ID, err)
		}
		messagesBySession[i] = messages
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit duckdb session batch: %w", err)
	}
	return messagesBySession, nil
}

func (s *Sync) pushSingleSession(
	ctx context.Context, sess db.Session, fingerprint string,
) (int, error) {
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin duckdb session tx %s: %w", sess.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	messages, err := s.pushSession(ctx, tx, sess, fingerprint)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit duckdb session %s: %w", sess.ID, err)
	}
	return messages, nil
}

func countPushSessions(sessions []db.Session) PushSessionCounts {
	counts := PushSessionCounts{Total: len(sessions)}
	if len(sessions) == 0 {
		return counts
	}
	counts.ByAgent = make(map[string]int)
	for _, sess := range sessions {
		agent := strings.TrimSpace(sess.Agent)
		if agent == "" {
			agent = "unknown"
		}
		counts.ByAgent[agent]++
	}
	return counts
}

func sessionIDs(sessions []db.Session) []string {
	ids := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.ID
	}
	return ids
}

func (s *Sync) sessionFingerprints(
	ctx context.Context,
	sessions []db.Session,
) (map[string]string, error) {
	ids := sessionIDs(sessions)
	usage, err := s.local.UsageEventFingerprints(ids)
	if err != nil {
		return nil, fmt.Errorf("computing usage fingerprints: %w", err)
	}
	out := make(map[string]string, len(sessions))
	for _, sess := range sessions {
		msgs, err := s.local.GetAllMessages(ctx, sess.ID)
		if err != nil {
			return nil, fmt.Errorf("message fingerprint %s: %w", sess.ID, err)
		}
		findings, err := s.local.SessionSecretFindings(ctx, sess.ID)
		if err != nil {
			return nil, fmt.Errorf("secret finding fingerprint %s: %w", sess.ID, err)
		}
		pins, err := s.local.ListPinnedMessages(ctx, sess.ID, "")
		if err != nil {
			return nil, fmt.Errorf("pin fingerprint %s: %w", sess.ID, err)
		}
		// file_path and call_index are json:"-" on ToolCall, so the
		// marshaled Messages do not cover them. Fold in the tool-call
		// fingerprint so a file_path-only backfill invalidates the mirror.
		toolCalls, err := s.local.ToolCallFingerprint(sess.ID)
		if err != nil {
			return nil, fmt.Errorf("tool call fingerprint %s: %w", sess.ID, err)
		}
		payload := struct {
			SessionFields  []any
			Messages       []db.Message
			Usage          string
			ToolCalls      string
			SecretFindings []db.SecretFinding
			Pins           []db.PinnedMessage
		}{
			SessionFields:  duckSessionFingerprintFields(sess, s.machine),
			Messages:       msgs,
			Usage:          usage[sess.ID],
			ToolCalls:      toolCalls,
			SecretFindings: findings,
			Pins:           pins,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encoding session fingerprint %s: %w", sess.ID, err)
		}
		sum := sha256.Sum256(data)
		out[sess.ID] = hex.EncodeToString(sum[:])
	}
	return out, nil
}

func duckSessionFingerprintFields(sess db.Session, machine string) []any {
	return []any{
		sess.ID, sess.Project, machine, sess.Agent,
		sess.AgentLabel, sess.Entrypoint,
		nilString(sess.FirstMessage), nilString(sess.DisplayName),
		nilString(sess.SessionName),
		nilTime(sess.StartedAt), nilTime(sess.EndedAt),
		sess.MessageCount, sess.UserMessageCount,
		nilString(sess.FilePath), nilString(sess.FileHash),
		nilString(sess.ParentSessionID),
		sess.RelationshipType, sess.TotalOutputTokens,
		sess.PeakContextTokens, sess.HasTotalOutputTokens,
		sess.HasPeakContextTokens, sess.IsAutomated,
		sess.ToolFailureSignalCount, sess.ToolRetryCount,
		sess.EditChurnCount, sess.ConsecutiveFailureMax,
		sess.Outcome, sess.OutcomeConfidence,
		sess.EndedWithRole, sess.FinalFailureStreak,
		nilString(sess.SignalsPendingSince),
		sess.CompactionCount, sess.MidTaskCompactionCount,
		sess.ContextPressureMax, sess.HealthScore,
		nilString(sess.HealthGrade), sess.HasToolCalls,
		sess.HasContextData, sess.DataVersion,
		sess.Cwd, sess.GitBranch, sess.SourceSessionID,
		sess.SourceVersion, sess.TranscriptFidelity, sess.ParserMalformedLines,
		nilString(sess.TranscriptRevision),
		sess.IsTruncated, nilTime(sess.DeletedAt),
		timeValue(sess.CreatedAt), nilString(sess.TerminationStatus),
		sess.SecretLeakCount, sess.SecretsRulesVersion,
	}
}
