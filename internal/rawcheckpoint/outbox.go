package rawcheckpoint

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawpath"
	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	defaultMaxOutboxBytes        int64 = 1 << 30
	generationMetadataBytes      int64 = 1024
	entryMetadataBytes           int64 = 512
	objectReferenceMetadataBytes int64 = 256
)

var (
	ErrOutboxFull          = errors.New("rawcheckpoint: outbox capacity exhausted")
	ErrCaptureConflict     = errors.New("rawcheckpoint: capture predecessor conflict")
	ErrReservationMissing  = errors.New("rawcheckpoint: capture reservation not found")
	ErrReservationTooSmall = errors.New("rawcheckpoint: capture reservation is too small")
)

// CoverageStatus is the capture completeness state for one configured root.
type CoverageStatus string

const (
	CoverageComplete CoverageStatus = "complete"
	CoverageDegraded CoverageStatus = "degraded"
)

// CoverageState records a persistent gap interval for one configured root.
type CoverageState struct {
	Provider         parser.AgentType `json:"provider"`
	ConfiguredRootID string           `json:"configured_root_id"`
	State            CoverageStatus   `json:"state"`
	Reason           string           `json:"reason,omitempty"`
	DegradedAt       *time.Time       `json:"degraded_at,omitempty"`
	RecoveredAt      *time.Time       `json:"recovered_at,omitempty"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

// Reservation fences outbox capacity while a capturer writes temporary files.
type Reservation struct {
	ID               string
	ConfiguredRootID string
	ReservedBytes    int64
	CreatedAt        time.Time
}

// SourceIdentity is one device-local raw source chain.
type SourceIdentity struct {
	Provider         parser.AgentType
	ConfiguredRootID string
	SourceKey        string
}

// SourceCheckpoint fences reconciliation work to the source generation that
// was current when the source page was read.
type SourceCheckpoint struct {
	Source              SourceIdentity
	CaptureID           string
	ObservationRevision int64
}

// CapturedEntry is one complete logical file in a captured generation.
type CapturedEntry struct {
	Path         string
	Length       int64
	ModTimeNS    int64
	FileIdentity string
	PrefixSHA256 string
	Appendable   bool
	Objects      []rawsync.ObjectRef
}

// CapturedGeneration is an immutable local source generation.
type CapturedGeneration struct {
	CaptureID                   string
	Source                      SourceIdentity
	PredecessorCaptureID        string
	CapturedAt                  time.Time
	Kind                        rawsync.ManifestKind
	Entries                     []CapturedEntry
	ExpectedObservationRevision *int64
}

// CaptureBaseState is the newest acknowledged or locally queued generation.
type CaptureBaseState struct {
	CaptureID           string
	ObservationRevision int64
	Kind                rawsync.ManifestKind
	Head                SourceHead
	Entries             []CapturedEntry
	PermanentlyRejected bool
}

// OutboxUsage reports charged and reserved bytes against the configured bound.
type OutboxUsage struct {
	UsedBytes     int64 `json:"used_bytes"`
	ReservedBytes int64 `json:"reserved_bytes"`
	LimitBytes    int64 `json:"limit_bytes"`
}

// GarbageCollectionReport describes normal-operation spool reclamation.
type GarbageCollectionReport struct {
	Objects int
	Bytes   int64
}

// Options configures durable outbox storage. Zero values select bounded local
// defaults so the existing Open API remains compatible.
type Options struct {
	SpoolDir       string
	MaxOutboxBytes int64
	Now            func() time.Time
}

var ErrSpoolMismatch = errors.New(
	"rawcheckpoint: checkpoint is bound to a different object spool")

// ConfiguredRoot is the stable local mapping used in remote source identity.
type ConfiguredRoot struct {
	ID        string
	Provider  parser.AgentType
	LocalPath string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// OpenWithOptions opens the checkpoint and configures its durable object spool.
func OpenWithOptions(ctx context.Context, path string, options Options) (*Store, error) {
	if options.SpoolDir == "" {
		options.SpoolDir = path + ".objects"
	}
	if options.MaxOutboxBytes == 0 {
		options.MaxOutboxBytes = defaultMaxOutboxBytes
	}
	if options.MaxOutboxBytes < 0 {
		return nil, fmt.Errorf("rawcheckpoint: maximum outbox bytes must be positive")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	spoolDir, err := filepath.Abs(options.SpoolDir)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: resolve spool directory: %s",
			checkpointFilesystemError(err))
	}
	if err := os.MkdirAll(filepath.Join(spoolDir, "objects", "sha256"), 0o700); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: create object spool: %s",
			checkpointFilesystemError(err))
	}
	if err := os.MkdirAll(filepath.Join(spoolDir, ".tmp"), 0o700); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: create temporary spool: %s",
			checkpointFilesystemError(err))
	}
	spoolDir, err = filepath.EvalSymlinks(spoolDir)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: resolve object spool: %s",
			checkpointFilesystemError(err))
	}
	processLocks, err := acquireStoreLocks(ctx, path, spoolDir)
	if err != nil {
		return nil, err
	}

	db, err := openCheckpointDB(path)
	if err != nil {
		_ = releaseStoreLocks(processLocks)
		return nil, err
	}
	store := &Store{
		db:             db,
		spoolDir:       spoolDir,
		maxOutboxBytes: options.MaxOutboxBytes,
		now:            options.Now,
		processLocks:   processLocks,
	}
	if err := store.init(ctx); err != nil {
		db.Close()
		_ = releaseStoreLocks(processLocks)
		return nil, err
	}
	if err := store.bindSpool(ctx); err != nil {
		db.Close()
		_ = releaseStoreLocks(processLocks)
		return nil, err
	}
	if _, err := store.Recover(ctx); err != nil {
		db.Close()
		_ = releaseStoreLocks(processLocks)
		return nil, err
	}
	return store, nil
}

func (s *Store) bindSpool(ctx context.Context) error {
	return s.withImmediateWrite(ctx, "bind object spool", func(conn *sql.Conn) error {
		var bound string
		var boundLimit int64
		err := conn.QueryRowContext(ctx,
			`SELECT spool_path, max_outbox_bytes FROM outbox_config WHERE id = 1`,
		).Scan(&bound, &boundLimit)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO outbox_config (id, spool_path, max_outbox_bytes)
				VALUES (1, ?, ?)`, s.spoolDir, s.maxOutboxBytes); err != nil {
				return fmt.Errorf("rawcheckpoint: bind object spool: %w", err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("rawcheckpoint: read object spool binding: %w", err)
		case bound != s.spoolDir:
			return ErrSpoolMismatch
		default:
			if boundLimit == s.maxOutboxBytes {
				return nil
			}
			if _, err := conn.ExecContext(ctx, `UPDATE outbox_config
				SET max_outbox_bytes = ? WHERE id = 1`, s.maxOutboxBytes); err != nil {
				return fmt.Errorf("rawcheckpoint: update outbox limit: %w", err)
			}
			return nil
		}
	})
}

