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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const (
	lastPushStateKey                   = "duckdb_last_push_at"
	lastPushBoundaryStateKey           = "duckdb_last_push_boundary_state"
	transcriptRevisionBackfillStateKey = "duckdb_transcript_revision_backfill_v1"
	localSyncTimestampLayout           = "2006-01-02T15:04:05.000Z"
)

type syncState struct {
	Cutoff       string            `json:"cutoff"`
	Fingerprints map[string]string `json:"fingerprints"`
}

// Sync manages push-only mirroring from the SQLite primary archive to DuckDB.
type Sync struct {
	duck                *sql.DB
	local               *db.DB
	machine             string
	syncStateScope      string
	backfillTargetScope string
	projects            []string
	excludeProjects     []string
	maintenance         duckDBMaintenance

	closeOnce sync.Once
	closeErr  error
	schemaMu  sync.Mutex
	schemaOK  bool
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
	SyncStateTarget string
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
	Full                     bool
	LastPushAt               string
	Cutoff                   string
	LocalSessions            PushSessionCounts
	CandidateSessions        PushSessionCounts
	SkippedUnchangedSessions PushSessionCounts
	PushedSessions           PushSessionCounts
	DeletedStaleSessions     int
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

// SyncStatus holds summary information about the DuckDB mirror.
type SyncStatus struct {
	Machine        string `json:"machine"`
	LastPushAt     string `json:"last_push_at"`
	DuckDBSessions int    `json:"duckdb_sessions"`
	DuckDBMessages int    `json:"duckdb_messages"`
}

// New opens a DuckDB mirror file and returns a Sync instance.
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
	backfillTargetScope := opts.SyncStateTarget
	if backfillTargetScope == "" {
		backfillTargetScope = localDuckDBBackfillTargetScope(path)
	}
	return &Sync{
		duck:                duck,
		local:               local,
		machine:             machine,
		syncStateScope:      opts.SyncStateTarget,
		backfillTargetScope: backfillTargetScope,
		projects:            opts.Projects,
		excludeProjects:     opts.ExcludeProjects,
		maintenance:         duckDBCheckpointMaintenance{},
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

func (s *Sync) syncStateKey(key string) string {
	if s.syncStateScope == "" {
		return key
	}
	return key + ":" + s.syncStateScope
}

func (s *Sync) transcriptRevisionBackfillKey() string {
	key := transcriptRevisionBackfillStateKey
	scope := s.backfillTargetScope
	if scope == "" {
		scope = s.syncStateScope
	}
	if scope != "" {
		key += ":" + scope
	}
	if !s.isFiltered() {
		return key
	}
	projects := append([]string(nil), s.projects...)
	excluded := append([]string(nil), s.excludeProjects...)
	sort.Strings(projects)
	sort.Strings(excluded)
	filterScope := strings.Join(projects, "\x00") + "\x01" +
		strings.Join(excluded, "\x00")
	sum := sha256.Sum256([]byte(filterScope))
	return key + ":filter:" + hex.EncodeToString(sum[:])
}

func localDuckDBBackfillTargetScope(path string) string {
	canonical := filepath.Clean(path)
	if absolute, err := filepath.Abs(canonical); err == nil {
		canonical = absolute
	}
	sum := sha256.Sum256([]byte(canonical))
	return "path-" + hex.EncodeToString(sum[:8])
}

// EnsureSchema creates or additively migrates the DuckDB mirror schema.
func (s *Sync) EnsureSchema(ctx context.Context) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if s.schemaOK {
		return nil
	}
	if err := ensureSchema(ctx, s.duck); err != nil {
		return err
	}
	s.schemaOK = true
	return nil
}

// Status returns current DuckDB mirror row counts.
func (s *Sync) Status(ctx context.Context) (SyncStatus, error) {
	lastPushKey := s.syncStateKey(lastPushStateKey)
	lastPush, err := s.local.GetSyncState(lastPushKey)
	if err != nil {
		log.Printf("warning: reading %s: %v", lastPushKey, err)
	}
	status := SyncStatus{Machine: s.machine, LastPushAt: lastPush}
	if err := s.EnsureSchema(ctx); err != nil {
		return SyncStatus{}, err
	}
	return readMachineStatus(
		ctx, s.duck, duckDBBaseConnection, nil, s.machine, status.LastPushAt,
	)
}

// Push syncs local sessions and dependent rows to DuckDB.
func (s *Sync) Push(
	ctx context.Context, full bool, onProgress func(PushProgress),
) (PushResult, error) {
	start := time.Now()
	var result PushResult
	if err := s.EnsureSchema(ctx); err != nil {
		return result, err
	}
	if err := s.syncModelPricing(ctx); err != nil {
		return result, err
	}
	if err := s.syncCursorUsageEvents(ctx); err != nil {
		return result, err
	}
	lastPushKey := s.syncStateKey(lastPushStateKey)
	lastPush, err := s.local.GetSyncState(lastPushKey)
	if err != nil {
		return result, fmt.Errorf("reading %s: %w", lastPushKey, err)
	}
	if full {
		lastPush = ""
	}
	transcriptRevisionBackfillKey := s.transcriptRevisionBackfillKey()
	transcriptRevisionBackfillDone, err := s.local.GetSyncState(
		transcriptRevisionBackfillKey,
	)
	if err != nil {
		return result, fmt.Errorf(
			"reading %s: %w", transcriptRevisionBackfillKey, err,
		)
	}
	transcriptRevisionBackfillNeeded :=
		transcriptRevisionBackfillDone != "1"
	if transcriptRevisionBackfillNeeded {
		log.Printf(
			"duckdbsync: transcript revision backfill marker missing; forcing full push",
		)
		lastPush = ""
		full = true
	}
	if lastPush == "" && !s.isFiltered() {
		full = true
	}
	if lastPush != "" {
		count, err := s.sessionCount(ctx)
		if err != nil {
			return result, err
		}
		if count == 0 {
			log.Printf("duckdbsync: local watermark set but DuckDB is empty; forcing full push")
			lastPush = ""
			full = true
		}
	}

	cutoff := time.Now().UTC().Format(localSyncTimestampLayout)
	result.Diagnostics.Full = full
	result.Diagnostics.LastPushAt = lastPush
	result.Diagnostics.Cutoff = cutoff
	sessions, err := s.local.ListSessionsModifiedBetween(
		ctx, lastPush, cutoff, s.projects, s.excludeProjects,
	)
	if err != nil {
		return result, fmt.Errorf("listing modified sessions: %w", err)
	}
	sessionByID := make(map[string]db.Session, len(sessions))
	for _, sess := range sessions {
		sessionByID[sess.ID] = sess
	}
	if lastPush != "" {
		windowStart, err := previousLocalSyncTimestamp(lastPush)
		if err != nil {
			return result, fmt.Errorf("computing duckdb boundary window before %s: %w", lastPush, err)
		}
		boundarySessions, err := s.local.ListSessionsModifiedBetween(
			ctx, windowStart, lastPush, s.projects, s.excludeProjects,
		)
		if err != nil {
			return result, fmt.Errorf("listing duckdb boundary sessions: %w", err)
		}
		for _, sess := range boundarySessions {
			if localSessionSyncMarker(sess) != lastPush {
				continue
			}
			if _, ok := sessionByID[sess.ID]; !ok {
				sessionByID[sess.ID] = sess
				sessions = append(sessions, sess)
			}
		}
	}
	allLocalSessions, err := s.local.ListSessionsModifiedBetween(
		ctx, "", "", s.projects, s.excludeProjects,
	)
	if err != nil {
		return result, fmt.Errorf("listing local sessions: %w", err)
	}
	allLocalSessionByID := make(map[string]db.Session, len(allLocalSessions))
	for _, sess := range allLocalSessions {
		allLocalSessionByID[sess.ID] = sess
	}
	result.Diagnostics.LocalSessions = countPushSessions(allLocalSessions)
	priorFingerprints := map[string]string{}
	if !full {
		priorFingerprints, err = readSyncFingerprintsWithKey(
			s.local,
			s.syncStateKey(lastPushBoundaryStateKey),
		)
		if err != nil {
			return result, err
		}
	}

	var staleIDs []string
	var mirrorSessionIDs map[string]bool
	err = s.withDuckTx(ctx, "delete hard-deleted sessions", func(tx *sql.Tx) error {
		var txErr error
		staleIDs, mirrorSessionIDs, txErr = s.deleteHardDeletedMirrorSessions(
			ctx, tx, allLocalSessions, s.machine, s.projects, s.excludeProjects,
		)
		return txErr
	})
	if err != nil {
		return result, err
	}
	for _, id := range staleIDs {
		delete(priorFingerprints, id)
	}
	result.Diagnostics.DeletedStaleSessions = len(staleIDs)
	if !full {
		missingMirrorIDs := pruneMissingMirrorFingerprints(
			priorFingerprints, mirrorSessionIDs,
		)
		sessions = appendMissingMirrorRepairCandidates(
			sessions, sessionByID, allLocalSessionByID, missingMirrorIDs,
		)
	}
	sessionFingerprints, err := s.sessionFingerprints(ctx, sessions)
	if err != nil {
		return result, err
	}
	candidateSessions := append([]db.Session(nil), sessions...)
	result.Diagnostics.CandidateSessions = countPushSessions(candidateSessions)
	if !full {
		sessions = filterUnchangedSessions(sessions, priorFingerprints, sessionFingerprints)
		result.Diagnostics.SkippedUnchangedSessions = skippedPushSessions(
			candidateSessions, sessions,
		)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	pushed := make([]db.Session, 0, len(sessions))
	for start := 0; start < len(sessions); start += duckSessionPushBatchSize {
		end := min(start+duckSessionPushBatchSize, len(sessions))
		if err := s.pushSessionBatchForMode(
			ctx, sessions[start:end], start, len(sessions),
			&result, &pushed, onProgress, full,
		); err != nil {
			return result, err
		}
	}
	result.Diagnostics.PushedSessions = countPushSessions(pushed)
	if result.Errors == 0 {
		err = s.withDuckTx(ctx, "replace curation rows", func(tx *sql.Tx) error {
			if !s.isFiltered() {
				if err := s.replaceAllPinnedMessages(ctx, tx, allLocalSessions); err != nil {
					return err
				}
			} else {
				if err := s.replaceScopedPinnedMessages(ctx, tx, allLocalSessions); err != nil {
					return err
				}
			}
			return s.replaceStarredSessions(ctx, tx, allLocalSessions)
		})
		if err != nil {
			return result, err
		}
	} else {
		log.Printf(
			"duckdbsync: skipping curation refresh after %d session push errors",
			result.Errors,
		)
	}
	if result.Errors == 0 {
		if err := s.syncProjectIdentityObservations(ctx, full); err != nil {
			return result, err
		}
	} else {
		log.Printf(
			"duckdbsync: skipping project identity publication after %d session push errors",
			result.Errors,
		)
	}
	if len(pushed) > 0 || len(staleIDs) > 0 {
		if err := s.checkpointAfterMutatingPush(ctx); err != nil {
			return result, err
		}
	}
	if full && s.isFiltered() {
		// Clear the global watermark so the next unfiltered push
		// starts from scratch; finalizeState then persists fresh
		// fingerprints keyed at cutoff for later filtered runs.
		if err := clearDuckDBSyncState(s.local, s.syncStateScope); err != nil {
			return result, err
		}
	}
	advanceWatermark := result.Errors == 0
	if err := s.finalizeState(
		lastPush, cutoff, pushed, priorFingerprints,
		sessionFingerprints, advanceWatermark,
	); err != nil {
		return result, err
	}
	if transcriptRevisionBackfillNeeded && result.Errors == 0 {
		if err := s.local.SetSyncState(
			transcriptRevisionBackfillKey, "1",
		); err != nil {
			return result, fmt.Errorf(
				"updating %s: %w",
				transcriptRevisionBackfillKey, err,
			)
		}
	}
	result.Duration = time.Since(start)
	return result, nil
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
		ctx, sessions, offset, total, result, pushed, onProgress, false,
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
	full bool,
) error {
	return pushSessionBatchWith(
		ctx, sessions, offset, total, result, pushed, onProgress,
		func(ctx context.Context, sessions []db.Session) ([]int, error) {
			return s.tryPushSessionBatch(ctx, sessions, full)
		},
		func(ctx context.Context, sess db.Session) (int, error) {
			return s.pushSingleSession(ctx, sess, full)
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
	ctx context.Context, sessions []db.Session, full bool,
) ([]int, error) {
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin duckdb session batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	messagesBySession := make([]int, len(sessions))

	for i, sess := range sessions {
		messages, err := s.pushSession(ctx, tx, tx, sess, full)
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
	ctx context.Context, sess db.Session, full bool,
) (int, error) {
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin duckdb session tx %s: %w", sess.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	messages, err := s.pushSession(ctx, tx, tx, sess, full)
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

func skippedPushSessions(
	candidates []db.Session,
	pushed []db.Session,
) PushSessionCounts {
	pushedIDs := make(map[string]struct{}, len(pushed))
	for _, sess := range pushed {
		pushedIDs[sess.ID] = struct{}{}
	}
	skipped := make([]db.Session, 0, len(candidates)-len(pushed))
	for _, sess := range candidates {
		if _, ok := pushedIDs[sess.ID]; ok {
			continue
		}
		skipped = append(skipped, sess)
	}
	return countPushSessions(skipped)
}

func (s *Sync) sessionCount(ctx context.Context) (int, error) {
	var count int
	if err := s.duck.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE machine = ?`,
		s.machine,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting duckdb sessions: %w", err)
	}
	return count, nil
}

func (s *Sync) finalizeState(
	lastPush, cutoff string,
	pushed []db.Session,
	priorFingerprints map[string]string,
	sessionFingerprints map[string]string,
	advanceWatermark bool,
) error {
	if s.isFiltered() {
		// Filtered pushes must not advance the global watermark
		// past sessions from other projects, but still persist
		// fingerprints so repeated filtered runs stay incremental.
		// Use cutoff as the boundary key when lastPush is empty
		// (--full or mirror reset) so the next filtered run can
		// match fingerprints, mirroring the PostgreSQL push.
		boundaryKey := lastPush
		if boundaryKey == "" {
			boundaryKey = cutoff
		}
		return writeSyncFingerprints(
			s.local, s.syncStateKey(lastPushBoundaryStateKey),
			boundaryKey, pushed, priorFingerprints, sessionFingerprints,
		)
	}
	lastPushKey := s.syncStateKey(lastPushStateKey)
	if advanceWatermark {
		if err := s.local.SetSyncState(lastPushKey, cutoff); err != nil {
			return fmt.Errorf("updating %s: %w", lastPushKey, err)
		}
	}
	return writeSyncFingerprints(
		s.local, s.syncStateKey(lastPushBoundaryStateKey),
		cutoff, pushed, priorFingerprints, sessionFingerprints,
	)
}

func clearDuckDBSyncState(local *db.DB, scope string) error {
	lastPushKey := scopedDuckDBSyncStateKey(lastPushStateKey, scope)
	if err := local.SetSyncState(lastPushKey, ""); err != nil {
		return fmt.Errorf("clearing %s: %w", lastPushKey, err)
	}
	boundaryKey := scopedDuckDBSyncStateKey(lastPushBoundaryStateKey, scope)
	if err := local.SetSyncState(boundaryKey, ""); err != nil {
		return fmt.Errorf("clearing %s: %w", boundaryKey, err)
	}
	return nil
}

func readSyncFingerprintsWithKey(
	local *db.DB, key string,
) (map[string]string, error) {
	raw, err := local.GetSyncState(key)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}
	if raw == "" {
		return map[string]string{}, nil
	}
	var state syncState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return map[string]string{}, nil
	}
	if state.Fingerprints == nil {
		return map[string]string{}, nil
	}
	return state.Fingerprints, nil
}

func writeSyncFingerprints(
	local *db.DB,
	key string,
	cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
	sessionFingerprints map[string]string,
) error {
	state := syncState{
		Cutoff:       cutoff,
		Fingerprints: make(map[string]string, len(priorFingerprints)+len(sessions)),
	}
	for id, fp := range priorFingerprints {
		state.Fingerprints[id] = normalizeStoredFingerprint(fp)
	}
	for _, sess := range sessions {
		fp, ok := sessionFingerprints[sess.ID]
		if !ok {
			return fmt.Errorf("missing session fingerprint for %s", sess.ID)
		}
		state.Fingerprints[sess.ID] = fp
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", key, err)
	}
	if err := local.SetSyncState(key, string(data)); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	return nil
}

func scopedDuckDBSyncStateKey(key, scope string) string {
	if scope == "" {
		return key
	}
	return key + ":" + scope
}

func normalizeStoredFingerprint(value string) string {
	if len(value) == sha256.Size*2 {
		if _, err := hex.DecodeString(value); err == nil {
			return value
		}
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func filterUnchangedSessions(
	sessions []db.Session,
	priorFingerprints map[string]string,
	sessionFingerprints map[string]string,
) []db.Session {
	out := sessions[:0]
	for _, sess := range sessions {
		if priorFingerprints[sess.ID] == sessionFingerprints[sess.ID] {
			continue
		}
		out = append(out, sess)
	}
	return out
}

func pruneMissingMirrorFingerprints(
	priorFingerprints map[string]string,
	mirrorSessionIDs map[string]bool,
) []string {
	var missing []string
	for id := range priorFingerprints {
		if !mirrorSessionIDs[id] {
			delete(priorFingerprints, id)
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return missing
}

func appendMissingMirrorRepairCandidates(
	sessions []db.Session,
	sessionByID map[string]db.Session,
	allLocalSessionByID map[string]db.Session,
	missingMirrorIDs []string,
) []db.Session {
	for _, id := range missingMirrorIDs {
		sess, ok := allLocalSessionByID[id]
		if !ok {
			continue
		}
		if _, ok := sessionByID[id]; ok {
			continue
		}
		sessionByID[id] = sess
		sessions = append(sessions, sess)
	}
	return sessions
}

func previousLocalSyncTimestamp(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	return ts.Add(-time.Millisecond).UTC().Format(localSyncTimestampLayout), nil
}

func normalizeLocalSyncTimestamp(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	return ts.UTC().Format(localSyncTimestampLayout), nil
}

func localSessionSyncMarker(sess db.Session) string {
	marker, err := normalizeLocalSyncTimestamp(sess.CreatedAt)
	if err != nil || marker == "" {
		marker = sess.CreatedAt
	}
	for _, value := range []*string{
		sess.LocalModifiedAt,
		sess.EndedAt,
		sess.StartedAt,
	} {
		if value == nil {
			continue
		}
		normalized, err := normalizeLocalSyncTimestamp(*value)
		if err != nil {
			continue
		}
		if normalized > marker {
			marker = normalized
		}
	}
	if sess.FileMtime != nil {
		fileMtime := time.Unix(0, *sess.FileMtime).UTC().Format(localSyncTimestampLayout)
		if fileMtime > marker {
			marker = fileMtime
		}
	}
	return marker
}

func (s *Sync) sessionFingerprints(
	ctx context.Context,
	sessions []db.Session,
) (map[string]string, error) {
	ids := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.ID
	}
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
