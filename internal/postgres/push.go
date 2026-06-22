package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const lastPushBoundaryStateKey = "last_push_boundary_state"

// pushMarkerIDStateKey names the local sync-state entry holding this DB's
// stable push-marker identifier. pushMarkerKeyPrefix prefixes that identifier
// to form the PG sync_metadata key under which the marker row is stored.
const (
	pushMarkerIDStateKey              = "pg_push_marker_id"
	pushMarkerKeyPrefix               = "push_marker:"
	pushMarkerMachineAliasesKeyPrefix = "push_marker_machine_aliases:"
)

var errSessionOwnershipConflict = errors.New("session ownership conflict")

// syncStateStore abstracts sync state read/write operations on the
// local database. Used by push boundary state helpers.
type syncStateStore interface {
	GetSyncState(key string) (string, error)
	SetSyncState(key, value string) error
	GetOrCreateSyncState(key, defaultValue string) (string, error)
}

type pushBoundaryState struct {
	Cutoff       string            `json:"cutoff"`
	Fingerprints map[string]string `json:"fingerprints"`
}

// PushResult summarizes a push sync operation.
type PushResult struct {
	SessionsPushed   int
	MessagesPushed   int
	SkippedConflicts int
	Errors           int
	Duration         time.Duration
}

// PushProgress is reported after each batch during Push.
type PushProgress struct {
	SessionsDone     int
	SessionsTotal    int
	MessagesDone     int
	SkippedConflicts int
	Errors           int
}

// Push syncs local sessions and messages to PostgreSQL.
// The onProgress callback, if non-nil, is called after each
// batch with current totals.
func (s *Sync) Push(
	ctx context.Context, full bool,
	onProgress func(PushProgress),
) (PushResult, error) {
	start := time.Now()
	var result PushResult

	if err := CheckDataVersionCompat(ctx, s.pg); err != nil {
		return result, err
	}

	if err := s.normalizeSyncTimestamps(ctx); err != nil {
		return result, err
	}

	lastPush, err := s.local.GetSyncState("last_push_at")
	if err != nil {
		return result, fmt.Errorf(
			"reading last_push_at: %w", err,
		)
	}
	markerID, err := s.pushMarkerID()
	if err != nil {
		return result, err
	}
	markerMachine, markerMachineAliases, markerExists, err := s.pgPushMarkerMachineState(ctx, markerID)
	if err != nil {
		return result, err
	}
	legacyMarkerMachines := pushMarkerLegacyMachines(
		markerMachine, markerMachineAliases,
	)
	if full {
		lastPush = ""
		// Caller requested a full push — the PG schema
		// may have been dropped since schemaDone was set.
		// Clear the memo so EnsureSchema re-runs.
		s.schemaMu.Lock()
		s.schemaDone = false
		s.schemaMu.Unlock()
		if err := s.normalizeSyncTimestamps(
			ctx,
		); err != nil {
			return result, err
		}
		// When a filtered full push runs, clear persisted
		// watermark and boundary state so the next
		// unfiltered push also starts from scratch.
		if s.isFiltered() {
			if err := clearPushState(s.local); err != nil {
				return result, err
			}
		}
	}

	// Coherence check: if the local watermark says we've pushed
	// before but this host's push marker is gone from PG, the PG side
	// was reset (schema dropped, DB recreated, etc.). Force a full
	// push so all sessions are re-synced.
	if lastPush != "" {
		if !markerExists {
			log.Printf(
				"pgsync: local watermark set but PG push marker " +
					"missing; PG was reset, forcing full push",
			)
			lastPush = ""
			full = true
			legacyMarkerMachines = nil
			s.schemaMu.Lock()
			s.schemaDone = false
			s.schemaMu.Unlock()
			if err := s.normalizeSyncTimestamps(
				ctx,
			); err != nil {
				return result, err
			}
			// Filtered push against a reset PG: clear
			// watermark and boundary state so the next
			// unfiltered push also starts from scratch.
			if s.isFiltered() {
				if err := clearPushState(s.local); err != nil {
					return result, err
				}
			}
		}
	}
	if err := s.syncModelPricing(ctx); err != nil {
		return result, err
	}

	cutoff := time.Now().UTC().Format(LocalSyncTimestampLayout)

	allSessions, err := s.local.ListSessionsModifiedBetween(
		ctx, lastPush, cutoff, s.projects, s.excludeProjects,
	)
	if err != nil {
		return result, fmt.Errorf(
			"listing modified sessions: %w", err,
		)
	}

	sessionByID := make(
		map[string]db.Session, len(allSessions),
	)
	for _, sess := range allSessions {
		sessionByID[sess.ID] = sess
	}

	var priorFingerprints map[string]string
	sessionFingerprints := make(map[string]string, len(sessionByID))
	if !full {
		var bErr error
		priorFingerprints, _, _, bErr = readBoundaryAndFingerprints(
			s.local, lastPush,
		)
		if bErr != nil {
			return result, bErr
		}
	}

	if lastPush != "" {
		windowStart, err := PreviousLocalSyncTimestamp(
			lastPush,
		)
		if err != nil {
			return result, fmt.Errorf(
				"computing push boundary window before %s: %w",
				lastPush, err,
			)
		}
		boundarySessions, err := s.local.ListSessionsModifiedBetween(
			ctx, windowStart, lastPush, s.projects, s.excludeProjects,
		)
		if err != nil {
			return result, fmt.Errorf(
				"listing push boundary sessions: %w", err,
			)
		}

		for _, sess := range boundarySessions {
			marker := localSessionSyncMarker(sess)
			if marker != lastPush {
				continue
			}
			if _, exists := sessionByID[sess.ID]; exists {
				continue
			}
			sessionByID[sess.ID] = sess
		}
	}

	usageFingerprints, err := s.local.UsageEventFingerprints(
		mapKeys(sessionByID),
	)
	if err != nil {
		return result, fmt.Errorf(
			"computing local usage event fingerprints: %w", err,
		)
	}
	for id, sess := range sessionByID {
		sessionFingerprints[id] = sessionPushFingerprint(
			sess, pushedSessionMachine(sess, s.machine),
			usageFingerprints[id], markerID,
		)
	}

	if len(priorFingerprints) > 0 {
		for id := range sessionByID {
			if priorFingerprints[id] == sessionFingerprints[id] {
				delete(sessionByID, id)
			}
		}
	}

	var sessions []db.Session
	for _, sess := range sessionByID {
		sessions = append(sessions, sess)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	if len(sessions) == 0 {
		if s.isFiltered() {
			// Filtered pushes must not advance the global
			// watermark but should still update fingerprints
			// so repeated filtered runs stay incremental.
			// Use cutoff as the boundary key when lastPush
			// is empty (--full/PG reset) so that the next
			// filtered run can match fingerprints.
			boundaryKey := lastPush
			if boundaryKey == "" {
				boundaryKey = cutoff
			}
			if err := writePushBoundaryState(
				s.local, boundaryKey, sessions,
				priorFingerprints, sessionFingerprints,
			); err != nil {
				return result, err
			}
		} else {
			if err := finalizePushState(
				s.local, cutoff, sessions, nil,
				sessionFingerprints,
			); err != nil {
				return result, err
			}
		}
		if err := s.writePushMarker(
			ctx, markerID, markerMachine, markerMachineAliases,
		); err != nil {
			return result, err
		}
		result.Duration = time.Since(start)
		return result, nil
	}

	var pushed []db.Session
	const batchSize = 50
	for i := 0; i < len(sessions); i += batchSize {
		end := min(i+batchSize, len(sessions))
		batch := sessions[i:end]

		batchResult, err := s.pushBatch(
			ctx, batch, full, markerID, legacyMarkerMachines,
			usageFingerprints, &pushed,
		)
		if err != nil {
			return result, err
		}
		if batchResult.ok {
			result.SessionsPushed += batchResult.sessions
			result.MessagesPushed += batchResult.messages
			result.SkippedConflicts += batchResult.skippedConflicts
		} else {
			// Batch failed — retry each session individually
			// so one bad session doesn't block the rest.
			for _, sess := range batch {
				sr, retryErr := s.pushBatch(
					ctx, []db.Session{sess},
					full, markerID, legacyMarkerMachines,
					usageFingerprints, &pushed,
				)
				if retryErr != nil {
					return result, retryErr
				}
				if sr.ok {
					result.SessionsPushed += sr.sessions
					result.MessagesPushed += sr.messages
					result.SkippedConflicts += sr.skippedConflicts
				} else {
					result.Errors++
				}
			}
		}
		if onProgress != nil {
			onProgress(PushProgress{
				SessionsDone:     end,
				SessionsTotal:    len(sessions),
				MessagesDone:     result.MessagesPushed,
				SkippedConflicts: result.SkippedConflicts,
				Errors:           result.Errors,
			})
		}
	}

	if s.isFiltered() {
		// Filtered pushes update fingerprints for pushed
		// sessions so subsequent filtered runs stay
		// incremental, but do not advance the global
		// watermark past sessions from other projects.
		// Use cutoff as the boundary key when lastPush is
		// empty (--full/PG reset) so the next filtered
		// run can match fingerprints instead of
		// re-pushing everything.
		boundaryKey := lastPush
		if boundaryKey == "" {
			boundaryKey = cutoff
		}
		if err := writePushBoundaryState(
			s.local, boundaryKey, pushed,
			priorFingerprints, sessionFingerprints,
		); err != nil {
			return result, err
		}
	} else {
		// When all sessions succeeded, advance the watermark
		// to cutoff. When some failed, keep the watermark at
		// lastPush so the failed sessions (plus any
		// already-pushed ones) are re-evaluated next time.
		// Already-pushed sessions are fingerprint-matched and
		// skipped cheaply.
		finalizeCutoff := cutoff
		var mergedFingerprints map[string]string
		if result.Errors > 0 {
			finalizeCutoff = lastPush
			mergedFingerprints = priorFingerprints
		}
		if err := finalizePushState(
			s.local, finalizeCutoff, pushed,
			mergedFingerprints, sessionFingerprints,
		); err != nil {
			return result, err
		}
	}

	// Write the push marker only after the push and local finalization
	// succeed. A reset-recovery push that fails before this point leaves
	// the marker absent, so the next push re-detects the reset and retries
	// rather than skipping the still-missing sessions.
	if err := s.writePushMarker(
		ctx, markerID, markerMachine, markerMachineAliases,
	); err != nil {
		return result, err
	}
	result.Duration = time.Since(start)
	return result, nil
}

// pgPushMarkerMachineState reports whether this host's push marker is present
// in PG and returns the current machine plus legacy machine aliases stored with
// the marker.
// A missing marker while the local watermark is set means PG was reset (schema
// dropped or recreated) since this host last pushed, so a full re-push is
// needed. Counting rows by machine cannot detect this reliably: another host
// pushing to the same PG can repopulate rows under a machine value this host
// also writes -- a remote host's sessions synced in over SSH, or this host's
// own renamed identity -- masking the loss of this host's own rows. The marker
// is per-local-DB, so no other pusher can satisfy this check.
func (s *Sync) pgPushMarkerMachineState(
	ctx context.Context, markerID string,
) (string, []string, bool, error) {
	var machine string
	err := s.pg.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		pushMarkerKeyPrefix+markerID,
	).Scan(&machine)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, false, nil
		}
		if isUndefinedTable(err) {
			return "", nil, false, nil
		}
		return "", nil, false, fmt.Errorf(
			"checking pg push marker: %w", err,
		)
	}
	aliases, err := s.pgPushMarkerMachineAliases(ctx, markerID)
	if err != nil {
		return "", nil, false, err
	}
	return machine, aliases, true, nil
}