// ResolveConfiguredRoot returns the persistent opaque identity for a provider
// root after canonicalizing symlinks.
func (s *Store) ResolveConfiguredRoot(
	ctx context.Context,
	provider parser.AgentType,
	localRoot string,
) (ConfiguredRoot, error) {
	canonical, err := canonicalConfiguredRoot(localRoot)
	if err != nil {
		return ConfiguredRoot{}, err
	}
	var resolved ConfiguredRoot
	err = s.withImmediateWrite(ctx, "resolve configured root", func(conn *sql.Conn) error {
		var createdAt, updatedAt string
		err := conn.QueryRowContext(ctx, `SELECT id, created_at, updated_at
			FROM configured_roots WHERE provider = ? AND local_root = ?`,
			string(provider), canonical,
		).Scan(&resolved.ID, &createdAt, &updatedAt)
		if err == nil {
			resolved.Provider = provider
			resolved.LocalPath = canonical
			resolved.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
			resolved.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rawcheckpoint: resolve configured root: %w", err)
		}
		id, err := randomCheckpointID()
		if err != nil {
			return err
		}
		now := s.now().UTC()
		stamp := now.Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `INSERT INTO configured_roots
			(id, provider, local_root, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`, id, string(provider), canonical, stamp, stamp); err != nil {
			return fmt.Errorf("rawcheckpoint: resolve configured root: %w", err)
		}
		resolved = ConfiguredRoot{
			ID: id, Provider: provider, LocalPath: canonical,
			CreatedAt: now, UpdatedAt: now,
		}
		return nil
	})
	return resolved, err
}

// ObjectPath returns the content-addressed local spool path for an object.
func (s *Store) ObjectPath(ref rawsync.ObjectRef) string {
	prefix := "invalid"
	if len(ref.SHA256) >= 2 {
		prefix = ref.SHA256[:2]
	}
	return filepath.Join(s.spoolDir, "objects", "sha256", prefix, ref.SHA256)
}

// CaptureTempDir is the private same-filesystem staging directory used before
// content-addressed object installation.
func (s *Store) CaptureTempDir() string {
	return filepath.Join(s.spoolDir, ".tmp")
}

// CaptureMetadataCharge returns the conservative durable metadata charge for
// one generation.
func CaptureMetadataCharge(entries, objectReferences int) int64 {
	if entries < 0 || objectReferences < 0 {
		return 0
	}
	return generationMetadataBytes + int64(entries)*entryMetadataBytes +
		int64(objectReferences)*objectReferenceMetadataBytes
}

// ReserveCapture atomically reserves root-scoped capacity. A root-scoped
// failure remains degraded until CompleteRootReconciliation records a
// successful full-root pass.
func (s *Store) ReserveCapture(
	ctx context.Context,
	configuredRootID string,
	bytes int64,
) (Reservation, error) {
	return s.reserveCapture(
		ctx, SourceIdentity{ConfiguredRootID: configuredRootID}, bytes,
	)
}

// ReserveSourceCapture atomically reserves the caller's worst-case object and
// metadata bytes and attributes capacity failure to one logical source.
func (s *Store) ReserveSourceCapture(
	ctx context.Context,
	source SourceIdentity,
	bytes int64,
) (Reservation, error) {
	if source.Provider == "" || source.SourceKey == "" {
		return Reservation{}, fmt.Errorf("rawcheckpoint: invalid source capture reservation")
	}
	return s.reserveCapture(ctx, source, bytes)
}

func (s *Store) reserveCapture(
	ctx context.Context,
	source SourceIdentity,
	bytes int64,
) (Reservation, error) {
	if source.ConfiguredRootID == "" || bytes < 0 {
		return Reservation{}, fmt.Errorf("rawcheckpoint: invalid capture reservation")
	}
	var reservation Reservation
	full := false
	err := s.withImmediateWrite(ctx, "reserve capture", func(conn *sql.Conn) error {
		var provider string
		if err := conn.QueryRowContext(ctx,
			`SELECT provider FROM configured_roots WHERE id = ?`, source.ConfiguredRootID,
		).Scan(&provider); err != nil {
			return fmt.Errorf("rawcheckpoint: reserve capture: read configured root: %w", err)
		}
		if source.Provider != "" && string(source.Provider) != provider {
			return fmt.Errorf("rawcheckpoint: reserve capture: provider mismatch")
		}
		source.Provider = parser.AgentType(provider)
		usage, err := outboxUsageConn(ctx, conn)
		if err != nil {
			return err
		}
		recyclable, err := recyclablePermanentFailureBytesConn(ctx, conn, source)
		if err != nil {
			return err
		}
		effectiveUsed := usage.UsedBytes - recyclable
		if effectiveUsed < 0 {
			return fmt.Errorf("rawcheckpoint: recyclable outbox capacity exceeds usage")
		}
		if bytes > s.maxOutboxBytes-effectiveUsed-usage.ReservedBytes {
			now := s.now().UTC()
			if err := setSourceCoverageDegradedConn(
				ctx, conn, source, "outbox_full", now,
			); err != nil {
				return err
			}
			full = true
			return nil
		}
		id, err := randomCheckpointID()
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox_reservations
			(id, provider, configured_root_id, source_key, reserved_bytes, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, string(source.Provider),
			source.ConfiguredRootID, source.SourceKey, bytes,
			now.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("rawcheckpoint: reserve capture: %w", err)
		}
		reservation = Reservation{
			ID: id, ConfiguredRootID: source.ConfiguredRootID,
			ReservedBytes: bytes, CreatedAt: now,
		}
		return nil
	})
	if err == nil && full {
		return Reservation{}, ErrOutboxFull
	}
	return reservation, err
}

// RecordSourceObservation durably invalidates older absence scans after the
// caller has positively opened a tracked source.
func (s *Store) RecordSourceObservation(
	ctx context.Context,
	source SourceIdentity,
) error {
	return s.withImmediateWrite(ctx, "record source observation", func(conn *sql.Conn) error {
		return recordSourceObservationConn(ctx, conn, source)
	})
}

func recordSourceObservationConn(
	ctx context.Context,
	conn *sql.Conn,
	source SourceIdentity,
) error {
	if _, err := conn.ExecContext(ctx, `UPDATE raw_sources
		SET observation_revision = observation_revision + 1
		WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
		string(source.Provider), source.ConfiguredRootID, source.SourceKey); err != nil {
		return fmt.Errorf("rawcheckpoint: record source observation: %w", err)
	}
	return nil
}

func recyclablePermanentFailureBytesConn(
	ctx context.Context,
	conn *sql.Conn,
	source SourceIdentity,
) (int64, error) {
	if source.Provider == "" || source.SourceKey == "" {
		return 0, nil
	}
	var reserved int
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM outbox_reservations
		WHERE provider = ? AND configured_root_id = ? AND source_key = ?
	)`, string(source.Provider), source.ConfiguredRootID, source.SourceKey).Scan(&reserved); err != nil {
		return 0, fmt.Errorf("rawcheckpoint: inspect recyclable reservation: %w", err)
	}
	if reserved != 0 {
		return 0, nil
	}
	latest, found, err := latestCaptureIDConn(ctx, conn, source)
	if err != nil || !found || latest == "" {
		return 0, err
	}
	permanentRoot, err := permanentFailureAncestorConn(ctx, conn, latest)
	if err != nil || permanentRoot == "" {
		return 0, err
	}
	var generationBytes, objectBytes int64
	if err := conn.QueryRowContext(ctx, `WITH RECURSIVE suffix(capture_id) AS (
		SELECT capture_id FROM outbox_generations WHERE capture_id = ?
		UNION ALL
		SELECT generation.capture_id FROM outbox_generations AS generation
		JOIN suffix ON generation.predecessor_capture_id = suffix.capture_id
	)
	SELECT coalesce(sum(generation.metadata_bytes), 0)
	FROM outbox_generations AS generation
	JOIN suffix ON suffix.capture_id = generation.capture_id`, permanentRoot,
	).Scan(&generationBytes); err != nil {
		return 0, fmt.Errorf("rawcheckpoint: read recyclable generation capacity: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `WITH RECURSIVE suffix(capture_id) AS (
		SELECT capture_id FROM outbox_generations WHERE capture_id = ?
		UNION ALL
		SELECT generation.capture_id FROM outbox_generations AS generation
		JOIN suffix ON generation.predecessor_capture_id = suffix.capture_id
	), suffix_refs(sha256, length, references_in_suffix) AS (
		SELECT entry_object.sha256, entry_object.length, count(*)
		FROM outbox_entry_objects AS entry_object
		JOIN suffix ON suffix.capture_id = entry_object.capture_id
		GROUP BY entry_object.sha256, entry_object.length
	)
	SELECT coalesce(sum(object.length), 0)
	FROM outbox_objects AS object
	JOIN suffix_refs AS suffix_ref
		ON suffix_ref.sha256 = object.sha256 AND suffix_ref.length = object.length
	WHERE object.state != 'remote'
		AND object.ref_count = suffix_ref.references_in_suffix`, permanentRoot,
	).Scan(&objectBytes); err != nil {
		return 0, fmt.Errorf("rawcheckpoint: read recyclable object capacity: %w", err)
	}
	return generationBytes + objectBytes, nil
}

// ReleaseReservation idempotently releases unused reserved capacity.
func (s *Store) ReleaseReservation(ctx context.Context, reservationID string) error {
	if reservationID == "" {
		return nil
	}
	return s.withImmediateWrite(ctx, "release capture reservation", func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM outbox_reservations WHERE id = ?`, reservationID); err != nil {
			return fmt.Errorf("rawcheckpoint: release capture reservation: %w", err)
		}
		return nil
	})
}

