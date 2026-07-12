package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/export"
)

const (
	archiveMetadataDatabaseIDKey              = "database_id"
	archiveMetadataArchiveIDKey               = "archive_id"
	archiveMetadataArchiveSaltKey             = "archive_salt"
	archiveMetadataProjectIdentityRevisionKey = "project_identity_publication_revision"
)

// ProjectIdentityPublicationRevision is an O(1) change token for the complete
// local identity publication. SQLite triggers advance it whenever an aggregate
// observation or immutable session snapshot changes.
func (db *DB) ProjectIdentityPublicationRevision(ctx context.Context) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var raw string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataProjectIdentityRevisionKey,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading project identity publication revision: %w", err)
	}
	revision, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || revision < 0 {
		return 0, fmt.Errorf("invalid project identity publication revision %q", raw)
	}
	return revision, nil
}

// ProjectIdentityObservationKey identifies one aggregate observation row in a
// downstream publication. SourceArchiveID is supplied by the publisher.
type ProjectIdentityObservationKey struct {
	Project   string
	Machine   string
	RootPath  string
	GitRemote string
}

// SessionProjectIdentitySnapshotKey identifies one immutable snapshot that a
// downstream publication must remove. Project is retained to apply filters.
type SessionProjectIdentitySnapshotKey struct {
	SessionID string
	Project   string
}

// ProjectIdentityPublicationDelta contains current rows and durable tombstones
// whose latest local change falls inside one publication revision window.
type ProjectIdentityPublicationDelta struct {
	Observations       []export.ProjectIdentityObservation
	ObservationDeletes []ProjectIdentityObservationKey
	Snapshots          []export.ProjectIdentityObservation
	SnapshotDeletes    []SessionProjectIdentitySnapshotKey
}