func (s *Sync) pgPushMarkerMachineAliases(
	ctx context.Context, markerID string,
) ([]string, error) {
	var raw string
	err := s.pg.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		pushMarkerMachineAliasesKeyPrefix+markerID,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"reading pg push marker machine aliases: %w", err,
		)
	}
	var aliases []string
	if err := json.Unmarshal([]byte(raw), &aliases); err != nil {
		return nil, fmt.Errorf(
			"decoding pg push marker machine aliases: %w", err,
		)
	}
	return normalizePushMarkerMachineAliases("", aliases), nil
}

// writePushMarker records this host's push marker in PG so a later push can
// tell whether PG still holds the rows this host pushed. The primary marker
// value carries the current machine name for debugging and reset detection;
// the alias key preserves previous marker machines so ownerless legacy rows can
// be adopted after renames across multiple incremental pushes.
func (s *Sync) writePushMarker(
	ctx context.Context,
	markerID, previousMarkerMachine string,
	previousAliases []string,
) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin push marker tx: %w", err)
	}
	aliases := pushMarkerMachineAliases(
		s.machine, previousMarkerMachine, previousAliases,
	)
	aliasesJSON, err := json.Marshal(aliases)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("encoding pg push marker machine aliases: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		pushMarkerKeyPrefix+markerID, s.machine,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("writing pg push marker: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		pushMarkerMachineAliasesKeyPrefix+markerID, string(aliasesJSON),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("writing pg push marker machine aliases: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing pg push marker: %w", err)
	}
	return nil
}

func pushMarkerLegacyMachines(machine string, aliases []string) []string {
	machines := append([]string{}, aliases...)
	if machine != "" {
		machines = append(machines, machine)
	}
	return normalizePushMarkerMachineAliases("", machines)
}

func pushMarkerMachineAliases(
	currentMachine, previousMachine string,
	previousAliases []string,
) []string {
	aliases := append([]string{}, previousAliases...)
	if previousMachine != "" && previousMachine != currentMachine {
		aliases = append(aliases, previousMachine)
	}
	return normalizePushMarkerMachineAliases(currentMachine, aliases)
}

func normalizePushMarkerMachineAliases(
	currentMachine string, aliases []string,
) []string {
	seen := make(map[string]struct{}, len(aliases))
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if alias == "" || alias == currentMachine {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		out = append(out, alias)
	}
	return out
}

// pushMarkerID returns this local DB's stable push-marker identifier, creating
// and persisting a random one on first use. It is independent of the machine
// name, so a machine rename keeps the same marker, and unique per local DB, so
// a different host pushing to the same PG cannot mask this host's reset.
func (s *Sync) pushMarkerID() (string, error) {
	id, err := s.local.GetSyncState(pushMarkerIDStateKey)
	if err != nil {
		return "", fmt.Errorf("reading push marker id: %w", err)
	}
	if id != "" {
		return id, nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating push marker id: %w", err)
	}
	id = hex.EncodeToString(buf)
	storedID, err := s.local.GetOrCreateSyncState(pushMarkerIDStateKey, id)
	if err != nil {
		return "", fmt.Errorf("persisting push marker id: %w", err)
	}
	return storedID, nil
}

type batchResult struct {
	ok               bool
	sessions         int
	messages         int
	skippedConflicts int
}

var errPushComparisonPreload = errors.New(
	"push comparison preload failed",
)

// pushBatch pushes a slice of sessions within a single
// transaction. On success it appends to pushed and returns
// ok=true with session/message counts. On a session-level
// error it rolls back and returns ok=false so the caller
// can retry individually. Fatal errors (BeginTx failure)
// return a non-nil error.
func (s *Sync) pushBatch(
	ctx context.Context,
	batch []db.Session,
	full bool,
	markerID string,
	legacyMarkerMachines []string,
	sessionUsageFingerprints map[string]string,
	pushed *[]db.Session,
) (batchResult, error) {
	preloadComparisons := len(batch) > 0 && !full
	result, err := s.pushBatchAttempt(
		ctx, batch, full, markerID, legacyMarkerMachines,
		sessionUsageFingerprints, pushed, preloadComparisons,
	)
	if err == nil || !errors.Is(err, errPushComparisonPreload) {
		return result, err
	}
	log.Printf(
		"pgsync: preloading pg comparison fingerprints failed, "+
			"retrying batch without preload: %v",
		err,
	)
	return s.pushBatchAttempt(
		ctx, batch, full, markerID, legacyMarkerMachines,
		sessionUsageFingerprints, pushed, false,
	)
}