// CompleteUnchangedCapture atomically releases a verified unchanged capture's
// reservation and clears only that source's coverage failure.
func (s *Store) CompleteUnchangedCapture(
	ctx context.Context,
	reservationID string,
	source SourceIdentity,
	expectedCaptureID string,
	expectedObservationRevision int64,
) error {
	if reservationID == "" || source.Provider == "" ||
		source.ConfiguredRootID == "" || source.SourceKey == "" {
		return fmt.Errorf("rawcheckpoint: invalid unchanged capture")
	}
	return s.withImmediateWrite(ctx, "complete unchanged capture", func(conn *sql.Conn) error {
		var reservationProvider, reservationRoot, reservationSourceKey string
		err := conn.QueryRowContext(ctx,
			`SELECT provider, configured_root_id, source_key
			FROM outbox_reservations WHERE id = ?`, reservationID,
		).Scan(&reservationProvider, &reservationRoot, &reservationSourceKey)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReservationMissing
		}
		if err != nil {
			return fmt.Errorf("rawcheckpoint: complete unchanged capture: read reservation: %w", err)
		}
		if !reservationMatchesSource(
			reservationProvider, reservationRoot, reservationSourceKey, source,
		) {
			return ErrCaptureConflict
		}
		if expectedCaptureID != "" {
			result, err := conn.ExecContext(ctx, `UPDATE raw_sources
				SET observation_revision = observation_revision + 1
				WHERE provider = ? AND configured_root_id = ? AND source_key = ?
					AND latest_capture_id = ? AND observation_revision = ?`,
				string(source.Provider), source.ConfiguredRootID, source.SourceKey,
				expectedCaptureID, expectedObservationRevision)
			if err != nil {
				return fmt.Errorf("rawcheckpoint: complete unchanged capture: record observation: %w", err)
			}
			updated, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("rawcheckpoint: complete unchanged capture: record observation: %w", err)
			}
			if updated != 1 {
				return ErrCaptureConflict
			}
		}
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM outbox_reservations WHERE id = ?`, reservationID); err != nil {
			return fmt.Errorf("rawcheckpoint: complete unchanged capture: release reservation: %w", err)
		}
		return clearSourceCoverageFailureConn(ctx, conn, source, s.now().UTC())
	})
}

// CompleteRootReconciliation clears a root-wide coverage failure after a full
// source reconciliation.
func (s *Store) CompleteRootReconciliation(
	ctx context.Context,
	configuredRootID string,
) error {
	if configuredRootID == "" {
		return fmt.Errorf("rawcheckpoint: invalid root reconciliation")
	}
	return s.withImmediateWrite(ctx, "complete root reconciliation", func(conn *sql.Conn) error {
		var provider string
		if err := conn.QueryRowContext(ctx,
			`SELECT provider FROM configured_roots WHERE id = ?`, configuredRootID,
		).Scan(&provider); err != nil {
			return fmt.Errorf(
				"rawcheckpoint: complete root reconciliation: read configured root: %w", err,
			)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM raw_coverage_failures
			WHERE provider = ? AND configured_root_id = ? AND source_key = ''`,
			provider, configuredRootID); err != nil {
			return fmt.Errorf("rawcheckpoint: clear root coverage failure: %w", err)
		}
		return refreshCoverageFromFailuresConn(
			ctx, conn, parser.AgentType(provider), configuredRootID, s.now().UTC(),
		)
	})
}

// OutboxUsage returns exact charged object and metadata bytes plus active
// worst-case reservations.
func (s *Store) OutboxUsage(ctx context.Context) (OutboxUsage, error) {
	var usage OutboxUsage
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT sum(length) FROM outbox_objects WHERE state != 'remote'), 0) +
		COALESCE((SELECT sum(metadata_bytes) FROM outbox_generations), 0),
		COALESCE((SELECT sum(reserved_bytes) FROM outbox_reservations), 0)`,
	).Scan(&usage.UsedBytes, &usage.ReservedBytes)
	if err != nil {
		return OutboxUsage{}, fmt.Errorf("rawcheckpoint: read outbox usage: %w", err)
	}
	usage.LimitBytes = s.maxOutboxBytes
	if usage.LimitBytes == 0 {
		if err := s.db.QueryRowContext(ctx, `SELECT max_outbox_bytes
			FROM outbox_config WHERE id = 1`).Scan(&usage.LimitBytes); err != nil {
			return OutboxUsage{}, fmt.Errorf("rawcheckpoint: read outbox limit: %w", err)
		}
	}
	return usage, nil
}

func outboxUsageConn(ctx context.Context, conn *sql.Conn) (OutboxUsage, error) {
	var usage OutboxUsage
	err := conn.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT sum(length) FROM outbox_objects WHERE state != 'remote'), 0) +
		COALESCE((SELECT sum(metadata_bytes) FROM outbox_generations), 0),
		COALESCE((SELECT sum(reserved_bytes) FROM outbox_reservations), 0)`,
	).Scan(&usage.UsedBytes, &usage.ReservedBytes)
	if err != nil {
		return OutboxUsage{}, fmt.Errorf("rawcheckpoint: read outbox usage: %w", err)
	}
	return usage, nil
}