// LoadProjectIdentityPublicationDelta returns the compact identity changes in
// (afterRevision, throughRevision]. Project filters are applied to both current
// rows and tombstones so filtered targets can maintain independent cursors.
func (db *DB) LoadProjectIdentityPublicationDelta(
	ctx context.Context,
	afterRevision, throughRevision int64,
	projects, excludeProjects []string,
) (ProjectIdentityPublicationDelta, error) {
	var delta ProjectIdentityPublicationDelta
	if ctx == nil {
		ctx = context.Background()
	}
	if afterRevision < 0 || throughRevision < afterRevision {
		return delta, fmt.Errorf(
			"invalid project identity publication window (%d, %d]",
			afterRevision, throughRevision,
		)
	}
	if afterRevision == throughRevision {
		return delta, nil
	}

	where, args := projectIdentityPublicationChangeWhere(
		"c", afterRevision, throughRevision, projects, excludeProjects,
	)
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT o.source_archive_id, o.source_archive_salt,
			o.project, o.machine, o.root_path, o.git_remote, o.git_remote_name,
			o.repository_path, o.worktree_name, o.worktree_root_path,
			o.worktree_relationship, o.checkout_state, o.git_branch,
			o.remote_resolution, o.remote_candidate_count, o.observed_at,
			o.normalized_remote, o.key_source, o.key
		FROM project_identity_observation_changes c
		JOIN project_identity_observations o
		  ON o.project = c.project AND o.machine = c.machine
		 AND o.root_path = c.root_path AND o.git_remote = c.git_remote
		`+where+` AND c.deleted = 0
		ORDER BY c.project, c.machine, c.root_path, c.git_remote`, args...)
	if err != nil {
		return delta, fmt.Errorf("listing changed project identity observations: %w", err)
	}
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		var observedAt string
		if err := rows.Scan(
			&obs.SourceArchiveID, &obs.SourceArchiveSalt,
			&obs.Project, &obs.Machine, &obs.RootPath, &obs.GitRemote,
			&obs.GitRemoteName, &obs.RepositoryPath, &obs.WorktreeName,
			&obs.WorktreeRootPath, &obs.WorktreeRelationship, &obs.CheckoutState,
			&obs.GitBranch, &obs.RemoteResolution, &obs.RemoteCandidateCount,
			&observedAt, &obs.NormalizedRemote, &obs.KeySource, &obs.Key,
		); err != nil {
			rows.Close()
			return delta, fmt.Errorf("scanning changed project identity observation: %w", err)
		}
		obs.ObservedAt, err = time.Parse(time.RFC3339Nano, observedAt)
		if err != nil {
			rows.Close()
			return delta, fmt.Errorf("parsing changed project identity timestamp: %w", err)
		}
		delta.Observations = append(delta.Observations, obs)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return delta, fmt.Errorf("iterating changed project identity observations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return delta, fmt.Errorf("closing changed project identity observations: %w", err)
	}

	rows, err = db.getReader().QueryContext(ctx, `
		SELECT c.project, c.machine, c.root_path, c.git_remote
		FROM project_identity_observation_changes c
		`+where+` AND c.deleted = 1
		ORDER BY c.project, c.machine, c.root_path, c.git_remote`, args...)
	if err != nil {
		return delta, fmt.Errorf("listing project identity observation tombstones: %w", err)
	}
	for rows.Next() {
		var key ProjectIdentityObservationKey
		if err := rows.Scan(
			&key.Project, &key.Machine, &key.RootPath, &key.GitRemote,
		); err != nil {
			rows.Close()
			return delta, fmt.Errorf("scanning project identity observation tombstone: %w", err)
		}
		delta.ObservationDeletes = append(delta.ObservationDeletes, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return delta, fmt.Errorf("iterating project identity observation tombstones: %w", err)
	}
	if err := rows.Close(); err != nil {
		return delta, fmt.Errorf("closing project identity observation tombstones: %w", err)
	}

	rows, err = db.getReader().QueryContext(ctx, `
		SELECT s.session_id, s.project, s.machine, s.root_path, s.git_remote,
			s.git_remote_name, s.repository_path, s.worktree_name,
			s.worktree_root_path, s.worktree_relationship, s.checkout_state,
			s.git_branch, s.remote_resolution, s.remote_candidate_count,
			s.observed_at, s.normalized_remote, s.key_source, s.key
		FROM session_project_identity_snapshot_changes c
		JOIN session_project_identity_snapshots s
		  ON s.session_id = c.session_id AND s.project = c.project
		`+where+` AND c.deleted = 0
		ORDER BY c.session_id, c.project`, args...)
	if err != nil {
		return delta, fmt.Errorf("listing changed session project identity snapshots: %w", err)
	}
	for rows.Next() {
		var snapshot export.ProjectIdentityObservation
		var observedAt string
		if err := rows.Scan(
			&snapshot.SessionID, &snapshot.Project, &snapshot.Machine,
			&snapshot.RootPath, &snapshot.GitRemote, &snapshot.GitRemoteName,
			&snapshot.RepositoryPath, &snapshot.WorktreeName,
			&snapshot.WorktreeRootPath, &snapshot.WorktreeRelationship,
			&snapshot.CheckoutState, &snapshot.GitBranch,
			&snapshot.RemoteResolution, &snapshot.RemoteCandidateCount,
			&observedAt, &snapshot.NormalizedRemote,
			&snapshot.KeySource, &snapshot.Key,
		); err != nil {
			rows.Close()
			return delta, fmt.Errorf("scanning changed session project identity snapshot: %w", err)
		}
		snapshot.ObservedAt, err = time.Parse(time.RFC3339Nano, observedAt)
		if err != nil {
			rows.Close()
			return delta, fmt.Errorf("parsing changed session identity timestamp: %w", err)
		}
		delta.Snapshots = append(delta.Snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return delta, fmt.Errorf("iterating changed session project identity snapshots: %w", err)
	}
	if err := rows.Close(); err != nil {
		return delta, fmt.Errorf("closing changed session project identity snapshots: %w", err)
	}

	rows, err = db.getReader().QueryContext(ctx, `
		SELECT c.session_id, c.project
		FROM session_project_identity_snapshot_changes c
		`+where+` AND c.deleted = 1
		ORDER BY c.session_id, c.project`, args...)
	if err != nil {
		return delta, fmt.Errorf("listing session project identity snapshot tombstones: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key SessionProjectIdentitySnapshotKey
		if err := rows.Scan(&key.SessionID, &key.Project); err != nil {
			return delta, fmt.Errorf("scanning session project identity snapshot tombstone: %w", err)
		}
		delta.SnapshotDeletes = append(delta.SnapshotDeletes, key)
	}
	if err := rows.Err(); err != nil {
		return delta, fmt.Errorf("iterating session project identity snapshot tombstones: %w", err)
	}
	return delta, nil
}

func projectIdentityPublicationChangeWhere(
	alias string,
	afterRevision, throughRevision int64,
	projects, excludeProjects []string,
) (string, []any) {
	where := "WHERE " + alias + ".revision > ? AND " + alias + ".revision <= ?"
	args := []any{afterRevision, throughRevision}
	appendProjects := func(values []string, negate bool) {
		if len(values) == 0 {
			return
		}
		placeholders := make([]string, len(values))
		for i, value := range values {
			placeholders[i] = "?"
			args = append(args, value)
		}
		op := " IN "
		if negate {
			op = " NOT IN "
		}
		where += " AND " + alias + ".project" + op +
			"(" + strings.Join(placeholders, ",") + ")"
	}
	appendProjects(projects, false)
	appendProjects(excludeProjects, true)
	return where, args
}

var ErrDatabaseIDMissing = errors.New("database id is missing")
var ErrArchiveIDMissing = errors.New("archive id is missing")
var ErrArchiveSaltMissing = errors.New("archive salt is missing")
var ErrArchiveSaltInvalid = errors.New("archive salt is invalid")

func validateArchiveSalt(salt string) (string, error) {
	salt = strings.TrimSpace(salt)
	if salt == "" {
		return "", ErrArchiveSaltMissing
	}
	decoded, err := hex.DecodeString(salt)
	if err != nil || len(decoded) != 32 || salt != strings.ToLower(salt) {
		return "", fmt.Errorf("%w: expected 64 lowercase hexadecimal characters",
			ErrArchiveSaltInvalid)
	}
	return salt, nil
}

// CopyArchiveIdentityFrom preserves the logical archive identity in a fresh
// resync database before any session is parsed. The database ID is deliberately
// not copied because it identifies the new physical generation.
func (db *DB) CopyArchiveIdentityFrom(sourcePath string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	ctx := context.Background()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring archive identity connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS identity_source", sourcePath); err != nil {
		return fmt.Errorf("attaching archive identity source: %w", err)
	}
	defer func() {
		_, _ = execWithoutCancel(ctx, conn, "DETACH DATABASE identity_source")
	}()

	type metadataRow struct {
		value     string
		createdAt string
		updatedAt string
	}
	metadata := make(map[string]metadataRow, 2)
	rows, err := conn.QueryContext(ctx, `
		SELECT key, value, created_at, updated_at
		FROM identity_source.archive_metadata
		WHERE key IN (?, ?)`,
		archiveMetadataArchiveIDKey, archiveMetadataArchiveSaltKey,
	)
	if err != nil {
		return fmt.Errorf("reading archive identity source: %w", err)
	}
	for rows.Next() {
		var key string
		var row metadataRow
		if err := rows.Scan(&key, &row.value, &row.createdAt, &row.updatedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scanning archive identity source: %w", err)
		}
		metadata[key] = row
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterating archive identity source: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing archive identity source rows: %w", err)
	}
	if strings.TrimSpace(metadata[archiveMetadataArchiveIDKey].value) == "" {
		return ErrArchiveIDMissing
	}
	if _, err := validateArchiveSalt(
		metadata[archiveMetadataArchiveSaltKey].value,
	); err != nil {
		return err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning archive identity copy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, key := range []string{
		archiveMetadataArchiveIDKey,
		archiveMetadataArchiveSaltKey,
	} {
		row := metadata[key]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO archive_metadata (key, value, created_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				value = excluded.value,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`,
			key, row.value, row.createdAt, row.updatedAt,
		); err != nil {
			return fmt.Errorf("copying archive identity %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing archive identity copy: %w", err)
	}

	for _, key := range []string{
		archiveMetadataArchiveIDKey,
		archiveMetadataArchiveSaltKey,
	} {
		var persisted string
		if err := conn.QueryRowContext(ctx,
			`SELECT value FROM archive_metadata WHERE key = ?`, key,
		).Scan(&persisted); err != nil {
			return fmt.Errorf("verifying archive identity %s: %w", key, err)
		}
		if persisted != metadata[key].value {
			return fmt.Errorf("verifying archive identity %s: value mismatch", key)
		}
	}
	return nil
}