func (s *Sync) pushBatchAttempt(
	ctx context.Context,
	batch []db.Session,
	full bool,
	markerID string,
	legacyMarkerMachines []string,
	sessionUsageFingerprints map[string]string,
	pushed *[]db.Session,
	preloadComparisons bool,
) (batchResult, error) {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return batchResult{}, fmt.Errorf(
			"begin pg tx: %w", err,
		)
	}

	n := 0
	msgs := 0
	skippedConflicts := 0
	sessionIDs := make([]string, 0, len(batch))
	for _, sess := range batch {
		sessionIDs = append(sessionIDs, sess.ID)
	}
	comparisons := (*pushMessageComparison)(nil)
	if preloadComparisons && len(sessionIDs) > 0 {
		comparisonsBatch, err := readPushSessionMessageComparisons(
			ctx, tx, sessionIDs,
		)
		if err != nil {
			_ = tx.Rollback()
			return batchResult{}, fmt.Errorf(
				"%w: %w", errPushComparisonPreload, err,
			)
		}
		comparisons = comparisonsBatch
	}

	for _, sess := range batch {
		if err := s.pushSession(
			ctx, tx, sess, markerID, legacyMarkerMachines,
		); err != nil {
			if errors.Is(err, errSessionOwnershipConflict) {
				skippedConflicts++
				continue
			}
			log.Printf(
				"pgsync: session %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		msgCount, err := s.pushMessages(
			ctx, tx, sess.ID, full,
			sessionUsageFingerprints, comparisons,
		)
		if err != nil {
			log.Printf(
				"pgsync: session %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		findingsChanged, err := s.pushSecretFindings(ctx, tx, sess.ID)
		if err != nil {
			log.Printf(
				"pgsync: secret findings %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		// Bump updated_at when messages or secret findings were
		// rewritten but pushSession was a metadata no-op (its
		// WHERE clause skips unchanged rows). PG read-mode session
		// watchers rely on updated_at to surface secret-only changes.
		if msgCount > 0 || findingsChanged {
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions
				SET updated_at = NOW()
				WHERE id = $1`,
				sess.ID,
			); err != nil {
				log.Printf(
					"pgsync: bumping updated_at %s: %v",
					sess.ID, err,
				)
				_ = tx.Rollback()
				*pushed = (*pushed)[:len(*pushed)-n]
				return batchResult{}, nil
			}
		}

		*pushed = append(*pushed, sess)
		n++
		msgs += msgCount
	}

	if err := tx.Commit(); err != nil {
		log.Printf(
			"pgsync: batch commit failed: %v", err,
		)
		*pushed = (*pushed)[:len(*pushed)-n]
		return batchResult{}, nil
	}
	return batchResult{ok: true, sessions: n, messages: msgs, skippedConflicts: skippedConflicts}, nil
}

func finalizePushState(
	local syncStateStore,
	cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
	sessionFingerprints map[string]string,
) error {
	if err := local.SetSyncState(
		"last_push_at", cutoff,
	); err != nil {
		return fmt.Errorf("updating last_push_at: %w", err)
	}
	return writePushBoundaryState(
		local, cutoff, sessions, priorFingerprints,
		sessionFingerprints,
	)
}

// clearPushState resets the watermark and boundary state so that
// the next push starts from scratch. Used when a filtered push
// runs --full or detects a PG reset, to avoid leaving stale
// state that would cause the next unfiltered push to skip
// sessions.
func clearPushState(local syncStateStore) error {
	if err := local.SetSyncState(
		lastPushBoundaryStateKey, "",
	); err != nil {
		return fmt.Errorf(
			"clearing boundary state: %w", err,
		)
	}
	if err := local.SetSyncState(
		"last_push_at", "",
	); err != nil {
		return fmt.Errorf(
			"clearing last_push_at: %w", err,
		)
	}
	return nil
}

func readBoundaryAndFingerprints(
	local syncStateStore,
	cutoff string,
) (
	fingerprints map[string]string,
	boundary map[string]string,
	boundaryOK bool,
	err error,
) {
	raw, err := local.GetSyncState(
		lastPushBoundaryStateKey,
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf(
			"reading %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	if raw == "" {
		return nil, nil, false, nil
	}
	var state pushBoundaryState
	if err := json.Unmarshal(
		[]byte(raw), &state,
	); err != nil {
		return nil, nil, false, nil
	}
	fingerprints = state.Fingerprints
	if cutoff != "" &&
		state.Cutoff == cutoff &&
		state.Fingerprints != nil {
		boundary = state.Fingerprints
		boundaryOK = true
	}
	return fingerprints, boundary, boundaryOK, nil
}

func writePushBoundaryState(
	local syncStateStore,
	cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
	sessionFingerprints map[string]string,
) error {
	state := pushBoundaryState{
		Cutoff: cutoff,
		Fingerprints: make(
			map[string]string,
			len(priorFingerprints)+len(sessions),
		),
	}
	maps.Copy(state.Fingerprints, priorFingerprints)
	for _, sess := range sessions {
		fp, ok := sessionFingerprints[sess.ID]
		if !ok {
			return fmt.Errorf(
				"missing session fingerprint for %s",
				sess.ID,
			)
		}
		state.Fingerprints[sess.ID] = fp
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf(
			"encoding %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	if err := local.SetSyncState(
		lastPushBoundaryStateKey, string(data),
	); err != nil {
		return fmt.Errorf(
			"writing %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	return nil
}

func mapKeys(m map[string]db.Session) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func localSessionSyncMarker(sess db.Session) string {
	marker, err := NormalizeLocalSyncTimestamp(sess.CreatedAt)
	if err != nil || marker == "" {
		if err != nil {
			log.Printf(
				"pgsync: normalizing CreatedAt %q for "+
					"session %s: %v (skipping non-RFC3339 "+
					"value)",
				sess.CreatedAt, sess.ID, err,
			)
		}
		marker = ""
	}
	for _, value := range []*string{
		sess.LocalModifiedAt,
		sess.EndedAt,
		sess.StartedAt,
	} {
		if value == nil {
			continue
		}
		normalized, err := NormalizeLocalSyncTimestamp(*value)
		if err != nil {
			continue
		}
		if normalized > marker {
			marker = normalized
		}
	}
	if sess.FileMtime != nil {
		fileMtime := time.Unix(
			0, *sess.FileMtime,
		).UTC().Format(LocalSyncTimestampLayout)
		if fileMtime > marker {
			marker = fileMtime
		}
	}
	if marker == "" {
		log.Printf(
			"pgsync: session %s: all timestamps failed "+
				"normalization, falling back to raw "+
				"CreatedAt %q",
			sess.ID, sess.CreatedAt,
		)
		marker = sess.CreatedAt
	}
	return marker
}

// sessionPushFingerprint builds the change-detection fingerprint for a
// session. pushedMachine is the value pushSession actually writes to PG
// (pushedSessionMachine), not the raw sess.Machine: a "local"/empty sentinel
// row is written under the fallback machine, so the fingerprint must track the
// fallback to force a re-push when s.machine changes.
func sessionPushFingerprint(
	sess db.Session, pushedMachine,
	usageEventFingerprint, ownerMarker string,
) string {
	fields := []string{
		sess.ID,
		sess.Project,
		pushedMachine,
		ownerMarker,
		sess.Agent,
		stringValue(sess.FirstMessage),
		stringValue(sess.DisplayName),
		stringValue(sess.SessionName),
		stringValue(sess.StartedAt),
		stringValue(sess.EndedAt),
		stringValue(sess.DeletedAt),
		fmt.Sprintf("%d", sess.MessageCount),
		fmt.Sprintf("%d", sess.UserMessageCount),
		fmt.Sprintf("%t", sess.IsAutomated),
		fmt.Sprintf("%d", sess.TotalOutputTokens),
		fmt.Sprintf("%d", sess.PeakContextTokens),
		fmt.Sprintf("%t", sess.HasTotalOutputTokens),
		fmt.Sprintf("%t", sess.HasPeakContextTokens),
		stringValue(sess.ParentSessionID),
		sess.RelationshipType,
		stringValue(sess.FileHash),
		int64Value(sess.FileMtime),
		stringValue(sess.LocalModifiedAt),
		sess.CreatedAt,
		fmt.Sprintf("%d", sess.ToolFailureSignalCount),
		fmt.Sprintf("%d", sess.ToolRetryCount),
		fmt.Sprintf("%d", sess.EditChurnCount),
		fmt.Sprintf("%d", sess.ConsecutiveFailureMax),
		sess.Outcome,
		sess.OutcomeConfidence,
		sess.EndedWithRole,
		fmt.Sprintf("%d", sess.FinalFailureStreak),
		stringValue(sess.SignalsPendingSince),
		fmt.Sprintf("%d", sess.CompactionCount),
		fmt.Sprintf("%d", sess.MidTaskCompactionCount),
		float64Value(sess.ContextPressureMax),
		intPtrValue(sess.HealthScore),
		stringValue(sess.HealthGrade),
		fmt.Sprintf("%t", sess.HasToolCalls),
		fmt.Sprintf("%t", sess.HasContextData),
		fmt.Sprintf("%d", sess.QualitySignalVersion),
		fmt.Sprintf("%d", sess.ShortPromptCount),
		fmt.Sprintf("%t", sess.UnstructuredStart),
		fmt.Sprintf("%d", sess.MissingSuccessCriteriaCount),
		fmt.Sprintf("%d", sess.MissingVerificationCount),
		fmt.Sprintf("%d", sess.DuplicatePromptCount),
		fmt.Sprintf("%d", sess.NoCodeContextCount),
		fmt.Sprintf("%d", sess.RunawayToolLoopCount),
		fmt.Sprintf("%d", sess.DataVersion),
		sess.Cwd,
		sess.GitBranch,
		sess.SourceSessionID,
		sess.SourceVersion,
		fmt.Sprintf("%d", sess.ParserMalformedLines),
		fmt.Sprintf("%t", sess.IsTruncated),
		stringValue(sess.TerminationStatus),
		fmt.Sprintf("%d", sess.SecretLeakCount),
		sess.SecretsRulesVersion,
		usageEventFingerprint,
	}
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%d:%s", len(f), f)
	}
	return b.String()
}

// pushedSessionMachine resolves the machine field for a PG row. Old rows
// pushed before this fix with machine="local" will be repaired gradually as
// each session is modified (message count change, etc.) and re-fingerprinted.
func pushedSessionMachine(sess db.Session, fallbackMachine string) string {
	if sess.Machine != "" && sess.Machine != "local" {
		return sess.Machine
	}
	return fallbackMachine
}

func sameSessionOwner(
	existingOwnerMarker, existingMachine, markerID, pushedMachine string,
	legacyMarkerMachines []string,
) bool {
	if existingOwnerMarker != "" {
		return existingOwnerMarker == markerID
	}
	if existingMachine == "" {
		return true
	}
	if existingMachine == "local" {
		return true
	}
	if slices.Contains(legacyMarkerMachines, existingMachine) {
		return true
	}
	return existingMachine == pushedMachine
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func int64Value(value *int64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

func float64Value(value *float64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%g", *value)
}

func intPtrValue(value *int) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

// nilStr converts a nil or empty *string to SQL NULL.
// Sanitizes before checking emptiness so strings like "\x00"
// that reduce to "" are correctly returned as NULL.
func nilStr(s *string) any {
	if s == nil {
		return nil
	}
	v := sanitizePG(*s)
	if v == "" {
		return nil
	}
	return v
}

// nilStrTS converts a nil or empty *string timestamp to a
// *time.Time for PG TIMESTAMPTZ columns.
func nilStrTS(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	t, ok := ParseSQLiteTimestamp(*s)
	if !ok {
		return nil
	}
	return t
}

// pushSession upserts a single session into PG.
// File-level metadata (file_hash, file_path, file_size,
// file_mtime) is intentionally not synced to PG -- it is
// local-only and used solely by the sync engine to detect
// re-parsed sessions.
func (s *Sync) pushSession(
	ctx context.Context, tx *sql.Tx, sess db.Session, markerID string,
	legacyMarkerMachines []string,
) error {
	createdAt, _ := ParseSQLiteTimestamp(sess.CreatedAt)
	isAutomated := sess.IsAutomated
	pushedMachine := pushedSessionMachine(sess, s.machine)
	var existingMachine sql.NullString
	var existingOwnerMarker sql.NullString
	checkErr := tx.QueryRowContext(ctx,
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sess.ID,
	).Scan(&existingMachine, &existingOwnerMarker)
	if checkErr != nil && !errors.Is(checkErr, sql.ErrNoRows) {
		return fmt.Errorf("checking session ownership %s: %w", sess.ID, checkErr)
	}
	if checkErr == nil && !sameSessionOwner(
		existingOwnerMarker.String,
		existingMachine.String,
		markerID,
		pushedMachine,
		legacyMarkerMachines,
	) {
		log.Printf(
			"pgsync: session %s: skipping — already owned by machine %q, "+
				"this pusher is %q; sync from the origin machine to update",
			sess.ID, existingMachine.String, pushedMachine,
		)
		return errSessionOwnershipConflict
	}
	if legacyMarkerMachines == nil {
		legacyMarkerMachines = []string{}
	}
	legacyMarkerMachinesJSON, err := json.Marshal(legacyMarkerMachines)
	if err != nil {
		return fmt.Errorf("encoding legacy marker machines: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent,
			first_message, display_name, session_name,
			created_at, started_at, ended_at, deleted_at,
			message_count, user_message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens,
			is_automated, data_version,
			cwd, git_branch, source_session_id,
			source_version, parser_malformed_lines,
			is_truncated, termination_status,
			parent_session_id, relationship_type,
			tool_failure_signal_count, tool_retry_count,
			edit_churn_count, consecutive_failure_max,
			outcome, outcome_confidence,
			ended_with_role, final_failure_streak,
			signals_pending_since,
			compaction_count, mid_task_compaction_count,
			context_pressure_max,
			health_score, health_grade,
			has_tool_calls, has_context_data,
			secret_leak_count, secrets_rules_version,
			quality_signal_version,
			short_prompt_count, unstructured_start,
			missing_success_criteria_count,
			missing_verification_count, duplicate_prompt_count,
			no_code_context_count, runaway_tool_loop_count,
			updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, $10, $11, $12,
			$13, $14, $15, $16,
			$17, $18, $19, $20,
			$21, $22, $23, $24, $25, $26, $27,
			$28, $29,
			$30, $31, $32, $33,
			$34, $35, $36, $37,
			$38,
			$39, $40,
			$41,
			$42, $43, $44, $45,
			$46, $47,
			$48, $49, $50, $51, $52, $53, $54, $55,
			NOW()
		)
		ON CONFLICT (id) DO UPDATE SET
			machine = EXCLUDED.machine,
			owner_marker = EXCLUDED.owner_marker,
			project = EXCLUDED.project,
			agent = EXCLUDED.agent,
			first_message = EXCLUDED.first_message,
			display_name = EXCLUDED.display_name,
			session_name = EXCLUDED.session_name,
			created_at = EXCLUDED.created_at,
			started_at = EXCLUDED.started_at,
			ended_at = EXCLUDED.ended_at,
			deleted_at = EXCLUDED.deleted_at,
			message_count = EXCLUDED.message_count,
			user_message_count = EXCLUDED.user_message_count,
			total_output_tokens = EXCLUDED.total_output_tokens,
			peak_context_tokens = EXCLUDED.peak_context_tokens,
			has_total_output_tokens = EXCLUDED.has_total_output_tokens,
			has_peak_context_tokens = EXCLUDED.has_peak_context_tokens,
			is_automated = EXCLUDED.is_automated,
			data_version = EXCLUDED.data_version,
			cwd = EXCLUDED.cwd,
			git_branch = EXCLUDED.git_branch,
			source_session_id = EXCLUDED.source_session_id,
			source_version = EXCLUDED.source_version,
			parser_malformed_lines = EXCLUDED.parser_malformed_lines,
			is_truncated = EXCLUDED.is_truncated,
			termination_status = EXCLUDED.termination_status,
			parent_session_id = EXCLUDED.parent_session_id,
			relationship_type = EXCLUDED.relationship_type,
			tool_failure_signal_count = EXCLUDED.tool_failure_signal_count,
			tool_retry_count = EXCLUDED.tool_retry_count,
			edit_churn_count = EXCLUDED.edit_churn_count,
			consecutive_failure_max = EXCLUDED.consecutive_failure_max,
			outcome = EXCLUDED.outcome,
			outcome_confidence = EXCLUDED.outcome_confidence,
			ended_with_role = EXCLUDED.ended_with_role,
			final_failure_streak = EXCLUDED.final_failure_streak,
			signals_pending_since = EXCLUDED.signals_pending_since,
			compaction_count = EXCLUDED.compaction_count,
			mid_task_compaction_count = EXCLUDED.mid_task_compaction_count,
			context_pressure_max = EXCLUDED.context_pressure_max,
			health_score = EXCLUDED.health_score,
			health_grade = EXCLUDED.health_grade,
			has_tool_calls = EXCLUDED.has_tool_calls,
			has_context_data = EXCLUDED.has_context_data,
			secret_leak_count = EXCLUDED.secret_leak_count,
			secrets_rules_version = EXCLUDED.secrets_rules_version,
			quality_signal_version = EXCLUDED.quality_signal_version,
			short_prompt_count = EXCLUDED.short_prompt_count,
			unstructured_start = EXCLUDED.unstructured_start,
			missing_success_criteria_count = EXCLUDED.missing_success_criteria_count,
			missing_verification_count = EXCLUDED.missing_verification_count,
			duplicate_prompt_count = EXCLUDED.duplicate_prompt_count,
			no_code_context_count = EXCLUDED.no_code_context_count,
			runaway_tool_loop_count = EXCLUDED.runaway_tool_loop_count,
			updated_at = NOW()
		WHERE ((
				sessions.owner_marker = ''
				AND (sessions.machine = EXCLUDED.machine
					OR sessions.machine = 'local'
					OR sessions.machine = ''
					OR sessions.machine IN (
						SELECT jsonb_array_elements_text($56::jsonb)
					))
			)
			OR sessions.owner_marker = EXCLUDED.owner_marker)
			AND (
			sessions.machine IS DISTINCT FROM EXCLUDED.machine
			OR sessions.owner_marker IS DISTINCT FROM EXCLUDED.owner_marker
			OR sessions.project IS DISTINCT FROM EXCLUDED.project
			OR sessions.agent IS DISTINCT FROM EXCLUDED.agent
			OR sessions.first_message IS DISTINCT FROM EXCLUDED.first_message
			OR sessions.display_name IS DISTINCT FROM EXCLUDED.display_name
			OR sessions.session_name IS DISTINCT FROM EXCLUDED.session_name
			OR sessions.created_at IS DISTINCT FROM EXCLUDED.created_at
			OR sessions.started_at IS DISTINCT FROM EXCLUDED.started_at
			OR sessions.ended_at IS DISTINCT FROM EXCLUDED.ended_at
			OR sessions.deleted_at IS DISTINCT FROM EXCLUDED.deleted_at
			OR sessions.message_count IS DISTINCT FROM EXCLUDED.message_count
			OR sessions.user_message_count IS DISTINCT FROM EXCLUDED.user_message_count
			OR sessions.total_output_tokens IS DISTINCT FROM EXCLUDED.total_output_tokens
			OR sessions.peak_context_tokens IS DISTINCT FROM EXCLUDED.peak_context_tokens
			OR sessions.has_total_output_tokens IS DISTINCT FROM EXCLUDED.has_total_output_tokens
			OR sessions.has_peak_context_tokens IS DISTINCT FROM EXCLUDED.has_peak_context_tokens
			OR sessions.is_automated IS DISTINCT FROM EXCLUDED.is_automated
			OR sessions.data_version IS DISTINCT FROM EXCLUDED.data_version
			OR sessions.cwd IS DISTINCT FROM EXCLUDED.cwd
			OR sessions.git_branch IS DISTINCT FROM EXCLUDED.git_branch
			OR sessions.source_session_id IS DISTINCT FROM EXCLUDED.source_session_id
			OR sessions.source_version IS DISTINCT FROM EXCLUDED.source_version
			OR sessions.parser_malformed_lines IS DISTINCT FROM EXCLUDED.parser_malformed_lines
			OR sessions.is_truncated IS DISTINCT FROM EXCLUDED.is_truncated
			OR sessions.termination_status IS DISTINCT FROM EXCLUDED.termination_status
			OR sessions.parent_session_id IS DISTINCT FROM EXCLUDED.parent_session_id
			OR sessions.relationship_type IS DISTINCT FROM EXCLUDED.relationship_type
			OR sessions.tool_failure_signal_count IS DISTINCT FROM EXCLUDED.tool_failure_signal_count
			OR sessions.tool_retry_count IS DISTINCT FROM EXCLUDED.tool_retry_count
			OR sessions.edit_churn_count IS DISTINCT FROM EXCLUDED.edit_churn_count
			OR sessions.consecutive_failure_max IS DISTINCT FROM EXCLUDED.consecutive_failure_max
			OR sessions.outcome IS DISTINCT FROM EXCLUDED.outcome
			OR sessions.outcome_confidence IS DISTINCT FROM EXCLUDED.outcome_confidence
			OR sessions.ended_with_role IS DISTINCT FROM EXCLUDED.ended_with_role
			OR sessions.final_failure_streak IS DISTINCT FROM EXCLUDED.final_failure_streak
			OR sessions.signals_pending_since IS DISTINCT FROM EXCLUDED.signals_pending_since
			OR sessions.compaction_count IS DISTINCT FROM EXCLUDED.compaction_count
			OR sessions.mid_task_compaction_count IS DISTINCT FROM EXCLUDED.mid_task_compaction_count
			OR sessions.context_pressure_max IS DISTINCT FROM EXCLUDED.context_pressure_max
			OR sessions.health_score IS DISTINCT FROM EXCLUDED.health_score
			OR sessions.health_grade IS DISTINCT FROM EXCLUDED.health_grade
			OR sessions.has_tool_calls IS DISTINCT FROM EXCLUDED.has_tool_calls
			OR sessions.has_context_data IS DISTINCT FROM EXCLUDED.has_context_data
			OR sessions.secret_leak_count IS DISTINCT FROM EXCLUDED.secret_leak_count
			OR sessions.secrets_rules_version IS DISTINCT FROM EXCLUDED.secrets_rules_version
			OR sessions.quality_signal_version IS DISTINCT FROM EXCLUDED.quality_signal_version
			OR sessions.short_prompt_count IS DISTINCT FROM EXCLUDED.short_prompt_count
			OR sessions.unstructured_start IS DISTINCT FROM EXCLUDED.unstructured_start
			OR sessions.missing_success_criteria_count IS DISTINCT FROM EXCLUDED.missing_success_criteria_count
			OR sessions.missing_verification_count IS DISTINCT FROM EXCLUDED.missing_verification_count
			OR sessions.duplicate_prompt_count IS DISTINCT FROM EXCLUDED.duplicate_prompt_count
			OR sessions.no_code_context_count IS DISTINCT FROM EXCLUDED.no_code_context_count
			OR sessions.runaway_tool_loop_count IS DISTINCT FROM EXCLUDED.runaway_tool_loop_count)`,
		sess.ID, pushedMachine, markerID,
		sanitizePG(sess.Project),
		sess.Agent,
		nilStr(sess.FirstMessage),
		nilStr(sess.DisplayName),
		nilStr(sess.SessionName),
		createdAt,
		nilStrTS(sess.StartedAt),
		nilStrTS(sess.EndedAt),
		nilStrTS(sess.DeletedAt),
		sess.MessageCount, sess.UserMessageCount,
		sess.TotalOutputTokens, sess.PeakContextTokens,
		sess.HasTotalOutputTokens, sess.HasPeakContextTokens,
		isAutomated, sess.DataVersion,
		sanitizePG(sess.Cwd), sanitizePG(sess.GitBranch),
		sanitizePG(sess.SourceSessionID),
		sanitizePG(sess.SourceVersion),
		sess.ParserMalformedLines,
		sess.IsTruncated, nilStr(sess.TerminationStatus),
		nilStr(sess.ParentSessionID),
		sess.RelationshipType,
		sess.ToolFailureSignalCount, sess.ToolRetryCount,
		sess.EditChurnCount, sess.ConsecutiveFailureMax,
		sess.Outcome, sess.OutcomeConfidence,
		sanitizePG(sess.EndedWithRole), sess.FinalFailureStreak,
		nilStr(sess.SignalsPendingSince),
		sess.CompactionCount, sess.MidTaskCompactionCount,
		sess.ContextPressureMax,
		sess.HealthScore, nilStr(sess.HealthGrade),
		sess.HasToolCalls, sess.HasContextData,
		sess.SecretLeakCount, sess.SecretsRulesVersion,
		sess.QualitySignalVersion,
		sess.ShortPromptCount, sess.UnstructuredStart,
		sess.MissingSuccessCriteriaCount,
		sess.MissingVerificationCount, sess.DuplicatePromptCount,
		sess.NoCodeContextCount, sess.RunawayToolLoopCount,
		string(legacyMarkerMachinesJSON),
	)
	if err != nil {
		return err
	}
	if rowsAffected, rowsErr := result.RowsAffected(); rowsErr == nil && rowsAffected == 0 {
		currentOwnerMarker := existingOwnerMarker.String
		currentMachine := existingMachine.String
		refreshErr := tx.QueryRowContext(ctx,
			`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sess.ID,
		).Scan(&existingMachine, &existingOwnerMarker)
		if refreshErr == nil {
			currentOwnerMarker = existingOwnerMarker.String
			currentMachine = existingMachine.String
		}
		if refreshErr == nil && !sameSessionOwner(
			currentOwnerMarker, currentMachine, markerID, pushedMachine,
			legacyMarkerMachines,
		) {
			log.Printf(
				"pgsync: session %s: skipping — already owned by machine %q, this pusher is %q; sync from the origin machine to update",
				sess.ID, currentMachine, pushedMachine,
			)
			return errSessionOwnershipConflict
		}
	}
	return nil
}

// pushMessages replaces a session's messages and tool calls
// in PG. It skips the replacement when the PG message count
// already matches the local count, avoiding redundant work
// for metadata-only changes.
func (s *Sync) pushMessages(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	full bool,
	sessionUsageFingerprints map[string]string,
	comparisons *pushMessageComparison,
) (int, error) {
	localCount, err := s.local.MessageCount(sessionID)
	if err != nil {
		return 0, fmt.Errorf(
			"counting local messages: %w", err,
		)
	}
	if localCount == 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tool_result_events WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf(
				"deleting stale pg tool_result_events: %w", err,
			)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tool_calls WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf(
				"deleting stale pg tool_calls: %w", err,
			)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM messages WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf(
				"deleting stale pg messages: %w", err,
			)
		}
		// Usage events are independent of transcript messages: a
		// session can carry token/cost accounting (e.g. a hermes
		// state.db-only session) with zero messages. Sync them here
		// too so their cost reaches PG instead of being dropped with
		// the rest of the message-replace path below.
		if err := s.replaceUsageEvents(ctx, tx, sessionID); err != nil {
			return 0, err
		}
		if err := reconcilePinnedMessages(
			ctx, tx, sessionID,
		); err != nil {
			return 0, err
		}
		return 0, nil
	}

	pgAgg, pgToolAgg, hasPreloadedComparisons := comparisonAggregates(
		sessionID, comparisons,
	)
	if !hasPreloadedComparisons {
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*),
				COALESCE(SUM(content_length), 0),
				COALESCE(MAX(content_length), 0),
				COALESCE(MIN(content_length), 0),
				COALESCE(
					STRING_AGG(ordinal::text, ',' ORDER BY ordinal)
						FILTER (WHERE is_system),
					''
				)
			 FROM messages
			 WHERE session_id = $1`,
			sessionID,
		).Scan(
			&pgAgg.Count, &pgAgg.Sum,
			&pgAgg.Max, &pgAgg.Min,
			&pgAgg.SysFP,
		); err != nil {
			return 0, fmt.Errorf(
				"counting pg messages: %w", err,
			)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*),
				COALESCE(SUM(result_content_length), 0)
			 FROM tool_calls
			 WHERE session_id = $1`,
			sessionID,
		).Scan(&pgToolAgg.Count, &pgToolAgg.Sum); err != nil {
			return 0, fmt.Errorf(
				"counting pg tool_calls: %w", err,
			)
		}
	}

	if !full && pgAgg.Count == localCount && pgAgg.Count > 0 {
		localFP := pushLocalMessageFingerprint{}

		localFP.Sum, localFP.Max, localFP.Min, err = s.local.MessageContentFingerprint(
			sessionID,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local content fingerprint: %w",
				err,
			)
		}
		localFP.ContentHashFP, err = s.local.MessageContentHashFingerprint(
			sessionID,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local content hash fingerprint: %w",
				err,
			)
		}
		localFP.RoleTimeFP, err = localMessageRoleTimePGFingerprint(
			s.local, sessionID,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local role/time fingerprint: %w",
				err,
			)
		}
		localFP.FlagsFP, err = s.local.MessageFlagsFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local message flags fingerprint: %w",
				err,
			)
		}
		localFP.SystemFP, err = s.local.SystemMessageFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local system message fingerprint: %w", err,
			)
		}
		localFP.ToolCallCount, err = s.local.ToolCallCount(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"counting local tool_calls: %w", err,
			)
		}
		localFP.ToolCallSum, err = s.local.ToolCallContentFingerprint(
			sessionID,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local tool_call content fingerprint: %w",
				err,
			)
		}
		localFP.ToolCallFP, err = s.local.ToolCallFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local tool_call fingerprint: %w", err,
			)
		}
		localFP.TokenFP, err = s.local.MessageTokenFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local token fingerprint: %w",
				err,
			)
		}

		usageFromMap := false
		if sessionUsageFingerprints != nil {
			var ok bool
			localFP.UsageEventFP, ok = sessionUsageFingerprints[sessionID]
			usageFromMap = ok
		}
		if !usageFromMap {
			localFP.UsageEventFP, err = s.local.UsageEventFingerprint(sessionID)
			if err != nil {
				return 0, fmt.Errorf(
					"computing local usage event fingerprint: %w",
					err,
				)
			}
		}

		if comparisons == nil {
			pgContentHashFP, err := pgMessageContentHashFingerprint(
				ctx, tx, sessionID,
			)
			if err != nil {
				return 0, fmt.Errorf(
					"computing pg content hash fingerprint: %w",
					err,
				)
			}
			pgRoleTimeFP, err := pgMessageRoleTimeFingerprint(
				ctx, tx, sessionID,
			)
			if err != nil {
				return 0, fmt.Errorf(
					"computing pg role/time fingerprint: %w",
					err,
				)
			}
			pgFlagsFP, err := pgMessageFlagsFingerprint(ctx, tx, sessionID)
			if err != nil {
				return 0, fmt.Errorf(
					"computing pg message flags fingerprint: %w",
					err,
				)
			}
			pgTokenFP, err := pgMessageTokenFingerprint(ctx, tx, sessionID)
			if err != nil {
				return 0, fmt.Errorf(
					"computing pg token fingerprint: %w",
					err,
				)
			}
			pgTCFP, err := pgToolCallFingerprint(ctx, tx, sessionID)
			if err != nil {
				return 0, fmt.Errorf(
					"computing pg tool_call fingerprint: %w",
					err,
				)
			}
			pgUsageFP, err := pgUsageEventFingerprint(ctx, tx, sessionID)
			if err != nil {
				return 0, fmt.Errorf(
					"computing pg usage event fingerprint: %w",
					err,
				)
			}

			if localFP.Sum == pgAgg.Sum &&
				localFP.Max == pgAgg.Max &&
				localFP.Min == pgAgg.Min &&
				localFP.ContentHashFP == pgContentHashFP &&
				localFP.RoleTimeFP == pgRoleTimeFP &&
				localFP.FlagsFP == pgFlagsFP &&
				localFP.SystemFP == pgAgg.SysFP &&
				localFP.ToolCallCount == pgToolAgg.Count &&
				localFP.ToolCallSum == pgToolAgg.Sum &&
				localFP.ToolCallFP == pgTCFP &&
				localFP.TokenFP == pgTokenFP &&
				localFP.UsageEventFP == pgUsageFP {
				return 0, nil
			}
		} else if shouldSkipSessionMessages(
			sessionID, localCount, localFP, full, comparisons,
		) {
			return 0, nil
		}
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM tool_result_events
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf(
			"deleting pg tool_result_events: %w", err,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM tool_calls
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf(
			"deleting pg tool_calls: %w", err,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf(
			"deleting pg messages: %w", err,
		)
	}
	if err := s.replaceUsageEvents(ctx, tx, sessionID); err != nil {
		return 0, err
	}

	count := 0
	startOrdinal := 0
	for {
		msgs, err := s.local.GetMessages(
			ctx, sessionID, startOrdinal,
			db.MaxMessageLimit, true,
		)
		if err != nil {
			return count, fmt.Errorf(
				"reading local messages: %w", err,
			)
		}
		if len(msgs) == 0 {
			break
		}

		nextOrdinal := msgs[len(msgs)-1].Ordinal + 1
		if nextOrdinal <= startOrdinal {
			return count, fmt.Errorf(
				"pushMessages %s: ordinal did not "+
					"advance (start=%d, last=%d)",
				sessionID, startOrdinal,
				msgs[len(msgs)-1].Ordinal,
			)
		}

		if err := bulkInsertMessages(
			ctx, tx, sessionID, msgs,
		); err != nil {
			return count, err
		}
		if err := bulkInsertToolCalls(
			ctx, tx, sessionID, msgs,
		); err != nil {
			return count, err
		}
		if err := bulkInsertToolResultEvents(
			ctx, tx, sessionID, msgs,
		); err != nil {
			return count, err
		}
		count += len(msgs)
		startOrdinal = nextOrdinal
	}

	if err := reconcilePinnedMessages(ctx, tx, sessionID); err != nil {
		return count, err
	}

	return count, nil
}

// replaceUsageEvents replaces a session's usage_events in PG with the
// current local set. Usage events are synced independently of transcript
// messages because a session can have token/cost accounting with no
// messages at all (e.g. a hermes state.db-only session). Both the
// zero-message and the normal message-replace paths in pushMessages call
// this so a session's cost always reaches PG.
func (s *Sync) replaceUsageEvents(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM usage_events
		WHERE session_id = $1
	`, sessionID); err != nil {
		return fmt.Errorf("deleting pg usage_events: %w", err)
	}
	usageEvents, err := s.local.GetUsageEvents(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("reading local usage events: %w", err)
	}
	if err := bulkInsertUsageEvents(ctx, tx, usageEvents); err != nil {
		return err
	}
	return nil
}

func reconcilePinnedMessages(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE pinned_messages p
		SET source_uuid = m.source_uuid
		FROM messages m
		WHERE p.session_id = $1
			AND m.session_id = p.session_id
			AND m.ordinal = p.message_id
			AND p.source_uuid = ''
			AND m.source_uuid <> ''`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"backfilling pg pin source_uuid: %w", err,
		)
	}

	// Move shifted source-backed pins out of the real ordinal range
	// first. Pins already on their resolved target stay in place so
	// duplicate repairs prefer the current target row's metadata.
	// When multiple messages share a source_uuid (the schema allows
	// it), prefer the message at the pin's current message_id so a
	// correctly-placed pin is not relocated to a different duplicate.
	if _, err := tx.ExecContext(ctx, `
		WITH matched AS (
			SELECT DISTINCT ON (p.id)
				p.id, p.message_id, p.ordinal,
				m.ordinal AS target_ordinal
			FROM pinned_messages p
			JOIN messages m
				ON m.session_id = p.session_id
				AND m.source_uuid = p.source_uuid
			WHERE p.session_id = $1
				AND p.source_uuid <> ''
			ORDER BY p.id,
				CASE WHEN m.ordinal = p.message_id THEN 0 ELSE 1 END,
				m.ordinal
		),
		numbered AS (
			SELECT id,
				ROW_NUMBER() OVER (ORDER BY id) AS temp_ordinal
			FROM matched
			WHERE target_ordinal <> message_id
				OR target_ordinal <> ordinal
		)
		UPDATE pinned_messages p
		SET message_id = (-2000000000 + numbered.temp_ordinal::INT),
			ordinal = (-2000000000 + numbered.temp_ordinal::INT)
		FROM numbered
		WHERE p.id = numbered.id`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"staging pg pins for source_uuid realignment: %w", err,
		)
	}

	if _, err := tx.ExecContext(ctx, `
		WITH matched AS (
			SELECT DISTINCT ON (p.id)
				p.id, p.message_id, p.created_at,
				m.ordinal AS target_ordinal
			FROM pinned_messages p
			JOIN messages m
				ON m.session_id = p.session_id
				AND m.source_uuid = p.source_uuid
			WHERE p.session_id = $1
				AND p.source_uuid <> ''
			ORDER BY p.id,
				CASE WHEN m.ordinal = p.message_id THEN 0 ELSE 1 END,
				m.ordinal
		),
		ranked AS (
			SELECT id, target_ordinal,
				ROW_NUMBER() OVER (
					PARTITION BY target_ordinal
					ORDER BY
						(message_id = target_ordinal) DESC,
						created_at DESC,
						id DESC
				) AS target_rank
			FROM matched
		)
		DELETE FROM pinned_messages p
		USING ranked r
		WHERE p.session_id = $1
			AND r.target_rank = 1
			AND p.message_id = r.target_ordinal
			AND p.id <> r.id`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"clearing pg pin target conflicts: %w", err,
		)
	}

	if _, err := tx.ExecContext(ctx, `
		WITH matched AS (
			SELECT DISTINCT ON (p.id)
				p.id, p.message_id, p.created_at,
				m.ordinal AS target_ordinal
			FROM pinned_messages p
			JOIN messages m
				ON m.session_id = p.session_id
				AND m.source_uuid = p.source_uuid
			WHERE p.session_id = $1
				AND p.source_uuid <> ''
			ORDER BY p.id,
				CASE WHEN m.ordinal = p.message_id THEN 0 ELSE 1 END,
				m.ordinal
		),
		ranked AS (
			SELECT id, target_ordinal,
				ROW_NUMBER() OVER (
					PARTITION BY target_ordinal
					ORDER BY
						(message_id = target_ordinal) DESC,
						created_at DESC,
						id DESC
				) AS target_rank
			FROM matched
		)
		UPDATE pinned_messages p
		SET message_id = r.target_ordinal,
			ordinal = r.target_ordinal
		FROM ranked r
		WHERE p.id = r.id
			AND r.target_rank = 1`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"realigning pg pins by source_uuid: %w", err,
		)
	}

	// Prune pins whose anchor no longer exists. For source-backed
	// pins (source_uuid <> '') the canonical anchor is source_uuid,
	// so a pin must be dropped when no message in this session has
	// that source_uuid — otherwise a stale pin can survive on top
	// of an unrelated message that now occupies the same ordinal.
	// The ordinal-NOT-EXISTS clause additionally removes legacy
	// pins (source_uuid = '') with a stale ordinal and clears any
	// non-rank-1 duplicate left at the sentinel ordinal by step 2.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM pinned_messages p
		WHERE p.session_id = $1
			AND (
				(
					p.source_uuid <> ''
					AND NOT EXISTS (
						SELECT 1 FROM messages m
						WHERE m.session_id = p.session_id
							AND m.source_uuid = p.source_uuid
					)
				)
				OR NOT EXISTS (
					SELECT 1 FROM messages m
					WHERE m.session_id = p.session_id
						AND m.ordinal = p.message_id
				)
			)`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"pruning stale pg pins: %w", err,
		)
	}

	return nil
}

func pgMessageTokenFingerprint(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT ordinal, model, token_usage, context_tokens,
			output_tokens, has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		 FROM messages
		 WHERE session_id = $1
		 ORDER BY ordinal ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ordinal, contextTokens, outputTokens int
		var model, tokenUsage string
		var hasContextTokens, hasOutputTokens bool
		var claudeMsgID, claudeReqID string
		var srcType, srcSubtype, srcUUID, srcParentUUID string
		var isSidechain, isCompactBoundary bool
		if err := rows.Scan(
			&ordinal, &model, &tokenUsage, &contextTokens,
			&outputTokens, &hasContextTokens, &hasOutputTokens,
			&claudeMsgID, &claudeReqID,
			&srcType, &srcSubtype, &srcUUID, &srcParentUUID,
			&isSidechain, &isCompactBoundary,
		); err != nil {
			return "", err
		}
		fmt.Fprintf(&b,
			"%d|%d:%s|%d:%s|%d|%d|%t|%t|%s|%s|"+
				"%d:%s|%d:%s|%d:%s|%d:%s|%t|%t;",
			ordinal,
			len(model), model,
			len(tokenUsage), tokenUsage,
			contextTokens, outputTokens,
			hasContextTokens, hasOutputTokens,
			claudeMsgID, claudeReqID,
			len(srcType), srcType,
			len(srcSubtype), srcSubtype,
			len(srcUUID), srcUUID,
			len(srcParentUUID), srcParentUUID,
			isSidechain, isCompactBoundary,
		)
	}
	return b.String(), rows.Err()
}

func pgMessageContentHashFingerprint(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT ordinal, COALESCE(content, ''), content_length
		 FROM messages
		 WHERE session_id = $1
		 ORDER BY ordinal ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ordinal, contentLength int
		var content string
		if err := rows.Scan(
			&ordinal, &content, &contentLength,
		); err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(db.SanitizeUTF8(content)))
		fmt.Fprintf(&b, "%d|%d|%x;", ordinal, contentLength, sum)
	}
	return b.String(), rows.Err()
}