// Coverage returns the current completeness state for one configured root.
func (s *Store) Coverage(
	ctx context.Context,
	provider parser.AgentType,
	configuredRootID string,
) (CoverageState, bool, error) {
	var coverage CoverageState
	var state, degradedAt, recoveredAt, updatedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT state, reason, degraded_at,
		recovered_at, updated_at FROM raw_coverage
		WHERE provider = ? AND configured_root_id = ?`,
		string(provider), configuredRootID,
	).Scan(&state, &coverage.Reason, &degradedAt, &recoveredAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CoverageState{}, false, nil
	}
	if err != nil {
		return CoverageState{}, false, fmt.Errorf("rawcheckpoint: read coverage: %w", err)
	}
	coverage.Provider = provider
	coverage.ConfiguredRootID = configuredRootID
	coverage.State = CoverageStatus(state.String)
	coverage.DegradedAt = parseOptionalTime(degradedAt)
	coverage.RecoveredAt = parseOptionalTime(recoveredAt)
	coverage.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt.String)
	return coverage, true, nil
}

// CommitCapture atomically publishes one completely installed local generation.
func (s *Store) CommitCapture(
	ctx context.Context,
	reservationID string,
	generation CapturedGeneration,
) error {
	s.objectMu.Lock()
	defer s.objectMu.Unlock()
	validated, metadataBytes, uniqueObjects, err := validateCapturedGeneration(s, generation)
	if err != nil {
		return err
	}
	return s.withImmediateWrite(ctx, "commit capture", func(conn *sql.Conn) error {
		var reservationProvider, reservationRoot, reservationSourceKey string
		var reservedBytes int64
		err := conn.QueryRowContext(ctx, `SELECT provider, configured_root_id,
			source_key, reserved_bytes
			FROM outbox_reservations WHERE id = ?`, reservationID,
		).Scan(&reservationProvider, &reservationRoot, &reservationSourceKey, &reservedBytes)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReservationMissing
		}
		if err != nil {
			return fmt.Errorf("rawcheckpoint: commit capture: read reservation: %w", err)
		}
		if !reservationMatchesSource(
			reservationProvider, reservationRoot, reservationSourceKey, validated.Source,
		) {
			return ErrCaptureConflict
		}

		latest, found, err := latestCaptureIDConn(ctx, conn, validated.Source)
		if err != nil {
			return err
		}
		if latest != validated.PredecessorCaptureID {
			return ErrCaptureConflict
		}
		if validated.ExpectedObservationRevision != nil {
			var observationRevision int64
			err := conn.QueryRowContext(ctx, `SELECT observation_revision
				FROM raw_sources
				WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
				string(validated.Source.Provider), validated.Source.ConfiguredRootID,
				validated.Source.SourceKey,
			).Scan(&observationRevision)
			if errors.Is(err, sql.ErrNoRows) ||
				err == nil && observationRevision != *validated.ExpectedObservationRevision {
				return ErrCaptureConflict
			}
			if err != nil {
				return fmt.Errorf("rawcheckpoint: commit capture: read source observation: %w", err)
			}
		}
		permanentRoot, err := permanentFailureAncestorConn(ctx, conn, latest)
		if err != nil {
			return err
		}
		newObjectBytes, err := missingObjectBytesConn(ctx, conn, uniqueObjects)
		if err != nil {
			return err
		}
		if metadataBytes > reservedBytes-newObjectBytes {
			return ErrReservationTooSmall
		}

		now := s.now().UTC().Format(time.RFC3339Nano)
		var headCaptureID string
		if found {
			if err := conn.QueryRowContext(ctx, `SELECT head_capture_id FROM raw_sources
					WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
				string(validated.Source.Provider), validated.Source.ConfiguredRootID,
				validated.Source.SourceKey).Scan(&headCaptureID); err != nil {
				return fmt.Errorf("rawcheckpoint: commit capture: read source head: %w", err)
			}
		}
		predecessorCaptureID := validated.PredecessorCaptureID
		if permanentRoot != "" {
			if err := discardRejectedGenerationConn(
				ctx, conn, permanentRoot, s,
			); err != nil {
				return err
			}
			predecessorCaptureID = headCaptureID
		}
		var predecessor any
		if predecessorCaptureID != "" && predecessorCaptureID != headCaptureID {
			predecessor = predecessorCaptureID
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox_generations
			(capture_id, provider, configured_root_id, source_key,
			 predecessor_capture_id, captured_at, kind, state, metadata_bytes,
			 created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?)`,
			validated.CaptureID, string(validated.Source.Provider),
			validated.Source.ConfiguredRootID, validated.Source.SourceKey,
			predecessor, checkpointTimestamp(validated.CapturedAt),
			string(validated.Kind), metadataBytes, now, now); err != nil {
			return fmt.Errorf("rawcheckpoint: commit capture: insert generation: %w", err)
		}
		if err := insertCapturedObjectsConn(ctx, conn, s, uniqueObjects, now); err != nil {
			return err
		}
		if err := insertCapturedEntriesConn(ctx, conn, validated); err != nil {
			return err
		}
		if found {
			if _, err := conn.ExecContext(ctx, `UPDATE raw_sources SET
				latest_capture_id = ?, observation_revision = observation_revision + 1,
				updated_at = ?
				WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
				validated.CaptureID, now, string(validated.Source.Provider),
				validated.Source.ConfiguredRootID, validated.Source.SourceKey); err != nil {
				return fmt.Errorf("rawcheckpoint: commit capture: update source: %w", err)
			}
		} else {
			if _, err := conn.ExecContext(ctx, `INSERT INTO raw_sources
				(provider, configured_root_id, source_key, updated_at, latest_capture_id,
				 observation_revision)
				VALUES (?, ?, ?, ?, ?, 1)`, string(validated.Source.Provider),
				validated.Source.ConfiguredRootID, validated.Source.SourceKey,
				now, validated.CaptureID); err != nil {
				return fmt.Errorf("rawcheckpoint: commit capture: insert source: %w", err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM outbox_reservations WHERE id = ?`, reservationID); err != nil {
			return fmt.Errorf("rawcheckpoint: commit capture: release reservation: %w", err)
		}
		if err := clearSourceCoverageFailureConn(
			ctx, conn, validated.Source, s.now().UTC(),
		); err != nil {
			return err
		}
		return nil
	})
}

// QueueTombstone durably appends one deletion after the newest local source
// generation. Repeated deletion observations are idempotent unless the newest
// tombstone was permanently rejected and must be replaced.
func (s *Store) QueueTombstone(
	ctx context.Context,
	source SourceIdentity,
) (string, bool, error) {
	return s.queueTombstone(ctx, source, "", 0, false)
}

// QueueTombstoneIfLatest queues a deletion only while expectedCaptureID is
// still the source's newest local generation. A concurrent capture makes the
// stale absence observation a no-op.
func (s *Store) QueueTombstoneIfLatest(
	ctx context.Context,
	source SourceIdentity,
	expectedCaptureID string,
	expectedObservationRevision int64,
) (string, bool, error) {
	return s.queueTombstone(
		ctx, source, expectedCaptureID, expectedObservationRevision, true,
	)
}

func (s *Store) queueTombstone(
	ctx context.Context,
	source SourceIdentity,
	expectedCaptureID string,
	expectedObservationRevision int64,
	fenced bool,
) (string, bool, error) {
	base, found, err := s.CaptureBase(ctx, source)
	if err != nil || !found {
		return "", false, err
	}
	if fenced && base.CaptureID != expectedCaptureID {
		return "", false, nil
	}
	if base.Kind == rawsync.ManifestTombstone && !base.PermanentlyRejected {
		return "", false, nil
	}
	reservation, err := s.ReserveSourceCapture(
		ctx, source, CaptureMetadataCharge(0, 0),
	)
	if err != nil {
		return "", false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = s.ReleaseReservation(context.Background(), reservation.ID)
		}
	}()
	captureID, err := randomCheckpointID()
	if err != nil {
		return "", false, err
	}
	var expectedRevision *int64
	if fenced {
		expectedRevision = &expectedObservationRevision
	}
	err = s.CommitCapture(ctx, reservation.ID, CapturedGeneration{
		CaptureID:                   captureID,
		Source:                      source,
		PredecessorCaptureID:        base.CaptureID,
		CapturedAt:                  s.now().UTC(),
		Kind:                        rawsync.ManifestTombstone,
		ExpectedObservationRevision: expectedRevision,
	})
	if fenced && errors.Is(err, ErrCaptureConflict) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	committed = true
	if base.PermanentlyRejected {
		if _, err := s.CollectGarbage(ctx); err != nil {
			return "", false, err
		}
	}
	return captureID, true, nil
}