func (db *DB) GetDatabaseID(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var id string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrDatabaseIDMissing
		}
		return "", fmt.Errorf("reading database id: %w", err)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", ErrDatabaseIDMissing
	}
	return id, nil
}

func (db *DB) GetOrCreateDatabaseID(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id, err := db.GetDatabaseID(ctx)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, ErrDatabaseIDMissing) {
		return "", err
	}
	if err := db.requireWritable(); err != nil {
		return "", ErrDatabaseIDMissing
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	err = db.getWriter().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	).Scan(&id)
	if err == nil && strings.TrimSpace(id) != "" {
		return id, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("reading database id: %w", err)
	}
	id, err = newUUIDv4()
	if err != nil {
		return "", err
	}
	if _, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE trim(archive_metadata.value) = ''`,
		archiveMetadataDatabaseIDKey, id,
	); err != nil {
		return "", fmt.Errorf("creating database id: %w", err)
	}
	var persisted string
	if err := db.getWriter().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	).Scan(&persisted); err != nil {
		return "", fmt.Errorf("rereading database id: %w", err)
	}
	return persisted, nil
}

func (db *DB) GetArchiveID(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var id string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataArchiveIDKey,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrArchiveIDMissing
		}
		return "", fmt.Errorf("reading archive id: %w", err)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", ErrArchiveIDMissing
	}
	return id, nil
}

func (db *DB) GetOrCreateArchiveID(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id, err := db.GetArchiveID(ctx)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, ErrArchiveIDMissing) {
		return "", err
	}
	if err := db.requireWritable(); err != nil {
		return "", ErrArchiveIDMissing
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	err = db.getWriter().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataArchiveIDKey,
	).Scan(&id)
	if err == nil && strings.TrimSpace(id) != "" {
		return id, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("reading archive id: %w", err)
	}
	id, err = newUUIDv4()
	if err != nil {
		return "", err
	}
	if _, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE trim(archive_metadata.value) = ''`,
		archiveMetadataArchiveIDKey, id,
	); err != nil {
		return "", fmt.Errorf("creating archive id: %w", err)
	}
	var persisted string
	if err := db.getWriter().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataArchiveIDKey,
	).Scan(&persisted); err != nil {
		return "", fmt.Errorf("rereading archive id: %w", err)
	}
	return persisted, nil
}