func localMessageRoleTimePGFingerprint(
	local *db.DB, sessionID string,
) (string, error) {
	return local.MessageRoleTimeFingerprintWithTimestampNormalizer(
		sessionID,
		pgPushTimestampFingerprintText,
	)
}

func pgPushTimestampFingerprintText(value string) string {
	t, ok := ParseSQLiteTimestamp(value)
	if !ok {
		return ""
	}
	return FormatISO8601(t.Truncate(time.Microsecond))
}

func pgMessageRoleTimeFingerprint(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT ordinal, role, timestamp
		 FROM messages
		 WHERE session_id = $1
		 ORDER BY ordinal ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ordinal int
		var role string
		var timestamp sql.NullTime
		if err := rows.Scan(
			&ordinal, &role, &timestamp,
		); err != nil {
			return "", err
		}
		role = db.SanitizeUTF8(role)
		timestampText := ""
		if timestamp.Valid {
			timestampText = FormatISO8601(timestamp.Time)
		}
		fmt.Fprintf(&b, "%d|%d:%s|%d:%s;",
			ordinal, len(role), role,
			len(timestampText), timestampText,
		)
	}
	return b.String(), rows.Err()
}

func pgMessageFlagsFingerprint(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT ordinal, is_system, has_thinking, has_tool_use,
			COALESCE(thinking_text, '')
		 FROM messages
		 WHERE session_id = $1
		 ORDER BY ordinal ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ordinal int
		var isSystem, hasThinking, hasToolUse bool
		var thinkingText string
		if err := rows.Scan(
			&ordinal, &isSystem, &hasThinking, &hasToolUse,
			&thinkingText,
		); err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(db.SanitizeUTF8(thinkingText)))
		fmt.Fprintf(&b, "%d|%t|%t|%t|%x;",
			ordinal, isSystem, hasThinking, hasToolUse, sum)
	}
	return b.String(), rows.Err()
}