func reservationMatchesSource(
	provider string,
	configuredRootID string,
	sourceKey string,
	source SourceIdentity,
) bool {
	return provider == string(source.Provider) &&
		configuredRootID == source.ConfiguredRootID &&
		(sourceKey == "" || sourceKey == source.SourceKey)
}

// CaptureBase returns the newest queued generation for append planning.
func (s *Store) CaptureBase(
	ctx context.Context,
	source SourceIdentity,
) (CaptureBaseState, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CaptureBaseState{}, false, fmt.Errorf(
			"rawcheckpoint: begin capture base snapshot: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	base, found, err := captureBaseSnapshot(ctx, tx, source)
	if err != nil {
		return CaptureBaseState{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return CaptureBaseState{}, false, fmt.Errorf(
			"rawcheckpoint: commit capture base snapshot: %w", err,
		)
	}
	return base, found, nil
}

// ConfiguredRootSources lists source chains already known beneath one local
// configured root. It contains identity only, never source content.
func (s *Store) ConfiguredRootSources(
	ctx context.Context,
	provider parser.AgentType,
	configuredRootID string,
) ([]SourceIdentity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_key FROM raw_sources
		WHERE provider = ? AND configured_root_id = ? ORDER BY source_key`,
		string(provider), configuredRootID)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: list configured-root sources: %w", err)
	}
	defer rows.Close()
	var sources []SourceIdentity
	for rows.Next() {
		var source SourceIdentity
		source.Provider = provider
		source.ConfiguredRootID = configuredRootID
		if err := rows.Scan(&source.SourceKey); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: list configured-root sources: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: list configured-root sources: %w", err)
	}
	return sources, nil
}

// ConfiguredRootSourcesPage returns at most limit source checkpoints after
// afterKey, wrapping to the beginning so repeated bounded calls rotate through
// a root.
func (s *Store) ConfiguredRootSourcesPage(
	ctx context.Context,
	provider parser.AgentType,
	configuredRootID string,
	afterKey string,
	limit int,
) ([]SourceCheckpoint, error) {
	if limit <= 0 {
		return nil, nil
	}
	sources, err := s.configuredRootSourcesRange(
		ctx, provider, configuredRootID, afterKey, ">", limit,
	)
	if err != nil || len(sources) == limit || afterKey == "" {
		return sources, err
	}
	wrapped, err := s.configuredRootSourcesRange(
		ctx, provider, configuredRootID, afterKey, "<=", limit-len(sources),
	)
	if err != nil {
		return nil, err
	}
	return append(sources, wrapped...), nil
}

func (s *Store) configuredRootSourcesRange(
	ctx context.Context,
	provider parser.AgentType,
	configuredRootID string,
	afterKey string,
	comparison string,
	limit int,
) ([]SourceCheckpoint, error) {
	query := `SELECT source_key, latest_capture_id, observation_revision FROM raw_sources
		WHERE provider = ? AND configured_root_id = ? AND source_key ` + comparison + ` ?
		ORDER BY source_key LIMIT ?`
	rows, err := s.db.QueryContext(
		ctx, query, string(provider), configuredRootID, afterKey, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: page configured-root sources: %w", err)
	}
	defer rows.Close()
	sources := make([]SourceCheckpoint, 0, limit)
	for rows.Next() {
		source := SourceCheckpoint{
			Source: SourceIdentity{
				Provider: provider, ConfiguredRootID: configuredRootID,
			},
		}
		if err := rows.Scan(
			&source.Source.SourceKey, &source.CaptureID, &source.ObservationRevision,
		); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: page configured-root sources: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: page configured-root sources: %w", err)
	}
	return sources, nil
}

type checkpointQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func captureBaseSnapshot(
	ctx context.Context,
	queryer checkpointQueryer,
	source SourceIdentity,
) (CaptureBaseState, bool, error) {
	var captureID string
	var headCaptureID string
	var head SourceHead
	var observationRevision int64
	var updatedAt string
	err := queryer.QueryRowContext(ctx, `SELECT latest_capture_id, head_capture_id, head_manifest_id,
		head_receipt, head_generation, observation_revision, updated_at FROM raw_sources
		WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
		string(source.Provider), source.ConfiguredRootID, source.SourceKey,
	).Scan(&captureID, &headCaptureID, &head.ManifestID, &head.Receipt,
		&head.Generation, &observationRevision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) || captureID == "" {
		return CaptureBaseState{}, false, nil
	}
	if err != nil {
		return CaptureBaseState{}, false, fmt.Errorf("rawcheckpoint: read capture base: %w", err)
	}
	head.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	if captureID != headCaptureID {
		generation, err := loadGeneration(ctx, queryer, captureID)
		if err != nil {
			return CaptureBaseState{}, false, err
		}
		permanentRoot, err := permanentFailureAncestor(ctx, queryer, captureID)
		if err != nil {
			return CaptureBaseState{}, false, err
		}
		return CaptureBaseState{
			CaptureID: captureID, ObservationRevision: observationRevision,
			Kind: generation.Kind,
			Head: head, Entries: generation.Entries,
			PermanentlyRejected: permanentRoot != "",
		}, true, nil
	}
	entries, err := loadAcknowledgedBase(ctx, queryer, source)
	if err != nil {
		return CaptureBaseState{}, false, err
	}
	kind := rawsync.ManifestSnapshot
	if len(entries) == 0 {
		kind = rawsync.ManifestTombstone
	}
	return CaptureBaseState{
		CaptureID: captureID, ObservationRevision: observationRevision,
		Kind: kind, Head: head, Entries: entries,
	}, true, nil
}

func permanentFailureAncestor(
	ctx context.Context,
	queryer checkpointQueryer,
	captureID string,
) (string, error) {
	var permanentCaptureID string
	err := queryer.QueryRowContext(ctx, `WITH RECURSIVE ancestors(
		capture_id, predecessor_capture_id, error_class, blocked
	) AS (
		SELECT capture_id, predecessor_capture_id, error_class, blocked
		FROM outbox_generations WHERE capture_id = ?
		UNION ALL
		SELECT generation.capture_id, generation.predecessor_capture_id,
			generation.error_class, generation.blocked
		FROM outbox_generations AS generation
		JOIN ancestors ON generation.capture_id = ancestors.predecessor_capture_id
	)
	SELECT capture_id FROM ancestors
	WHERE blocked = 1 AND error_class = ? LIMIT 1`,
		captureID, string(GenerationFailurePermanent),
	).Scan(&permanentCaptureID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("rawcheckpoint: read permanent failure ancestor: %w", err)
	}
	return permanentCaptureID, nil
}

func permanentFailureAncestorConn(
	ctx context.Context,
	conn *sql.Conn,
	captureID string,
) (string, error) {
	if captureID == "" {
		return "", nil
	}
	return permanentFailureAncestor(ctx, conn, captureID)
}

func loadAcknowledgedBase(
	ctx context.Context,
	queryer checkpointQueryer,
	source SourceIdentity,
) ([]CapturedEntry, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT entry_ordinal, path, length,
		mod_time_ns, file_identity, prefix_sha256, appendable
		FROM raw_source_base_entries
		WHERE provider = ? AND configured_root_id = ? AND source_key = ?
		ORDER BY entry_ordinal`, string(source.Provider), source.ConfiguredRootID, source.SourceKey)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: load acknowledged base: %w", err)
	}
	defer rows.Close()
	entries := make([]CapturedEntry, 0)
	ordinals := make([]int, 0)
	for rows.Next() {
		var entry CapturedEntry
		var ordinal int
		if err := rows.Scan(&ordinal, &entry.Path, &entry.Length, &entry.ModTimeNS,
			&entry.FileIdentity, &entry.PrefixSHA256, &entry.Appendable); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: load acknowledged base: %w", err)
		}
		entries = append(entries, entry)
		ordinals = append(ordinals, ordinal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: load acknowledged base: %w", err)
	}
	rows.Close()
	for i, ordinal := range ordinals {
		objectRows, err := queryer.QueryContext(ctx, `SELECT sha256, length
			FROM raw_source_base_objects
			WHERE provider = ? AND configured_root_id = ? AND source_key = ?
			AND entry_ordinal = ? ORDER BY object_ordinal`, string(source.Provider),
			source.ConfiguredRootID, source.SourceKey, ordinal)
		if err != nil {
			return nil, fmt.Errorf("rawcheckpoint: load acknowledged base objects: %w", err)
		}
		for objectRows.Next() {
			var ref rawsync.ObjectRef
			if err := objectRows.Scan(&ref.SHA256, &ref.Length); err != nil {
				objectRows.Close()
				return nil, fmt.Errorf("rawcheckpoint: load acknowledged base objects: %w", err)
			}
			entries[i].Objects = append(entries[i].Objects, ref)
		}
		if err := objectRows.Err(); err != nil {
			objectRows.Close()
			return nil, fmt.Errorf("rawcheckpoint: load acknowledged base objects: %w", err)
		}
		if err := objectRows.Close(); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: load acknowledged base objects: %w", err)
		}
	}
	return entries, nil
}

// NextGeneration returns the oldest queued generation whose predecessor is
// absent or acknowledged.
func (s *Store) NextGeneration(
	ctx context.Context,
) (CapturedGeneration, bool, error) {
	var captureID string
	err := s.db.QueryRowContext(ctx, `SELECT generation.capture_id
		FROM outbox_generations AS generation
		LEFT JOIN outbox_generations AS predecessor
			ON predecessor.capture_id = generation.predecessor_capture_id
		WHERE generation.state = 'queued'
		AND generation.blocked = 0
		AND (generation.retry_at = '' OR generation.retry_at <= ?)
		AND (generation.predecessor_capture_id IS NULL OR predecessor.state = 'acknowledged')
		ORDER BY generation.captured_at, generation.capture_id LIMIT 1`,
		checkpointTimestamp(s.now()),
	).Scan(&captureID)
	if errors.Is(err, sql.ErrNoRows) {
		return CapturedGeneration{}, false, nil
	}
	if err != nil {
		return CapturedGeneration{}, false, fmt.Errorf("rawcheckpoint: read next generation: %w", err)
	}
	generation, err := loadGeneration(ctx, s.db, captureID)
	return generation, err == nil, err
}

func loadGeneration(
	ctx context.Context,
	queryer checkpointQueryer,
	captureID string,
) (CapturedGeneration, error) {
	var generation CapturedGeneration
	var provider, capturedAt, kind string
	var predecessor sql.NullString
	err := queryer.QueryRowContext(ctx, `SELECT provider, configured_root_id,
		source_key, predecessor_capture_id, captured_at, kind
		FROM outbox_generations WHERE capture_id = ?`, captureID,
	).Scan(&provider, &generation.Source.ConfiguredRootID,
		&generation.Source.SourceKey, &predecessor, &capturedAt, &kind)
	if err != nil {
		return CapturedGeneration{}, fmt.Errorf("rawcheckpoint: load generation: %w", err)
	}
	generation.CaptureID = captureID
	generation.Source.Provider = parser.AgentType(provider)
	generation.PredecessorCaptureID = predecessor.String
	generation.CapturedAt, _ = time.Parse(time.RFC3339Nano, capturedAt)
	generation.Kind = rawsync.ManifestKind(kind)
	entries, err := loadGenerationEntries(ctx, queryer, captureID)
	if err != nil {
		return CapturedGeneration{}, err
	}
	generation.Entries = entries
	return generation, nil
}

func loadGenerationEntries(
	ctx context.Context,
	queryer checkpointQueryer,
	captureID string,
) ([]CapturedEntry, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT entry_ordinal, path, length,
		mod_time_ns, file_identity, prefix_sha256, appendable
		FROM outbox_entries WHERE capture_id = ? ORDER BY entry_ordinal`, captureID)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: load generation entries: %w", err)
	}
	defer rows.Close()
	entries := make([]CapturedEntry, 0)
	ordinals := make([]int, 0)
	for rows.Next() {
		var entry CapturedEntry
		var ordinal int
		if err := rows.Scan(&ordinal, &entry.Path, &entry.Length, &entry.ModTimeNS,
			&entry.FileIdentity, &entry.PrefixSHA256, &entry.Appendable); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: load generation entries: %w", err)
		}
		entries = append(entries, entry)
		ordinals = append(ordinals, ordinal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: load generation entries: %w", err)
	}
	for i, ordinal := range ordinals {
		objectRows, err := queryer.QueryContext(ctx, `SELECT sha256, length
			FROM outbox_entry_objects WHERE capture_id = ? AND entry_ordinal = ?
			ORDER BY object_ordinal`, captureID, ordinal)
		if err != nil {
			return nil, fmt.Errorf("rawcheckpoint: load generation objects: %w", err)
		}
		for objectRows.Next() {
			var ref rawsync.ObjectRef
			if err := objectRows.Scan(&ref.SHA256, &ref.Length); err != nil {
				objectRows.Close()
				return nil, fmt.Errorf("rawcheckpoint: load generation objects: %w", err)
			}
			entries[i].Objects = append(entries[i].Objects, ref)
		}
		if err := objectRows.Err(); err != nil {
			objectRows.Close()
			return nil, fmt.Errorf("rawcheckpoint: load generation objects: %w", err)
		}
		if err := objectRows.Close(); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: load generation objects: %w", err)
		}
	}
	return entries, nil
}