func (db *DB) GetArchiveSalt(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var salt string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataArchiveSaltKey,
	).Scan(&salt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrArchiveSaltMissing
		}
		return "", fmt.Errorf("reading archive salt: %w", err)
	}
	return validateArchiveSalt(salt)
}

func (db *DB) GetOrCreateArchiveSalt(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	salt, err := db.GetArchiveSalt(ctx)
	if err == nil {
		return salt, nil
	}
	if !errors.Is(err, ErrArchiveSaltMissing) {
		return "", err
	}
	if err := db.requireWritable(); err != nil {
		return "", ErrArchiveSaltMissing
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	err = db.getWriter().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataArchiveSaltKey,
	).Scan(&salt)
	if err == nil && strings.TrimSpace(salt) != "" {
		return validateArchiveSalt(salt)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("reading archive salt: %w", err)
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generating archive salt: %w", err)
	}
	salt = hex.EncodeToString(random)
	if _, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE trim(archive_metadata.value) = ''`,
		archiveMetadataArchiveSaltKey, salt,
	); err != nil {
		return "", fmt.Errorf("creating archive salt: %w", err)
	}
	return salt, nil
}

func (db *DB) SetDatabaseIDForTest(ctx context.Context, id string) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("database id is required")
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		archiveMetadataDatabaseIDKey, id,
	)
	if err != nil {
		return fmt.Errorf("setting database id: %w", err)
	}
	return nil
}

func (db *DB) SetArchiveIdentityForTest(ctx context.Context, id, salt string) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	salt = strings.TrimSpace(salt)
	if id == "" || salt == "" {
		return fmt.Errorf("archive id and salt are required")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for key, value := range map[string]string{
		archiveMetadataArchiveIDKey: id, archiveMetadataArchiveSaltKey: salt,
	} {
		if _, err := db.getWriter().ExecContext(ctx, `
			INSERT INTO archive_metadata (key, value)
			VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value,
				updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
			key, value,
		); err != nil {
			return fmt.Errorf("setting archive identity: %w", err)
		}
	}
	return nil
}