func pgToolCallFingerprint(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT message_ordinal, call_index, tool_name, category,
			tool_use_id, COALESCE(input_json, ''),
			COALESCE(skill_name, ''), COALESCE(subagent_session_id, ''),
			COALESCE(result_content_length, 0),
			COALESCE(result_content, '')
		 FROM tool_calls
		 WHERE session_id = $1
		 ORDER BY message_ordinal ASC, call_index ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var messageOrdinal, callIndex, resultContentLength int
		var toolName, category, toolUseID, inputJSON string
		var skillName, subagentSessionID, resultContent string
		if err := rows.Scan(
			&messageOrdinal, &callIndex, &toolName, &category,
			&toolUseID, &inputJSON, &skillName, &subagentSessionID,
			&resultContentLength, &resultContent,
		); err != nil {
			return "", err
		}
		fmt.Fprintf(&b,
			"%d|%d|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d|%d:%s;",
			messageOrdinal, callIndex,
			len(toolName), toolName,
			len(category), category,
			len(toolUseID), toolUseID,
			len(inputJSON), inputJSON,
			len(skillName), skillName,
			len(subagentSessionID), subagentSessionID,
			resultContentLength,
			len(resultContent), resultContent,
		)
	}
	return b.String(), rows.Err()
}

func pgUsageEventFingerprint(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT message_ordinal, source, model,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			reasoning_tokens, cost_usd, cost_status, cost_source,
			occurred_at, dedup_key
		 FROM usage_events
		 WHERE session_id = $1
		 ORDER BY occurred_at NULLS FIRST, id`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ordinal sql.NullInt64
		var source, model, costStatus, costSource string
		var inputTokens, outputTokens int
		var cacheCreationInputTokens, cacheReadInputTokens int
		var reasoningTokens int
		var cost sql.NullFloat64
		var occurredAt sql.NullTime
		var dedupKey sql.NullString
		if err := rows.Scan(
			&ordinal, &source, &model,
			&inputTokens, &outputTokens,
			&cacheCreationInputTokens, &cacheReadInputTokens,
			&reasoningTokens, &cost, &costStatus, &costSource,
			&occurredAt, &dedupKey,
		); err != nil {
			return "", err
		}
		occurred := ""
		if occurredAt.Valid {
			occurred = FormatISO8601(occurredAt.Time)
		}
		fmt.Fprintf(&b,
			"%t|%d|%d:%s|%d:%s|%d|%d|%d|%d|%d|%t|%g|%d:%s|%d:%s|%d:%s|%d:%s;",
			ordinal.Valid,
			ordinal.Int64,
			len(source), source,
			len(model), model,
			inputTokens,
			outputTokens,
			cacheCreationInputTokens,
			cacheReadInputTokens,
			reasoningTokens,
			cost.Valid,
			cost.Float64,
			len(costStatus), costStatus,
			len(costSource), costSource,
			len(occurred), occurred,
			len(dedupKey.String), dedupKey.String,
		)
	}
	return b.String(), rows.Err()
}

const msgInsertBatch = 100

// bulkInsertMessages inserts messages using multi-row VALUES.
func bulkInsertMessages(
	ctx context.Context, tx *sql.Tx,
	sessionID string, msgs []db.Message,
) error {
	for i := 0; i < len(msgs); i += msgInsertBatch {
		end := min(i+msgInsertBatch, len(msgs))
		batch := msgs[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO messages (
			session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use,
			content_length, is_system, model, token_usage,
			context_tokens, output_tokens,
			has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain,
			is_compact_boundary) VALUES `)
		args := make([]any, 0, len(batch)*24)
		for j, m := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*24 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4,
				p+5, p+6, p+7, p+8, p+9,
				p+10, p+11, p+12, p+13, p+14, p+15,
				p+16, p+17, p+18, p+19, p+20,
				p+21, p+22, p+23,
			)
			var ts any
			if m.Timestamp != "" {
				if t, ok := ParseSQLiteTimestamp(
					m.Timestamp,
				); ok {
					ts = t
				}
			}
			// Sanitize every parser-derived string, not just
			// content: model and source fields come from
			// third-party session files and have carried NUL
			// bytes (e.g. raw protobuf fragments), which PG
			// rejects with SQLSTATE 22021.
			args = append(args,
				sessionID, m.Ordinal, sanitizePG(m.Role),
				sanitizePG(m.Content),
				sanitizePG(m.ThinkingText), ts,
				m.HasThinking,
				m.HasToolUse, m.ContentLength, m.IsSystem,
				sanitizePG(m.Model),
				sanitizePG(string(m.TokenUsage)),
				m.ContextTokens, m.OutputTokens,
				m.HasContextTokens, m.HasOutputTokens,
				sanitizePG(m.ClaudeMessageID),
				sanitizePG(m.ClaudeRequestID),
				sanitizePG(m.SourceType),
				sanitizePG(m.SourceSubtype),
				sanitizePG(m.SourceUUID),
				sanitizePG(m.SourceParentUUID),
				m.IsSidechain,
				m.IsCompactBoundary,
			)
		}
		if _, err := tx.ExecContext(
			ctx, b.String(), args...,
		); err != nil {
			return fmt.Errorf(
				"bulk inserting messages: %w", err,
			)
		}
	}
	return nil
}