// CollectGarbage waits for active object publication, removes unreferenced
// spool objects, and only then releases their charged rows. Missing files are
// an idempotent success.
func (s *Store) CollectGarbage(ctx context.Context) (GarbageCollectionReport, error) {
	s.publicationMu.Lock()
	defer s.publicationMu.Unlock()
	s.objectMu.Lock()
	defer s.objectMu.Unlock()
	var report GarbageCollectionReport
	err := s.withImmediateWrite(ctx, "collect object garbage", func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT sha256, length FROM outbox_objects
			WHERE state = 'garbage_pending' AND ref_count = 0 ORDER BY sha256, length`)
		if err != nil {
			return fmt.Errorf("rawcheckpoint: list garbage: %w", err)
		}
		var refs []rawsync.ObjectRef
		for rows.Next() {
			var ref rawsync.ObjectRef
			if err := rows.Scan(&ref.SHA256, &ref.Length); err != nil {
				rows.Close()
				return fmt.Errorf("rawcheckpoint: list garbage: %w", err)
			}
			refs = append(refs, ref)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("rawcheckpoint: list garbage: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("rawcheckpoint: list garbage: %w", err)
		}
		for _, ref := range refs {
			if err := os.Remove(s.ObjectPath(ref)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("rawcheckpoint: remove garbage object: %s",
					checkpointFilesystemError(err))
			}
			var retained int
			err := conn.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM raw_source_base_objects
				WHERE sha256 = ? AND length = ?
			)`, ref.SHA256, ref.Length).Scan(&retained)
			if err != nil {
				return fmt.Errorf("rawcheckpoint: inspect acknowledged object base: %w", err)
			}
			var result sql.Result
			if retained != 0 {
				result, err = conn.ExecContext(ctx, `UPDATE outbox_objects SET state = 'remote'
					WHERE sha256 = ? AND length = ? AND state = 'garbage_pending'
					AND ref_count = 0`, ref.SHA256, ref.Length)
			} else {
				result, err = conn.ExecContext(ctx, `DELETE FROM outbox_objects
					WHERE sha256 = ? AND length = ? AND state = 'garbage_pending'
					AND ref_count = 0`, ref.SHA256, ref.Length)
			}
			if err != nil {
				return fmt.Errorf("rawcheckpoint: finish object garbage collection: %w", err)
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("rawcheckpoint: finish object garbage collection: %w", err)
			}
			if rows == 1 {
				report.Objects++
				report.Bytes += ref.Length
			}
		}
		return nil
	})
	return report, err
}