func (db *DB) UpsertProjectIdentityObservation(
	ctx context.Context,
	obs export.ProjectIdentityObservation,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	obs, err := normalizeProjectIdentityObservation(obs)
	if err != nil {
		return err
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning project identity observation upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertProjectIdentityObservationTx(tx, obs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing project identity observation upsert: %w", err)
	}
	return nil
}

func upsertProjectIdentityObservationTx(
	tx *sql.Tx,
	obs export.ProjectIdentityObservation,
) error {
	normalized, err := normalizeProjectIdentityObservation(obs)
	if err != nil {
		return err
	}
	if err := upsertProjectIdentityObservationExec(
		context.Background(), tx,
		func(ctx context.Context, query string, args ...any) rowScanner {
			return tx.QueryRowContext(ctx, query, args...)
		},
		normalized,
	); err != nil {
		return err
	}
	if err := upsertSessionProjectIdentitySnapshotExec(
		context.Background(), tx,
		func(ctx context.Context, query string, args ...any) rowScanner {
			return tx.QueryRowContext(ctx, query, args...)
		},
		normalized,
	); err != nil {
		return err
	}
	return nil
}

func upsertSessionProjectIdentitySnapshotExec(
	ctx context.Context,
	exec contextExecer,
	queryRow contextQueryRow,
	obs export.ProjectIdentityObservation,
) error {
	if obs.SessionID == "" {
		return nil
	}
	var sessionExists int
	if err := queryRow(ctx, `SELECT 1 FROM sessions WHERE id = ?`,
		obs.SessionID).Scan(&sessionExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("checking session for project identity snapshot: %w", err)
	}
	var existing export.ProjectResolution
	var existingKey string
	err := queryRow(ctx, `
		SELECT remote_resolution, key
		FROM session_project_identity_snapshots
		WHERE session_id = ?`, obs.SessionID).Scan(&existing, &existingKey)
	if err == nil {
		if existing == export.ProjectResolutionResolved ||
			existing == export.ProjectResolutionAmbiguous ||
			(obs.RemoteResolution == export.ProjectResolutionUnknown &&
				(obs.Key == "" || strings.TrimSpace(existingKey) != "")) {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking session project identity snapshot: %w", err)
	}

	_, err = exec.ExecContext(ctx, `
		INSERT INTO session_project_identity_snapshots (
			session_id, project, machine, root_path, git_remote,
			git_remote_name, repository_path, worktree_name,
			worktree_root_path, worktree_relationship, checkout_state,
			git_branch, remote_resolution, remote_candidate_count,
			observed_at, normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			project = excluded.project,
			machine = excluded.machine,
			root_path = excluded.root_path,
			git_remote = excluded.git_remote,
			git_remote_name = excluded.git_remote_name,
			repository_path = excluded.repository_path,
			worktree_name = excluded.worktree_name,
			worktree_root_path = excluded.worktree_root_path,
			worktree_relationship = excluded.worktree_relationship,
			checkout_state = excluded.checkout_state,
			git_branch = excluded.git_branch,
			remote_resolution = excluded.remote_resolution,
			remote_candidate_count = excluded.remote_candidate_count,
			observed_at = excluded.observed_at,
			normalized_remote = excluded.normalized_remote,
			key_source = excluded.key_source,
			key = excluded.key`,
		obs.SessionID, obs.Project, obs.Machine, obs.RootPath,
		obs.GitRemote, obs.GitRemoteName, obs.RepositoryPath,
		obs.WorktreeName, obs.WorktreeRootPath, obs.WorktreeRelationship,
		obs.CheckoutState, obs.GitBranch, obs.RemoteResolution,
		obs.RemoteCandidateCount, obs.ObservedAt.UTC().Format(time.RFC3339Nano),
		obs.NormalizedRemote, obs.KeySource, obs.Key,
	)
	if err != nil {
		return fmt.Errorf("upserting session project identity snapshot: %w", err)
	}
	return nil
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type contextQueryRow func(context.Context, string, ...any) rowScanner

func upsertProjectIdentityObservationExec(
	ctx context.Context,
	exec contextExecer,
	queryRow contextQueryRow,
	obs export.ProjectIdentityObservation,
) error {
	return upsertProjectIdentityObservationExecExcludingRemote(
		ctx, exec, queryRow, obs, "")
}

func upsertProjectIdentityObservationExecExcludingRemote(
	ctx context.Context,
	exec contextExecer,
	queryRow contextQueryRow,
	obs export.ProjectIdentityObservation,
	excludeRemote string,
) error {
	if obs.GitRemote == "" && obs.RemoteResolution != export.ProjectResolutionAmbiguous {
		var exists int
		query := `
			SELECT 1 FROM project_identity_observations
			WHERE project = ? AND machine = ? AND root_path = ?
			  AND (git_remote != '' OR remote_resolution = ?)`
		args := []any{
			obs.Project, obs.Machine, obs.RootPath,
			export.ProjectResolutionAmbiguous,
		}
		if excludeRemote != "" {
			query += ` AND git_remote != ?`
			args = append(args, excludeRemote)
		}
		query += ` LIMIT 1`
		err := queryRow(ctx, `
			`+strings.TrimSpace(query),
			args...,
		).Scan(&exists)
		if err == nil && exists == 1 {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking project identity remote observation: %w", err)
		}
	} else if _, err := exec.ExecContext(ctx, `
		DELETE FROM project_identity_observations
		WHERE project = ? AND machine = ? AND root_path = ?
		  AND git_remote = '' AND remote_resolution != ?`,
		obs.Project, obs.Machine, obs.RootPath,
		export.ProjectResolutionAmbiguous,
	); err != nil {
		return fmt.Errorf("removing stale project identity root fallback: %w", err)
	}

	_, err := exec.ExecContext(ctx, `
		INSERT INTO project_identity_observations (
			source_archive_id, source_archive_salt,
			project, machine, root_path, git_remote, git_remote_name,
			repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
			source_archive_id = excluded.source_archive_id,
			source_archive_salt = excluded.source_archive_salt,
			git_remote_name = excluded.git_remote_name,
			repository_path = excluded.repository_path,
			worktree_name = excluded.worktree_name,
			worktree_root_path = excluded.worktree_root_path,
			worktree_relationship = excluded.worktree_relationship,
			checkout_state = excluded.checkout_state,
			git_branch = excluded.git_branch,
			remote_resolution = excluded.remote_resolution,
			remote_candidate_count = excluded.remote_candidate_count,
			observed_at = excluded.observed_at,
			normalized_remote = excluded.normalized_remote,
			key_source = excluded.key_source,
			key = excluded.key`,
		obs.SourceArchiveID, obs.SourceArchiveSalt,
		obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
		obs.GitRemoteName, obs.RepositoryPath, obs.WorktreeName,
		obs.WorktreeRootPath, obs.WorktreeRelationship, obs.CheckoutState,
		obs.GitBranch, obs.RemoteResolution, obs.RemoteCandidateCount,
		obs.ObservedAt.UTC().Format(time.RFC3339Nano),
		obs.NormalizedRemote, obs.KeySource, obs.Key,
	)
	if err != nil {
		return fmt.Errorf("upserting project identity observation: %w", err)
	}
	return nil
}

func normalizeProjectIdentityObservation(
	obs export.ProjectIdentityObservation,
) (export.ProjectIdentityObservation, error) {
	obs.Project = strings.TrimSpace(obs.Project)
	obs.SessionID = strings.TrimSpace(obs.SessionID)
	obs.SourceArchiveID = strings.TrimSpace(obs.SourceArchiveID)
	obs.SourceArchiveSalt = strings.TrimSpace(obs.SourceArchiveSalt)
	obs.Machine = strings.TrimSpace(obs.Machine)
	obs.RootPath = strings.TrimSpace(obs.RootPath)
	obs.GitRemote = export.SanitizeGitRemoteForStorage(obs.GitRemote)
	obs.GitRemoteName = strings.TrimSpace(obs.GitRemoteName)
	obs.RepositoryPath = strings.TrimSpace(obs.RepositoryPath)
	obs.WorktreeName = strings.TrimSpace(obs.WorktreeName)
	obs.WorktreeRootPath = strings.TrimSpace(obs.WorktreeRootPath)
	obs.GitBranch = strings.TrimSpace(obs.GitBranch)
	if obs.Project == "" {
		return obs, fmt.Errorf("project is required")
	}
	if obs.Machine == "" {
		return obs, fmt.Errorf("machine is required")
	}
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = time.Now().UTC()
	}
	if obs.WorktreeRelationship == "" {
		obs.WorktreeRelationship = export.WorktreeUnknown
	}
	if obs.CheckoutState == "" {
		obs.CheckoutState = export.CheckoutUnknown
	}
	if obs.RemoteResolution == "" {
		if obs.GitRemote != "" {
			obs.RemoteResolution = export.ProjectResolutionResolved
		} else if obs.RemoteCandidateCount > 1 {
			obs.RemoteResolution = export.ProjectResolutionAmbiguous
		} else {
			obs.RemoteResolution = export.ProjectResolutionUnknown
		}
	}
	if obs.RemoteResolution == export.ProjectResolutionAmbiguous {
		obs.GitRemote = ""
		obs.GitRemoteName = ""
		obs.NormalizedRemote = ""
		obs.KeySource = ""
		obs.Key = ""
		return obs, nil
	}
	identity := export.BuildStoredProjectIdentity(
		export.ProjectIdentityInput{
			RootPath:         obs.RootPath,
			GitRemote:        obs.GitRemote,
			GitRemoteName:    obs.GitRemoteName,
			WorktreeName:     obs.WorktreeName,
			WorktreeRootPath: obs.WorktreeRootPath,
		},
	)
	obs.NormalizedRemote = identity.NormalizedRemote
	obs.KeySource = identity.KeySource
	obs.Key = identity.Key
	return obs, nil
}

func scrubProjectIdentityGitRemoteCredentialsTx(
	ctx context.Context, tx *sql.Tx,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT source_archive_id, source_archive_salt,
			project, machine, root_path, git_remote, git_remote_name,
			repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at,
			normalized_remote, key_source, key
		FROM project_identity_observations
		WHERE git_remote != ''`)
	if err != nil {
		return fmt.Errorf("listing project identity remotes for scrub: %w", err)
	}

	type pendingScrub struct {
		obs       export.ProjectIdentityObservation
		rawRemote string
	}
	var pending []pendingScrub
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		var observedAt string
		if err := rows.Scan(
			&obs.SourceArchiveID,
			&obs.SourceArchiveSalt,
			&obs.Project,
			&obs.Machine,
			&obs.RootPath,
			&obs.GitRemote,
			&obs.GitRemoteName,
			&obs.RepositoryPath,
			&obs.WorktreeName,
			&obs.WorktreeRootPath,
			&obs.WorktreeRelationship,
			&obs.CheckoutState,
			&obs.GitBranch,
			&obs.RemoteResolution,
			&obs.RemoteCandidateCount,
			&observedAt,
			&obs.NormalizedRemote,
			&obs.KeySource,
			&obs.Key,
		); err != nil {
			return fmt.Errorf("scanning project identity remote for scrub: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, observedAt); err == nil {
			obs.ObservedAt = t
		}
		sanitized := export.SanitizeGitRemoteForStorage(obs.GitRemote)
		if sanitized == obs.GitRemote {
			continue
		}
		pending = append(pending, pendingScrub{obs: obs, rawRemote: obs.GitRemote})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating project identity remotes for scrub: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing project identity remotes scrub rows: %w", err)
	}

	for _, scrub := range pending {
		obs := scrub.obs
		obs = export.SanitizeStoredProjectIdentityObservation(obs)
		normalized, err := normalizeProjectIdentityObservation(obs)
		if err != nil {
			return fmt.Errorf("normalizing project identity remote scrub: %w", err)
		}
		if err := upsertProjectIdentityObservationExecExcludingRemote(
			ctx, tx,
			func(ctx context.Context, query string, args ...any) rowScanner {
				return tx.QueryRowContext(ctx, query, args...)
			},
			normalized, scrub.rawRemote,
		); err != nil {
			return fmt.Errorf("scrubbing project identity remote: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM project_identity_observations
			WHERE project = ? AND machine = ? AND root_path = ?
			  AND git_remote = ?`,
			scrub.obs.Project, scrub.obs.Machine, scrub.obs.RootPath,
			scrub.rawRemote,
		); err != nil {
			return fmt.Errorf("removing raw project identity remote: %w", err)
		}
	}
	return nil
}