func bulkInsertUsageEvents(
	ctx context.Context, tx *sql.Tx, events []db.UsageEvent,
) error {
	if len(events) == 0 {
		return nil
	}
	const usageBatch = 100
	for i := 0; i < len(events); i += usageBatch {
		end := min(i+usageBatch, len(events))
		batch := events[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO usage_events (
			session_id, message_ordinal, source, model,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			reasoning_tokens, cost_usd, cost_status, cost_source,
			occurred_at, dedup_key) VALUES `)
		args := make([]any, 0, len(batch)*14)
		for j, ev := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*14 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4, p+5, p+6,
				p+7, p+8, p+9, p+10, p+11, p+12, p+13,
			)
			var occurred any
			if ev.OccurredAt != "" {
				if t, ok := ParseSQLiteTimestamp(ev.OccurredAt); ok {
					occurred = t
				}
			}
			var ordinal any
			if ev.MessageOrdinal != nil {
				ordinal = *ev.MessageOrdinal
			}
			var cost any
			if ev.CostUSD != nil {
				cost = *ev.CostUSD
			}
			args = append(args,
				ev.SessionID,
				ordinal,
				sanitizePG(ev.Source),
				sanitizePG(ev.Model),
				ev.InputTokens,
				ev.OutputTokens,
				ev.CacheCreationInputTokens,
				ev.CacheReadInputTokens,
				ev.ReasoningTokens,
				cost,
				sanitizePG(ev.CostStatus),
				sanitizePG(ev.CostSource),
				occurred,
				sanitizePG(ev.DedupKey),
			)
		}
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return fmt.Errorf(
				"bulk inserting usage_events: %w", err,
			)
		}
	}
	return nil
}

// bulkInsertToolCalls inserts tool calls using multi-row VALUES.
func bulkInsertToolCalls(
	ctx context.Context, tx *sql.Tx,
	sessionID string, msgs []db.Message,
) error {
	// Collect all tool calls from messages.
	type tcRow struct {
		ordinal int
		index   int
		tc      db.ToolCall
	}
	var rows []tcRow
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			rows = append(rows, tcRow{m.Ordinal, i, tc})
		}
	}
	if len(rows) == 0 {
		return nil
	}

	const tcBatch = 50
	for i := 0; i < len(rows); i += tcBatch {
		end := min(i+tcBatch, len(rows))
		batch := rows[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO tool_calls (
			session_id, tool_name, category,
			call_index, tool_use_id, input_json,
			skill_name, result_content_length,
			result_content, subagent_session_id,
			message_ordinal) VALUES `)
		args := make([]any, 0, len(batch)*11)
		for j, r := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*11 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,"+
					"$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4, p+5,
				p+6, p+7, p+8, p+9, p+10,
			)
			args = append(args,
				sessionID,
				sanitizePG(r.tc.ToolName),
				sanitizePG(r.tc.Category),
				r.index,
				sanitizePG(r.tc.ToolUseID),
				nilIfEmpty(r.tc.InputJSON),
				nilIfEmpty(r.tc.SkillName),
				nilIfZero(r.tc.ResultContentLength),
				nilIfEmpty(r.tc.ResultContent),
				nilIfEmpty(r.tc.SubagentSessionID),
				r.ordinal,
			)
		}
		if _, err := tx.ExecContext(
			ctx, b.String(), args...,
		); err != nil {
			return fmt.Errorf(
				"bulk inserting tool_calls: %w", err,
			)
		}
	}
	return nil
}