// DiscardUnreferencedObjects removes objects installed by a failed capture if
// no committed generation adopted them. It is safe to call repeatedly.
func (s *Store) DiscardUnreferencedObjects(
	ctx context.Context,
	refs []rawsync.ObjectRef,
) error {
	s.objectMu.Lock()
	defer s.objectMu.Unlock()
	return s.withImmediateWrite(ctx, "discard unreferenced capture objects", func(conn *sql.Conn) error {
		for _, ref := range refs {
			var state string
			err := conn.QueryRowContext(ctx, `SELECT state FROM outbox_objects
				WHERE sha256 = ? AND length = ?`, ref.SHA256, ref.Length).Scan(&state)
			switch {
			case err == nil && state != "remote":
				continue
			case !errors.Is(err, sql.ErrNoRows):
				if err != nil {
					return fmt.Errorf("rawcheckpoint: inspect failed capture object: %w", err)
				}
			}
			if err := os.Remove(s.ObjectPath(ref)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("rawcheckpoint: discard failed capture object: %s",
					checkpointFilesystemError(err))
			}
		}
		return nil
	})
}

func validateCapturedGeneration(
	store *Store,
	generation CapturedGeneration,
) (CapturedGeneration, int64, map[string]rawsync.ObjectRef, error) {
	if generation.CaptureID == "" || generation.Source.Provider == "" ||
		generation.Source.ConfiguredRootID == "" || generation.Source.SourceKey == "" ||
		generation.CapturedAt.IsZero() {
		return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: invalid captured generation")
	}
	switch generation.Kind {
	case rawsync.ManifestSnapshot:
		if len(generation.Entries) == 0 {
			return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: invalid captured generation")
		}
	case rawsync.ManifestTombstone:
		if len(generation.Entries) != 0 {
			return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: invalid captured generation")
		}
	default:
		return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: invalid captured generation")
	}
	validated := generation
	validated.CapturedAt = generation.CapturedAt.UTC()
	validated.Entries = make([]CapturedEntry, len(generation.Entries))
	uniqueObjects := make(map[string]rawsync.ObjectRef)
	byDigest := make(map[string]rawsync.ObjectRef)
	metadataBytes := generationMetadataBytes + int64(len(generation.Entries))*entryMetadataBytes
	seenPaths := make(map[string]struct{}, len(generation.Entries))
	for i, entry := range generation.Entries {
		if err := rawpath.Validate(entry.Path, rawpath.DefaultMaxBytes); err != nil {
			return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: invalid captured entry path: %w", err)
		}
		if _, ok := seenPaths[entry.Path]; ok || entry.Length < 0 || len(entry.Objects) == 0 {
			return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: invalid captured entry")
		}
		seenPaths[entry.Path] = struct{}{}
		if _, err := rawsync.NewObjectRef(entry.PrefixSHA256, 0); err != nil {
			return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: invalid captured prefix digest")
		}
		validated.Entries[i] = entry
		validated.Entries[i].Objects = append([]rawsync.ObjectRef(nil), entry.Objects...)
		var total int64
		for _, ref := range entry.Objects {
			canonical, err := rawsync.NewObjectRef(ref.SHA256, ref.Length)
			if err != nil || canonical != ref || ref.Length > entry.Length-total {
				return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: invalid captured object reference")
			}
			total += ref.Length
			key := ref.SHA256 + fmt.Sprintf(":%d", ref.Length)
			if prior, ok := byDigest[ref.SHA256]; ok && prior.Length != ref.Length {
				return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: conflicting object lengths")
			}
			byDigest[ref.SHA256] = ref
			uniqueObjects[key] = ref
			metadataBytes += objectReferenceMetadataBytes
		}
		if total != entry.Length {
			return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: captured object lengths do not match entry")
		}
	}
	for _, ref := range uniqueObjects {
		var state string
		dbErr := store.db.QueryRow(`SELECT state FROM outbox_objects
			WHERE sha256 = ? AND length = ?`, ref.SHA256, ref.Length).Scan(&state)
		info, err := os.Stat(store.ObjectPath(ref))
		if dbErr == nil && state == "remote" {
			switch {
			case errors.Is(err, os.ErrNotExist):
				continue
			case err != nil || !info.Mode().IsRegular() || info.Size() != ref.Length:
				return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: acknowledged object has conflicting local state")
			default:
				if err := os.Remove(store.ObjectPath(ref)); err != nil {
					return CapturedGeneration{}, 0, nil, fmt.Errorf(
						"rawcheckpoint: discard redundant acknowledged object: %s",
						checkpointFilesystemError(err),
					)
				}
				continue
			}
		}
		if err == nil && info.Mode().IsRegular() && info.Size() == ref.Length {
			continue
		}
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: captured object has conflicting local state")
		}
		return CapturedGeneration{}, 0, nil, fmt.Errorf("rawcheckpoint: captured object is not durably installed")
	}
	return validated, metadataBytes, uniqueObjects, nil
}