func (db *DB) ListProjectIdentityObservations(
	ctx context.Context,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	return db.listProjectIdentityObservationsFrom(ctx, db.getReader(), labels)
}

func (db *DB) listProjectIdentityObservationsFrom(
	ctx context.Context,
	q sessionExportQuerier,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if labels != nil && len(labels) == 0 {
		return []export.ProjectIdentityObservation{}, nil
	}
	query := `SELECT source_archive_id, source_archive_salt,
		project, machine, root_path, git_remote, git_remote_name,
		repository_path, worktree_name, worktree_root_path,
		worktree_relationship, checkout_state, git_branch,
		remote_resolution, remote_candidate_count, observed_at,
		normalized_remote, key_source, key
		FROM project_identity_observations`
	args := make([]any, 0, len(labels))
	if len(labels) > 0 {
		placeholders := make([]string, 0, len(labels))
		for _, label := range labels {
			placeholders = append(placeholders, "?")
			args = append(args, label)
		}
		query += " WHERE project IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY project, machine, root_path, git_remote"

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing project identity observations: %w", err)
	}
	defer rows.Close()

	var out []export.ProjectIdentityObservation
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		var observedAt string
		if err := rows.Scan(
			&obs.SourceArchiveID,
			&obs.SourceArchiveSalt,
			&obs.Project,
			&obs.Machine,
			&obs.RootPath,
			&obs.GitRemote,
			&obs.GitRemoteName,
			&obs.RepositoryPath,
			&obs.WorktreeName,
			&obs.WorktreeRootPath,
			&obs.WorktreeRelationship,
			&obs.CheckoutState,
			&obs.GitBranch,
			&obs.RemoteResolution,
			&obs.RemoteCandidateCount,
			&observedAt,
			&obs.NormalizedRemote,
			&obs.KeySource,
			&obs.Key,
		); err != nil {
			return nil, fmt.Errorf("scanning project identity observation: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, observedAt); err == nil {
			obs.ObservedAt = t
		}
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating project identity observations: %w", err)
	}
	return out, nil
}

