package duckdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const (
	lastPushStateKey         = "duckdb_last_push_at"
	lastPushBoundaryStateKey = "duckdb_last_push_boundary_state"
	localSyncTimestampLayout = "2006-01-02T15:04:05.000Z"
)

type syncState struct {
	Cutoff       string            `json:"cutoff"`
	Fingerprints map[string]string `json:"fingerprints"`
}

// Sync manages push-only mirroring from the SQLite primary archive to DuckDB.
type Sync struct {
	duck            *sql.DB
	local           *db.DB
	machine         string
	projects        []string
	excludeProjects []string

	closeOnce sync.Once
	closeErr  error
	schemaMu  sync.Mutex
	schemaOK  bool
}

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
}

// PushProgress is reported after each pushed session.
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
	if local == nil {
		return nil, fmt.Errorf("local db is required")
	}
	if machine == "" {
		return nil, fmt.Errorf("machine name must not be empty")
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
	}, nil
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

// EnsureSchema creates or additively migrates the DuckDB mirror schema.
func (s *Sync) EnsureSchema(ctx context.Context) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if s.schemaOK {
		return nil
	}
	if err := EnsureSchema(ctx, s.duck); err != nil {
		return err
	}
	s.schemaOK = true
	return nil
}

// Status returns current DuckDB mirror row counts.
func (s *Sync) Status(ctx context.Context) (SyncStatus, error) {
	lastPush, err := s.local.GetSyncState(lastPushStateKey)
	if err != nil {
		log.Printf("warning: reading %s: %v", lastPushStateKey, err)
	}
	status := SyncStatus{Machine: s.machine, LastPushAt: lastPush}
	if err := s.EnsureSchema(ctx); err != nil {
		return SyncStatus{}, err
	}
	if err := s.duck.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE machine = ?`,
		s.machine,
	).Scan(&status.DuckDBSessions); err != nil {
		return SyncStatus{}, fmt.Errorf("counting duckdb sessions: %w", err)
	}
	if err := s.duck.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM messages
		 WHERE session_id IN (
			SELECT id FROM sessions WHERE machine = ?
		 )`,
		s.machine,
	).Scan(&status.DuckDBMessages); err != nil {
		return SyncStatus{}, fmt.Errorf("counting duckdb messages: %w", err)
	}
	return status, nil
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

	lastPush, err := s.local.GetSyncState(lastPushStateKey)
	if err != nil {
		return result, fmt.Errorf("reading %s: %w", lastPushStateKey, err)
	}
	if full {
		lastPush = ""
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
	sessionFingerprints, err := s.sessionFingerprints(ctx, sessions)
	if err != nil {
		return result, err
	}
	priorFingerprints := map[string]string{}
	if !full {
		priorFingerprints, err = readSyncFingerprints(s.local)
		if err != nil {
			return result, err
		}
		sessions = filterUnchangedSessions(sessions, priorFingerprints, sessionFingerprints)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin duckdb tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var staleIDs []string
	staleIDs, err = deleteHardDeletedMirrorSessions(
		ctx, tx, allLocalSessions, s.machine, s.projects, s.excludeProjects,
	)
	if err != nil {
		return result, err
	}
	for _, id := range staleIDs {
		delete(priorFingerprints, id)
	}

	pushed := make([]db.Session, 0, len(sessions))
	for i, sess := range sessions {
		messages, err := s.pushSession(ctx, tx, sess)
		if err != nil {
			return result, err
		}
		result.SessionsPushed++
		result.MessagesPushed += messages
		pushed = append(pushed, sess)
		if onProgress != nil {
			onProgress(PushProgress{
				SessionsDone:  i + 1,
				SessionsTotal: len(sessions),
				MessagesDone:  result.MessagesPushed,
				Errors:        result.Errors,
			})
		}
	}
	if !s.isFiltered() {
		if err := s.replaceAllPinnedMessages(ctx, tx, allLocalSessions); err != nil {
			return result, err
		}
	} else {
		if err := s.replaceScopedPinnedMessages(ctx, tx, allLocalSessions); err != nil {
			return result, err
		}
	}
	if err := s.replaceStarredSessions(ctx, tx, allLocalSessions); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit duckdb tx: %w", err)
	}
	if full && s.isFiltered() {
		// Clear the global watermark so the next unfiltered push
		// starts from scratch; finalizeState then persists fresh
		// fingerprints keyed at cutoff for later filtered runs.
		if err := clearDuckDBSyncState(s.local); err != nil {
			return result, err
		}
	}
	if err := s.finalizeState(lastPush, cutoff, pushed, priorFingerprints, sessionFingerprints); err != nil {
		return result, err
	}
	result.Duration = time.Since(start)
	return result, nil
}

func clearSessionTables(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`DELETE FROM pinned_messages`,
		`DELETE FROM secret_findings`,
		`DELETE FROM tool_result_events`,
		`DELETE FROM tool_calls`,
		`DELETE FROM usage_events`,
		`DELETE FROM messages`,
		`DELETE FROM sessions`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("clearing duckdb full-push session table: %w", err)
		}
	}
	return nil
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
			s.local, boundaryKey, pushed, priorFingerprints, sessionFingerprints,
		)
	}
	if err := s.local.SetSyncState(lastPushStateKey, cutoff); err != nil {
		return fmt.Errorf("updating %s: %w", lastPushStateKey, err)
	}
	return writeSyncFingerprints(
		s.local, cutoff, pushed, priorFingerprints, sessionFingerprints,
	)
}

func clearDuckDBSyncState(local *db.DB) error {
	if err := local.SetSyncState(lastPushStateKey, ""); err != nil {
		return fmt.Errorf("clearing %s: %w", lastPushStateKey, err)
	}
	if err := local.SetSyncState(lastPushBoundaryStateKey, ""); err != nil {
		return fmt.Errorf("clearing %s: %w", lastPushBoundaryStateKey, err)
	}
	return nil
}

func readSyncFingerprints(local *db.DB) (map[string]string, error) {
	raw, err := local.GetSyncState(lastPushBoundaryStateKey)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", lastPushBoundaryStateKey, err)
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
		return fmt.Errorf("encoding %s: %w", lastPushBoundaryStateKey, err)
	}
	if err := local.SetSyncState(lastPushBoundaryStateKey, string(data)); err != nil {
		return fmt.Errorf("writing %s: %w", lastPushBoundaryStateKey, err)
	}
	return nil
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
		payload := struct {
			SessionFields  []any
			Messages       []db.Message
			Usage          string
			SecretFindings []db.SecretFinding
			Pins           []db.PinnedMessage
		}{
			SessionFields:  duckSessionFingerprintFields(sess, s.machine),
			Messages:       msgs,
			Usage:          usage[sess.ID],
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
		nilString(sess.FirstMessage), nilString(sess.DisplayName),
		nilString(sess.SessionName),
		nilTime(sess.StartedAt), nilTime(sess.EndedAt),
		sess.MessageCount, sess.UserMessageCount,
		nilString(sess.FilePath), sess.FileSize, sess.FileMtime,
		sess.FileInode, sess.FileDevice, nilString(sess.FileHash),
		nilTime(sess.LocalModifiedAt), nilString(sess.ParentSessionID),
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
		sess.SourceVersion, sess.ParserMalformedLines,
		sess.IsTruncated, nilTime(sess.DeletedAt),
		timeValue(sess.CreatedAt), nilString(sess.TerminationStatus),
		sess.SecretLeakCount, sess.SecretsRulesVersion,
	}
}