func bulkInsertToolResultEvents(
	ctx context.Context, tx *sql.Tx,
	sessionID string, msgs []db.Message,
) error {
	type evRow struct {
		ordinal int
		index   int
		ev      db.ToolResultEvent
	}
	var rows []evRow
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			for _, ev := range tc.ResultEvents {
				rows = append(rows, evRow{m.Ordinal, i, ev})
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}

	const evBatch = 100
	for i := 0; i < len(rows); i += evBatch {
		end := min(i+evBatch, len(rows))
		batch := rows[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO tool_result_events (
			session_id, tool_call_message_ordinal, call_index,
			tool_use_id, agent_id, subagent_session_id,
			source, status, content, content_length,
			timestamp, event_index) VALUES `)
		args := make([]any, 0, len(batch)*12)
		for j, r := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*12 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,"+
					"$%d,$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4, p+5,
				p+6, p+7, p+8, p+9, p+10, p+11,
			)
			var ts any
			if r.ev.Timestamp != "" {
				if t, ok := ParseSQLiteTimestamp(r.ev.Timestamp); ok {
					ts = t
				}
			}
			args = append(args,
				sessionID,
				r.ordinal,
				r.index,
				nilIfEmpty(r.ev.ToolUseID),
				nilIfEmpty(r.ev.AgentID),
				nilIfEmpty(r.ev.SubagentSessionID),
				sanitizePG(r.ev.Source),
				sanitizePG(r.ev.Status),
				sanitizePG(r.ev.Content),
				r.ev.ContentLength,
				ts,
				r.ev.EventIndex,
			)
		}
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return fmt.Errorf("bulk inserting tool_result_events: %w", err)
		}
	}
	return nil
}

// pushSecretFindings replaces a session's secret findings in PG.
// It deletes all existing rows for the session then bulk-inserts
// the current local set. It reports whether it changed any rows
// (deleted existing or inserted new) so the caller can bump
// sessions.updated_at for secret-only changes that pushSession and
// pushMessages would otherwise miss. Per-finding rules_version is
// pushed via this table; the session-level
// sessions.secrets_rules_version is pushed by pushSession alongside
// the rest of the session columns.
func (s *Sync) pushSecretFindings(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM secret_findings WHERE session_id = $1`,
		sessionID,
	)
	if err != nil {
		return false, fmt.Errorf(
			"deleting pg secret_findings for %s: %w",
			sessionID, err,
		)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf(
			"counting deleted secret_findings for %s: %w",
			sessionID, err,
		)
	}

	findings, err := s.local.SessionSecretFindings(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf(
			"reading local secret_findings for %s: %w",
			sessionID, err,
		)
	}
	if len(findings) == 0 {
		return deleted > 0, nil
	}

	const sfBatch = 50
	for i := 0; i < len(findings); i += sfBatch {
		end := min(i+sfBatch, len(findings))
		batch := findings[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO secret_findings (
			session_id, rule_name, confidence,
			location_kind, message_ordinal,
			call_index, event_index,
			match_start, match_end, match_index,
			redacted_match, rules_version) VALUES `)
		const cols = 12
		args := make([]any, 0, len(batch)*cols)
		for j, f := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*cols + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,"+
					"$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4,
				p+5, p+6, p+7, p+8, p+9, p+10, p+11,
			)
			args = append(args,
				f.SessionID, f.RuleName, f.Confidence,
				f.LocationKind, f.MessageOrdinal,
				f.CallIndex, f.EventIndex,
				f.MatchStart, f.MatchEnd, f.MatchIndex,
				sanitizePG(f.RedactedMatch), f.RulesVersion,
			)
		}
		if _, err := tx.ExecContext(
			ctx, b.String(), args...,
		); err != nil {
			return false, fmt.Errorf(
				"bulk inserting secret_findings for %s: %w",
				sessionID, err,
			)
		}
	}
	return true, nil
}

// normalizeSyncTimestamps ensures schema exists and normalizes
// local sync state timestamps.
func (s *Sync) normalizeSyncTimestamps(
	ctx context.Context,
) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if !s.schemaDone {
		if err := EnsureSchema(
			ctx, s.pg, s.schema,
		); err != nil {
			return err
		}
		s.schemaDone = true
	}
	return NormalizeLocalSyncStateTimestamps(s.local)
}

// sanitizePG strips null bytes and replaces invalid UTF-8
// sequences so text can be safely inserted into PostgreSQL,
// which enforces strict UTF-8 encoding. It delegates to
// db.SanitizeUTF8 so the local fingerprint builders apply the
// exact same normalization.
func sanitizePG(s string) string {
	return db.SanitizeUTF8(s)
}

func nilIfEmpty(s string) any {
	s = sanitizePG(s)
	if s == "" {
		return nil
	}
	return s
}

func nilIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