func (db *DB) listSessionProjectIdentitySnapshots(
	ctx context.Context,
	sessionIDs []string,
) (map[string]export.ProjectIdentityObservation, error) {
	return db.listSessionProjectIdentitySnapshotsFrom(
		ctx, db.getReader(), sessionIDs)
}

func (db *DB) listSessionProjectIdentitySnapshotsFrom(
	ctx context.Context,
	q sessionExportQuerier,
	sessionIDs []string,
) (map[string]export.ProjectIdentityObservation, error) {
	out := make(map[string]export.ProjectIdentityObservation, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, `
		SELECT session_id, project, machine, root_path, git_remote,
			git_remote_name, repository_path, worktree_name,
			worktree_root_path, worktree_relationship, checkout_state,
			git_branch, remote_resolution, remote_candidate_count,
			observed_at, normalized_remote, key_source, key
		FROM session_project_identity_snapshots
		WHERE session_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing session project identity snapshots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		var observedAt string
		if err := rows.Scan(
			&obs.SessionID, &obs.Project, &obs.Machine, &obs.RootPath,
			&obs.GitRemote, &obs.GitRemoteName, &obs.RepositoryPath,
			&obs.WorktreeName, &obs.WorktreeRootPath,
			&obs.WorktreeRelationship, &obs.CheckoutState, &obs.GitBranch,
			&obs.RemoteResolution, &obs.RemoteCandidateCount, &observedAt,
			&obs.NormalizedRemote, &obs.KeySource, &obs.Key,
		); err != nil {
			return nil, fmt.Errorf("scanning session project identity snapshot: %w", err)
		}
		obs.ObservedAt, err = time.Parse(time.RFC3339Nano, observedAt)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing session project identity snapshot timestamp: %w", err,
			)
		}
		out[obs.SessionID] = obs
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session project identity snapshots: %w", err)
	}
	return out, nil
}

// ListSessionProjectIdentitySnapshots returns the immutable per-session
// identity facts used by mirror push. Aggregate observations are deliberately
// not substituted because they lose historical worktree and remote context.
func (db *DB) ListSessionProjectIdentitySnapshots(
	ctx context.Context,
) ([]export.ProjectIdentityObservation, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT session_id, project, machine, root_path, git_remote,
			git_remote_name, repository_path, worktree_name,
			worktree_root_path, worktree_relationship, checkout_state,
			git_branch, remote_resolution, remote_candidate_count,
			observed_at, normalized_remote, key_source, key
		FROM session_project_identity_snapshots
		ORDER BY session_id`)
	if err != nil {
		return nil, fmt.Errorf("listing all session project identity snapshots: %w", err)
	}
	defer rows.Close()
	var out []export.ProjectIdentityObservation
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		var observedAt string
		if err := rows.Scan(
			&obs.SessionID, &obs.Project, &obs.Machine, &obs.RootPath,
			&obs.GitRemote, &obs.GitRemoteName, &obs.RepositoryPath,
			&obs.WorktreeName, &obs.WorktreeRootPath,
			&obs.WorktreeRelationship, &obs.CheckoutState, &obs.GitBranch,
			&obs.RemoteResolution, &obs.RemoteCandidateCount, &observedAt,
			&obs.NormalizedRemote, &obs.KeySource, &obs.Key,
		); err != nil {
			return nil, fmt.Errorf("scanning session project identity snapshot: %w", err)
		}
		obs.ObservedAt, err = time.Parse(time.RFC3339Nano, observedAt)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing session project identity snapshot timestamp: %w", err,
			)
		}
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session project identity snapshots: %w", err)
	}
	return out, nil
}

func (db *DB) BuildProjectIdentityMap(
	ctx context.Context,
	labels []string,
) (map[string]export.ProjectMapEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if labels != nil && len(labels) == 0 {
		return map[string]export.ProjectMapEntry{}, nil
	}
	observations, err := db.ListProjectIdentityObservations(ctx, labels)
	if err != nil {
		return nil, err
	}
	archiveID, err := db.GetArchiveID(ctx)
	if err != nil {
		return nil, err
	}
	archiveSalt, err := db.GetArchiveSalt(ctx)
	if err != nil {
		return nil, err
	}
	return export.BuildProjectsMapWithScope(labels, observations, export.IdentityScope{
		ArchiveID: archiveID, ArchiveSalt: archiveSalt,
	}), nil
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating database id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b[:])
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		encoded[0:8], encoded[8:12], encoded[12:16],
		encoded[16:20], encoded[20:32],
	), nil
}