func latestCaptureIDConn(
	ctx context.Context,
	conn *sql.Conn,
	source SourceIdentity,
) (string, bool, error) {
	var latest string
	err := conn.QueryRowContext(ctx, `SELECT latest_capture_id FROM raw_sources
		WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
		string(source.Provider), source.ConfiguredRootID, source.SourceKey,
	).Scan(&latest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("rawcheckpoint: read latest capture: %w", err)
	}
	return latest, true, nil
}

func missingObjectBytesConn(
	ctx context.Context,
	conn *sql.Conn,
	objects map[string]rawsync.ObjectRef,
) (int64, error) {
	var bytes int64
	for _, ref := range objects {
		var length int64
		err := conn.QueryRowContext(ctx,
			`SELECT length FROM outbox_objects WHERE sha256 = ?`, ref.SHA256).Scan(&length)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			bytes += ref.Length
		case err != nil:
			return 0, fmt.Errorf("rawcheckpoint: inspect captured object: %w", err)
		case length != ref.Length:
			return 0, fmt.Errorf("rawcheckpoint: object digest has conflicting length")
		}
	}
	return bytes, nil
}

func insertCapturedObjectsConn(
	ctx context.Context,
	conn *sql.Conn,
	store *Store,
	objects map[string]rawsync.ObjectRef,
	now string,
) error {
	for _, ref := range objects {
		spoolName, err := filepath.Rel(store.spoolDir, store.ObjectPath(ref))
		if err != nil || strings.HasPrefix(spoolName, "..") {
			return fmt.Errorf("rawcheckpoint: derive object spool name")
		}
		state := "live"
		if info, statErr := os.Stat(store.ObjectPath(ref)); statErr != nil ||
			!info.Mode().IsRegular() || info.Size() != ref.Length {
			state = "remote"
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox_objects
			(sha256, length, spool_name, ref_count, state, created_at)
			VALUES (?, ?, ?, 0, ?, ?)
			ON CONFLICT(sha256, length) DO UPDATE SET
				spool_name = excluded.spool_name,
				state = CASE WHEN excluded.state = 'live' THEN 'live'
					ELSE outbox_objects.state END`,
			ref.SHA256, ref.Length, filepath.ToSlash(spoolName), state, now); err != nil {
			return fmt.Errorf("rawcheckpoint: insert captured object: %w", err)
		}
	}
	return nil
}

func insertCapturedEntriesConn(
	ctx context.Context,
	conn *sql.Conn,
	generation CapturedGeneration,
) error {
	for entryOrdinal, entry := range generation.Entries {
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox_entries
			(capture_id, entry_ordinal, path, length, mod_time_ns,
			 file_identity, prefix_sha256, appendable)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, generation.CaptureID, entryOrdinal,
			entry.Path, entry.Length, entry.ModTimeNS, entry.FileIdentity,
			entry.PrefixSHA256, entry.Appendable); err != nil {
			return fmt.Errorf("rawcheckpoint: insert captured entry: %w", err)
		}
		for objectOrdinal, ref := range entry.Objects {
			if _, err := conn.ExecContext(ctx, `INSERT INTO outbox_entry_objects
				(capture_id, entry_ordinal, object_ordinal, sha256, length)
				VALUES (?, ?, ?, ?, ?)`, generation.CaptureID, entryOrdinal,
				objectOrdinal, ref.SHA256, ref.Length); err != nil {
				return fmt.Errorf("rawcheckpoint: insert captured entry object: %w", err)
			}
			if _, err := conn.ExecContext(ctx, `UPDATE outbox_objects
				SET ref_count = ref_count + 1
				WHERE sha256 = ? AND length = ?`, ref.SHA256, ref.Length); err != nil {
				return fmt.Errorf("rawcheckpoint: reference captured object: %w", err)
			}
		}
	}
	return nil
}

func setCoverageConn(
	ctx context.Context,
	conn *sql.Conn,
	provider parser.AgentType,
	configuredRootID string,
	state CoverageStatus,
	reason string,
	now time.Time,
) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	if state == CoverageDegraded {
		_, err := conn.ExecContext(ctx, `INSERT INTO raw_coverage
			(provider, configured_root_id, state, reason, degraded_at, updated_at)
			VALUES (?, ?, 'degraded', ?, ?, ?)
			ON CONFLICT(provider, configured_root_id) DO UPDATE SET
			state = 'degraded', reason = excluded.reason,
			degraded_at = COALESCE(raw_coverage.degraded_at, excluded.degraded_at),
			recovered_at = NULL, updated_at = excluded.updated_at`,
			string(provider), configuredRootID, reason, stamp, stamp)
		if err != nil {
			return fmt.Errorf("rawcheckpoint: mark coverage degraded: %w", err)
		}
		return nil
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO raw_coverage
		(provider, configured_root_id, state, reason, recovered_at, updated_at)
		VALUES (?, ?, 'complete', '', ?, ?)
		ON CONFLICT(provider, configured_root_id) DO UPDATE SET
		state = 'complete', reason = '',
		recovered_at = CASE WHEN raw_coverage.state = 'degraded'
			THEN excluded.recovered_at ELSE raw_coverage.recovered_at END,
		updated_at = excluded.updated_at`, string(provider), configuredRootID, stamp, stamp)
	if err != nil {
		return fmt.Errorf("rawcheckpoint: mark coverage complete: %w", err)
	}
	return nil
}

func setSourceCoverageDegradedConn(
	ctx context.Context,
	conn *sql.Conn,
	source SourceIdentity,
	reason string,
	now time.Time,
) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	_, err := conn.ExecContext(ctx, `INSERT INTO raw_coverage_failures
		(provider, configured_root_id, source_key, reason, degraded_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, configured_root_id, source_key) DO UPDATE SET
			reason = excluded.reason, updated_at = excluded.updated_at`,
		string(source.Provider), source.ConfiguredRootID, source.SourceKey,
		reason, stamp, stamp)
	if err != nil {
		return fmt.Errorf("rawcheckpoint: record source coverage failure: %w", err)
	}
	return setCoverageConn(ctx, conn, source.Provider, source.ConfiguredRootID,
		CoverageDegraded, reason, now)
}

func clearSourceCoverageFailureConn(
	ctx context.Context,
	conn *sql.Conn,
	source SourceIdentity,
	now time.Time,
) error {
	if _, err := conn.ExecContext(ctx, `DELETE FROM raw_coverage_failures
		WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
		string(source.Provider), source.ConfiguredRootID, source.SourceKey); err != nil {
		return fmt.Errorf("rawcheckpoint: clear source coverage failure: %w", err)
	}
	return refreshCoverageFromFailuresConn(
		ctx, conn, source.Provider, source.ConfiguredRootID, now,
	)
}

func refreshCoverageFromFailuresConn(
	ctx context.Context,
	conn *sql.Conn,
	provider parser.AgentType,
	configuredRootID string,
	now time.Time,
) error {
	var reason string
	err := conn.QueryRowContext(ctx, `SELECT reason FROM raw_coverage_failures
		WHERE provider = ? AND configured_root_id = ?
		ORDER BY degraded_at, source_key LIMIT 1`,
		string(provider), configuredRootID).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return setCoverageConn(ctx, conn, provider, configuredRootID,
			CoverageComplete, "", now)
	}
	if err != nil {
		return fmt.Errorf("rawcheckpoint: inspect remaining coverage failures: %w", err)
	}
	return setCoverageConn(ctx, conn, provider, configuredRootID,
		CoverageDegraded, reason, now)
}

func parseOptionalTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil
	}
	return &parsed
}

func canonicalConfiguredRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("rawcheckpoint: resolve configured root: %s",
			checkpointFilesystemError(err))
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("rawcheckpoint: resolve configured root: %s",
			checkpointFilesystemError(err))
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("rawcheckpoint: stat configured root: %s",
			checkpointFilesystemError(err))
	}
	if !info.IsDir() {
		return "", fmt.Errorf("rawcheckpoint: configured root is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func checkpointFilesystemError(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "not found"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.Is(err, os.ErrInvalid):
		return "invalid path"
	default:
		return "filesystem error"
	}
}

func randomCheckpointID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("rawcheckpoint: generate identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
