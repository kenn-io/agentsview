package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
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
// downstream publication must remove.
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
// (afterRevision, throughRevision]. Project filters apply to current rows and
// aggregate-observation tombstones. Snapshot tombstones remain unfiltered
// because current rows are scoped by the owning session project while their
// deletion journal retains the immutable snapshot project. Destinations decide
// whether a tombstone applies from their resident rows or publication owners.
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
		"c", "c.project", afterRevision, throughRevision,
		projects, excludeProjects,
	)
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT o.source_archive_id, o.source_archive_salt,
			o.project, o.machine, o.root_path, o.git_remote, o.git_remote_name,
			o.repository_path, o.worktree_name, o.worktree_root_path,
			o.worktree_relationship, o.checkout_state, o.git_branch,
			o.remote_resolution, o.remote_candidate_count, o.observed_at,
			o.normalized_remote, o.key_source, o.key
		FROM project_identity_observation_changes c
		JOIN source_project_identity_observations o
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

	snapshotWhere, snapshotArgs := projectIdentityPublicationChangeWhere(
		"c", "owner.project", afterRevision, throughRevision,
		projects, excludeProjects,
	)
	rows, err = db.getReader().QueryContext(ctx, `
		SELECT s.source_session_id, s.project, s.machine, s.root_path, s.git_remote,
			s.git_remote_name, s.repository_path, s.worktree_name,
			s.worktree_root_path, s.worktree_relationship, s.checkout_state,
			s.git_branch, s.remote_resolution, s.remote_candidate_count,
			s.observed_at, s.normalized_remote, s.key_source, s.key
		FROM session_project_identity_snapshot_changes c
		JOIN source_session_project_identity_snapshots s
		  ON s.source_session_id = c.session_id AND s.project = c.project
		JOIN sessions owner
		  ON owner.id = s.source_session_id
		 AND owner.source_archive_id = s.source_archive_id
		 AND owner.source_database_generation = s.source_database_generation
		 AND owner.deleted_at IS NULL
		`+snapshotWhere+` AND c.deleted = 0
		  AND (TRIM(s.key_source) != '' OR TRIM(s.worktree_root_path) != '')
		ORDER BY c.session_id, c.project`, snapshotArgs...)
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

	tombstoneWhere, tombstoneArgs := projectIdentityPublicationChangeWhere(
		"c", "c.project", afterRevision, throughRevision,
		nil, nil,
	)
	rows, err = db.getReader().QueryContext(ctx, `
		SELECT c.session_id, c.project
		FROM session_project_identity_snapshot_changes c
		`+tombstoneWhere+` AND c.deleted = 1
		ORDER BY c.session_id, c.project`, tombstoneArgs...)
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
	revisionAlias string,
	projectExpression string,
	afterRevision, throughRevision int64,
	projects, excludeProjects []string,
) (string, []any) {
	where := "WHERE " + revisionAlias + ".revision > ? AND " +
		revisionAlias + ".revision <= ?"
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
		where += " AND " + projectExpression + op +
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
	var previousArchiveID string
	if err := tx.QueryRowContext(ctx, `
		SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataArchiveIDKey,
	).Scan(&previousArchiveID); err != nil {
		return fmt.Errorf("reading prior archive identity: %w", err)
	}
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source_archives (source_archive_id, source_archive_salt)
		VALUES (?, ?)
		ON CONFLICT(source_archive_id) DO UPDATE SET
			source_archive_salt = excluded.source_archive_salt`,
		metadata[archiveMetadataArchiveIDKey].value,
		metadata[archiveMetadataArchiveSaltKey].value,
	); err != nil {
		return fmt.Errorf("recording copied source archive identity: %w", err)
	}
	if previousArchiveID != metadata[archiveMetadataArchiveIDKey].value {
		if err := rekeyLocalArchiveRows(
			ctx, tx, previousArchiveID,
			metadata[archiveMetadataArchiveIDKey].value,
		); err != nil {
			return err
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
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning archive identity repair: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var previousArchiveID string
	if err := tx.QueryRowContext(ctx, `
		SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataArchiveIDKey,
	).Scan(&previousArchiveID); err != nil {
		return fmt.Errorf("reading prior archive identity: %w", err)
	}
	for _, item := range []struct {
		key   string
		value string
	}{
		{archiveMetadataArchiveIDKey, id},
		{archiveMetadataArchiveSaltKey, salt},
	} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO archive_metadata (key, value)
			VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value,
				updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
			item.key, item.value,
		); err != nil {
			return fmt.Errorf("setting archive identity: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source_archives (source_archive_id, source_archive_salt)
		VALUES (?, ?)
		ON CONFLICT(source_archive_id) DO UPDATE SET
			source_archive_salt = excluded.source_archive_salt`, id, salt,
	); err != nil {
		return fmt.Errorf("setting source archive identity: %w", err)
	}
	if previousArchiveID != id {
		if err := rekeyLocalArchiveRows(
			ctx, tx, previousArchiveID, id,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing archive identity repair: %w", err)
	}
	return nil
}

func (db *DB) UpsertProjectIdentityObservation(
	ctx context.Context,
	obs export.ProjectIdentityObservation,
) error {
	return db.upsertProjectIdentityObservationWithSnapshotProject(
		ctx, obs, obs.Project, false,
	)
}

// UpsertProjectIdentityObservationWithSnapshotProject publishes current
// aggregate evidence while preserving a separately labelled parser-time
// snapshot. Only the project label may differ, so both rows retain identical
// source evidence. An empty snapshot project preserves the aggregate and
// leaves any snapshot unchanged; session insertion paths use the state-aware
// variant below to remove only their newly created trigger fallback.
func (db *DB) UpsertProjectIdentityObservationWithSnapshotProject(
	ctx context.Context,
	obs export.ProjectIdentityObservation,
	snapshotProject string,
) error {
	return db.upsertProjectIdentityObservationWithSnapshotProject(
		ctx, obs, snapshotProject, true,
	)
}

func (db *DB) upsertProjectIdentityObservationWithSnapshotProject(
	ctx context.Context,
	obs export.ProjectIdentityObservation,
	snapshotProject string,
	allowSnapshotProjectCorrection bool,
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
	identity, err := db.localArchiveIdentity(ctx)
	if err != nil {
		return err
	}
	archiveSalt, err := db.GetArchiveSalt(ctx)
	if err != nil {
		return err
	}
	obs.SourceArchiveID = identity.SourceArchiveID
	obs.SourceArchiveSalt = archiveSalt

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.beginBunWriteTx(ctx)
	if err != nil {
		return fmt.Errorf("beginning project identity observation upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertProjectIdentityObservationWithSnapshotProjectBun(
		ctx, tx, obs, snapshotProject, false,
		allowSnapshotProjectCorrection,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing project identity observation upsert: %w", err)
	}
	return nil
}

// UpsertSessionWithProjectIdentity atomically writes the current session and
// aggregate identity while preserving parser-time snapshot evidence. The
// transaction-local insert result permits removal of only the fallback created
// by this session write.
func (db *DB) UpsertSessionWithProjectIdentity(
	s Session,
	obs export.ProjectIdentityObservation,
	snapshotProject string,
) error {
	_, err := db.upsertSessionWithProjectIdentity(
		s, obs, snapshotProject,
	)
	return err
}

func (db *DB) upsertSessionWithProjectIdentity(
	s Session,
	obs export.ProjectIdentityObservation,
	snapshotProject string,
) (sessionUpsertResult, error) {
	if err := db.requireWritable(); err != nil {
		return sessionUpsertResult{}, err
	}
	if strings.TrimSpace(s.ID) == "" {
		return sessionUpsertResult{}, fmt.Errorf("session id is required")
	}
	normalized, err := normalizeProjectIdentityObservation(obs)
	if err != nil {
		return sessionUpsertResult{}, err
	}
	if normalized.SessionID == "" {
		return sessionUpsertResult{},
			fmt.Errorf("identity observation session id is required")
	}
	if normalized.SessionID != s.ID {
		return sessionUpsertResult{}, fmt.Errorf(
			"identity observation session id %q does not match session id %q",
			normalized.SessionID, s.ID,
		)
	}
	obs = normalized
	identity, err := db.localArchiveIdentity(context.Background())
	if err != nil {
		return sessionUpsertResult{}, err
	}
	stampSessionArchiveIdentity(&s, identity)
	archiveSalt, err := db.GetArchiveSalt(context.Background())
	if err != nil {
		return sessionUpsertResult{}, err
	}
	obs.SourceArchiveID = identity.SourceArchiveID
	obs.SourceArchiveSalt = archiveSalt
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.beginBunWriteTx(context.Background())
	if err != nil {
		return sessionUpsertResult{},
			fmt.Errorf("beginning session identity upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := upsertArchiveSessionRow(
		context.Background(), tx, s, true,
	)
	if err != nil {
		return sessionUpsertResult{}, err
	}
	if obs.Project != "" {
		if err := upsertProjectIdentityObservationWithSnapshotProjectBun(
			context.Background(), tx, obs, snapshotProject, result.inserted, true,
		); err != nil {
			return sessionUpsertResult{}, err
		}
	}
	if !result.inserted && result.previousProject != result.currentProject {
		if err := reconcileSessionProjectIdentityAggregatesTx(
			context.Background(), tx, s.ID,
			[]string{result.previousProject, result.currentProject},
		); err != nil {
			return sessionUpsertResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return sessionUpsertResult{},
			fmt.Errorf("committing session identity upsert: %w", err)
	}
	return result, nil
}

func upsertProjectIdentityObservationWithSnapshotProjectBun(
	ctx context.Context,
	store bun.IDB,
	obs export.ProjectIdentityObservation,
	snapshotProject string,
	sessionInserted bool,
	allowSnapshotProjectCorrection bool,
) error {
	normalized, err := normalizeProjectIdentityObservation(obs)
	if err != nil {
		return err
	}
	if err := upsertProjectIdentityObservationBun(ctx, store, normalized); err != nil {
		return err
	}
	return writeSessionProjectIdentitySnapshotBun(
		ctx, store, normalized, snapshotProject, sessionInserted,
		allowSnapshotProjectCorrection,
	)
}

func upsertProjectIdentityObservationBun(
	ctx context.Context,
	store bun.IDB,
	obs export.ProjectIdentityObservation,
) error {
	if obs.SourceArchiveID == "" && obs.SessionID != "" {
		if err := store.NewSelect().Model((*bunmodel.Session)(nil)).
			Column("source_archive_id").
			Where("id = ?", obs.SessionID).Scan(ctx, &obs.SourceArchiveID); err != nil {
			return fmt.Errorf("reading identity observation archive: %w", err)
		}
	}
	if obs.SourceArchiveSalt == "" && obs.SourceArchiveID != "" {
		if err := store.NewSelect().Model((*bunmodel.SourceArchive)(nil)).
			Column("source_archive_salt").
			Where("source_archive_id = ?", obs.SourceArchiveID).
			Scan(ctx, &obs.SourceArchiveSalt); err != nil {
			return fmt.Errorf("reading identity observation archive salt: %w", err)
		}
	}
	if obs.SourceArchiveID == "" || obs.SourceArchiveSalt == "" {
		return fmt.Errorf("project identity observation archive identity is required")
	}
	rows, err := CanonicalProjectIdentityObservationRows(
		obs.SourceArchiveID, obs.SourceArchiveSalt,
		[]export.ProjectIdentityObservation{obs},
	)
	if err != nil {
		return err
	}
	row := rows[0]
	if row.GitRemote == "" &&
		row.RemoteResolution != string(export.ProjectResolutionAmbiguous) {
		exists, err := store.NewSelect().
			Model((*bunmodel.SourceProjectIdentityObservation)(nil)).
			Where("source_archive_id = ?", row.SourceArchiveID).
			Where("project = ?", row.Project).
			Where("machine = ?", row.Machine).
			Where("root_path = ?", row.RootPath).
			Where("(git_remote != '' OR remote_resolution = ?)",
				export.ProjectResolutionAmbiguous).
			Exists(ctx)
		if err != nil {
			return fmt.Errorf("checking project identity remote observation: %w", err)
		}
		if exists {
			return nil
		}
	} else if _, err := store.NewDelete().
		Model((*bunmodel.SourceProjectIdentityObservation)(nil)).
		Where("source_archive_id = ?", row.SourceArchiveID).
		Where("project = ?", row.Project).
		Where("machine = ?", row.Machine).
		Where("root_path = ?", row.RootPath).
		Where("git_remote = ''").
		Where("remote_resolution != ?", export.ProjectResolutionAmbiguous).
		Exec(ctx); err != nil {
		return fmt.Errorf("removing stale project identity root fallback: %w", err)
	}
	return UpsertProjectIdentityObservationRows(ctx, store, rows)
}

func writeSessionProjectIdentitySnapshotBun(
	ctx context.Context,
	store bun.IDB,
	obs export.ProjectIdentityObservation,
	snapshotProject string,
	sessionInserted bool,
	allowProjectCorrection bool,
) error {
	snapshotProject = strings.TrimSpace(snapshotProject)
	if snapshotProject == "" {
		if !sessionInserted {
			return nil
		}
		sessionID := strings.TrimSpace(obs.SessionID)
		if sessionID == "" {
			return nil
		}
		if _, err := store.NewDelete().
			Model((*bunmodel.SourceSessionProjectIdentitySnapshot)(nil)).
			Where("source_session_id = ?", sessionID).Exec(ctx); err != nil {
			return fmt.Errorf("deleting session project identity snapshot: %w", err)
		}
		return nil
	}
	snapshot := obs
	snapshot.Project = snapshotProject
	normalized, err := normalizeProjectIdentityObservation(snapshot)
	if err != nil {
		return err
	}
	return upsertSessionProjectIdentitySnapshotBun(
		ctx, store, normalized, allowProjectCorrection,
	)
}

func upsertSessionProjectIdentitySnapshotBun(
	ctx context.Context,
	store bun.IDB,
	obs export.ProjectIdentityObservation,
	allowProjectCorrection bool,
) error {
	if obs.SessionID == "" {
		return nil
	}
	var session struct {
		SourceArchiveID          string
		SourceDatabaseGeneration string
	}
	if err := store.NewSelect().Model((*bunmodel.Session)(nil)).
		Column("source_archive_id", "source_database_generation").
		Where("id = ?", obs.SessionID).Scan(ctx, &session); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("checking session for project identity snapshot: %w", err)
	}
	var existing bunmodel.SourceSessionProjectIdentitySnapshot
	err := store.NewSelect().Model(&existing).
		Where("source_archive_id = ?", session.SourceArchiveID).
		Where("source_database_generation = ?", session.SourceDatabaseGeneration).
		Where("source_session_id = ?", obs.SessionID).Scan(ctx)
	if err == nil {
		preserveExisting := existing.RemoteResolution ==
			string(export.ProjectResolutionResolved) ||
			existing.RemoteResolution == string(export.ProjectResolutionAmbiguous) ||
			(obs.RemoteResolution == export.ProjectResolutionUnknown &&
				(obs.Key == "" || strings.TrimSpace(existing.Key) != ""))
		if preserveExisting {
			if allowProjectCorrection && existing.Project != obs.Project {
				if _, err := store.NewUpdate().
					Model((*bunmodel.SourceSessionProjectIdentitySnapshot)(nil)).
					Set("project = ?", obs.Project).
					Where("source_archive_id = ?", session.SourceArchiveID).
					Where("source_database_generation = ?",
						session.SourceDatabaseGeneration).
					Where("source_session_id = ?", obs.SessionID).Exec(ctx); err != nil {
					return fmt.Errorf(
						"correcting session project identity snapshot label: %w", err,
					)
				}
			}
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking session project identity snapshot: %w", err)
	}
	rows, err := CanonicalSessionProjectIdentitySnapshotRows(
		session.SourceArchiveID, session.SourceDatabaseGeneration,
		[]export.ProjectIdentityObservation{obs},
	)
	if err != nil {
		return err
	}
	return UpsertSessionProjectIdentitySnapshotRows(ctx, store, rows)
}

// RestoreSessionProjectsFromIdentitySnapshots resets current project labels to
// resolved Git-backed parser-source snapshots. Full resync calls this after
// copying snapshots from the old archive and before reapplying active worktree
// mappings, so a missing checkout cannot replace resolved historical identity
// with a basename fallback.
func (db *DB) RestoreSessionProjectsFromIdentitySnapshots(
	ctx context.Context,
) (int, error) {
	if err := db.requireWritable(); err != nil {
		return 0, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	bunTx, err := db.beginBunWriteTx(ctx)
	if err != nil {
		return 0, fmt.Errorf(
			"beginning session project identity restore: %w", err,
		)
	}
	defer func() { _ = bunTx.Rollback() }()
	tx := bunTx.Tx

	type projectRestore struct {
		sessionID       string
		previousProject string
		currentProject  string
	}
	var restores []projectRestore
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.project, snap.project
		FROM sessions s
		JOIN source_session_project_identity_snapshots snap
		  ON snap.source_session_id = s.id
		 AND snap.source_archive_id = s.source_archive_id
		 AND snap.source_database_generation = s.source_database_generation
		WHERE snap.project != ''
		  AND snap.remote_resolution = 'resolved'
		  AND snap.git_remote != ''
		  AND s.deleted_at IS NULL
		  AND s.project != snap.project`)
	if err != nil {
		return 0, fmt.Errorf(
			"listing session project identity restores: %w", err,
		)
	}
	for rows.Next() {
		var restore projectRestore
		if err := rows.Scan(
			&restore.sessionID,
			&restore.previousProject,
			&restore.currentProject,
		); err != nil {
			rows.Close()
			return 0, fmt.Errorf(
				"scanning session project identity restore: %w", err,
			)
		}
		restores = append(restores, restore)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf(
			"iterating session project identity restores: %w", err,
		)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf(
			"closing session project identity restores: %w", err,
		)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET project = (
			SELECT snap.project
			FROM source_session_project_identity_snapshots snap
			WHERE snap.source_session_id = sessions.id
			  AND snap.source_archive_id = sessions.source_archive_id
			  AND snap.source_database_generation = sessions.source_database_generation
		)
		WHERE sessions.deleted_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM source_session_project_identity_snapshots snap
			WHERE snap.source_session_id = sessions.id
			  AND snap.source_archive_id = sessions.source_archive_id
			  AND snap.source_database_generation = sessions.source_database_generation
			  AND snap.project != ''
			  AND snap.remote_resolution = 'resolved'
			  AND snap.git_remote != ''
			  AND snap.project != sessions.project
		)`)
	if err != nil {
		return 0, fmt.Errorf(
			"restoring session projects from identity snapshots: %w", err,
		)
	}
	restored, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf(
			"counting restored session projects: %w", err,
		)
	}
	for _, restore := range restores {
		if err := reconcileSessionProjectIdentityAggregatesTx(
			ctx, bunTx, restore.sessionID,
			[]string{restore.previousProject, restore.currentProject},
		); err != nil {
			return 0, err
		}
	}
	if err := bunTx.Commit(); err != nil {
		return 0, fmt.Errorf(
			"committing session project identity restore: %w", err,
		)
	}
	return int(restored), nil
}

// reconcileSessionProjectIdentityAggregatesTx republishes only the immutable
// evidence key carried by sessionID under the supplied current project labels.
// Aggregate-only legacy evidence has no session snapshot and remains untouched.
func reconcileSessionProjectIdentityAggregatesTx(
	ctx context.Context,
	store bun.IDB,
	sessionID string,
	projects []string,
) error {
	var source bunmodel.SourceSessionProjectIdentitySnapshot
	err := store.NewSelect().Model(&source).
		Where("source_session_id = ?", sessionID).
		OrderExpr("observed_at DESC, source_session_id").
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading session project identity key: %w", err)
	}

	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		project = strings.TrimSpace(project)
		if project == "" {
			continue
		}
		if _, ok := seen[project]; ok {
			continue
		}
		seen[project] = struct{}{}

		if _, err := store.NewDelete().
			Model((*bunmodel.SourceProjectIdentityObservation)(nil)).
			Where("project = ?", project).
			Where("machine = ?", source.Machine).
			Where("root_path = ?", source.RootPath).
			Where("git_remote = ?", source.GitRemote).
			Exec(ctx); err != nil {
			return fmt.Errorf(
				"removing stale project identity aggregate key: %w", err,
			)
		}

		var winner bunmodel.SourceSessionProjectIdentitySnapshot
		err := store.NewSelect().Model(&winner).
			TableExpr("source_session_project_identity_snapshots AS snap").
			ColumnExpr("snap.*").
			Where("snap.machine = ?", source.Machine).
			Where("snap.root_path = ?", source.RootPath).
			Where("snap.git_remote = ?", source.GitRemote).
			Where(`EXISTS (
				SELECT 1 FROM sessions AS session
				WHERE session.id = snap.source_session_id
				  AND session.source_archive_id = snap.source_archive_id
				  AND session.source_database_generation = snap.source_database_generation
				  AND session.deleted_at IS NULL
				  AND session.machine = ?
				  AND session.project = ?
			)`, source.Machine, project).
			OrderExpr("snap.observed_at DESC, snap.source_session_id").
			Limit(1).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf(
				"selecting project identity aggregate winner: %w", err,
			)
		}

		var archive bunmodel.SourceArchive
		if err := store.NewSelect().Model(&archive).
			Where("source_archive_id = ?", winner.SourceArchiveID).
			Scan(ctx); err != nil {
			return fmt.Errorf("reading project identity winner archive: %w", err)
		}
		rows, err := CanonicalProjectIdentityObservationRows(
			winner.SourceArchiveID, archive.SourceArchiveSalt,
			[]export.ProjectIdentityObservation{{
				SourceArchiveID:   winner.SourceArchiveID,
				SourceArchiveSalt: archive.SourceArchiveSalt,
				SessionID:         winner.SourceSessionID,
				Project:           project, Machine: winner.Machine,
				RootPath: winner.RootPath, GitRemote: winner.GitRemote,
				GitRemoteName:        winner.GitRemoteName,
				RepositoryPath:       winner.RepositoryPath,
				WorktreeName:         winner.WorktreeName,
				WorktreeRootPath:     winner.WorktreeRootPath,
				WorktreeRelationship: export.WorktreeRelationship(winner.WorktreeRelationship),
				CheckoutState:        export.CheckoutState(winner.CheckoutState),
				GitBranch:            winner.GitBranch,
				RemoteResolution:     export.ProjectResolution(winner.RemoteResolution),
				RemoteCandidateCount: winner.RemoteCandidateCount,
				ObservedAt:           winner.ObservedAt.Time,
				NormalizedRemote:     winner.NormalizedRemote,
				KeySource:            winner.KeySource, Key: winner.Key,
			}},
		)
		if err != nil {
			return err
		}
		if err := UpsertProjectIdentityObservationRows(ctx, store, rows); err != nil {
			return fmt.Errorf(
				"reconciling project identity aggregate key: %w", err,
			)
		}
	}
	return nil
}

// upsertScrubbedProjectIdentityObservationTx is a migration-only seam for the
// startup credential scrub, which runs on a raw writer transaction before the
// guarded Bun handles are installed. Live identity writes use the canonical
// Bun helpers above.
func upsertScrubbedProjectIdentityObservationTx(
	ctx context.Context,
	tx *sql.Tx,
	obs export.ProjectIdentityObservation,
	excludeRemote string,
) error {
	if obs.SourceArchiveID == "" && obs.SessionID != "" {
		if err := tx.QueryRowContext(ctx, `
			SELECT source_archive_id FROM sessions WHERE id = ?`,
			obs.SessionID,
		).Scan(&obs.SourceArchiveID); err != nil {
			return fmt.Errorf("reading identity observation archive: %w", err)
		}
	}
	if obs.SourceArchiveSalt == "" && obs.SourceArchiveID != "" {
		if err := tx.QueryRowContext(ctx, `
			SELECT source_archive_salt FROM source_archives
			WHERE source_archive_id = ?`, obs.SourceArchiveID,
		).Scan(&obs.SourceArchiveSalt); err != nil {
			return fmt.Errorf("reading identity observation archive salt: %w", err)
		}
	}
	if obs.SourceArchiveID == "" || obs.SourceArchiveSalt == "" {
		return fmt.Errorf("project identity observation archive identity is required")
	}
	if obs.GitRemote == "" && obs.RemoteResolution != export.ProjectResolutionAmbiguous {
		var exists int
		query := `
			SELECT 1 FROM source_project_identity_observations
			WHERE source_archive_id = ?
			  AND project = ? AND machine = ? AND root_path = ?
			  AND (git_remote != '' OR remote_resolution = ?)`
		args := []any{
			obs.SourceArchiveID, obs.Project, obs.Machine, obs.RootPath,
			export.ProjectResolutionAmbiguous,
		}
		if excludeRemote != "" {
			query += ` AND git_remote != ?`
			args = append(args, excludeRemote)
		}
		query += ` LIMIT 1`
		err := tx.QueryRowContext(ctx, `
			`+strings.TrimSpace(query),
			args...,
		).Scan(&exists)
		if err == nil && exists == 1 {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking project identity remote observation: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `
		DELETE FROM source_project_identity_observations
		WHERE source_archive_id = ?
		  AND project = ? AND machine = ? AND root_path = ?
		  AND git_remote = '' AND remote_resolution != ?`,
		obs.SourceArchiveID, obs.Project, obs.Machine, obs.RootPath,
		export.ProjectResolutionAmbiguous,
	); err != nil {
		return fmt.Errorf("removing stale project identity root fallback: %w", err)
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			source_archive_id, source_archive_salt,
			project, machine, root_path, git_remote, git_remote_name,
			repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(
			source_archive_id, project, machine, root_path, git_remote
		) DO UPDATE SET
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
		FROM source_project_identity_observations
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
		if err := upsertScrubbedProjectIdentityObservationTx(
			ctx, tx, normalized, scrub.rawRemote,
		); err != nil {
			return fmt.Errorf("scrubbing project identity remote: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM source_project_identity_observations
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

// listProjectIdentityObservationsFrom retains the transaction-scoped identity
// projection used by export snapshots. Serving reads use BunStore's canonical
// source-scoped implementation.
func (db *DB) listProjectIdentityObservationsFrom(
	ctx context.Context,
	q sessionExportQuerier,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if labels == nil {
		return listProjectIdentityObservationsChunk(ctx, q, nil)
	}
	if len(labels) == 0 {
		return []export.ProjectIdentityObservation{}, nil
	}
	sorted := slices.Clone(labels)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var out []export.ProjectIdentityObservation
	err := queryChunked(sorted, func(chunk []string) error {
		part, err := listProjectIdentityObservationsChunk(ctx, q, chunk)
		if err != nil {
			return err
		}
		out = append(out, part...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// listProjectIdentityObservationsChunk runs one observation query for a
// single label chunk (nil means "all rows"); the chunk must already be
// within SQLite's bind-variable budget.
func listProjectIdentityObservationsChunk(
	ctx context.Context,
	q sessionExportQuerier,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	query := `SELECT source_archive_id, source_archive_salt,
		project, machine, root_path, git_remote, git_remote_name,
		repository_path, worktree_name, worktree_root_path,
		worktree_relationship, checkout_state, git_branch,
		remote_resolution, remote_candidate_count, observed_at,
		normalized_remote, key_source, key
		FROM source_project_identity_observations`
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

// ListSessionProjectIdentitySnapshotsByID returns the durable source identity
// for the requested sessions. The result is keyed by session ID and omits IDs
// without a snapshot.
func (db *DB) ListSessionProjectIdentitySnapshotsByID(
	ctx context.Context,
	sessionIDs []string,
) (map[string]export.ProjectIdentityObservation, error) {
	return db.listSessionProjectIdentitySnapshots(ctx, sessionIDs)
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
		SELECT source_session_id, project, machine, root_path, git_remote,
			git_remote_name, repository_path, worktree_name,
			worktree_root_path, worktree_relationship, checkout_state,
			git_branch, remote_resolution, remote_candidate_count,
			observed_at, normalized_remote, key_source, key
		FROM source_session_project_identity_snapshots
		WHERE source_session_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
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
		SELECT source_session_id, project, machine, root_path, git_remote,
			git_remote_name, repository_path, worktree_name,
			worktree_root_path, worktree_relationship, checkout_state,
			git_branch, remote_resolution, remote_candidate_count,
			observed_at, normalized_remote, key_source, key
		FROM source_session_project_identity_snapshots
		ORDER BY source_session_id`)
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

// ListPublishableSessionProjectIdentitySnapshots returns authoritative source
// snapshots owned by sessions in the current project scope. sessionIDs limits
// the result when non-nil; an empty non-nil slice returns no rows. Placeholder
// snapshots created before a session is inspected are deliberately excluded.
func (db *DB) ListPublishableSessionProjectIdentitySnapshots(
	ctx context.Context,
	sessionIDs, projects, excludeProjects []string,
) ([]export.ProjectIdentityObservation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sessionIDs != nil && len(sessionIDs) == 0 {
		return nil, nil
	}

	query := func(ids []string) ([]export.ProjectIdentityObservation, error) {
		predicates := []string{
			"owner.deleted_at IS NULL",
			"(TRIM(snap.key_source) != '' OR TRIM(snap.worktree_root_path) != '')",
		}
		var args []any
		appendSet := func(expression string, values []string, negate bool) {
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
			predicates = append(
				predicates,
				expression+op+"("+strings.Join(placeholders, ",")+")",
			)
		}
		appendSet("owner.project", projects, false)
		appendSet("owner.project", excludeProjects, true)
		appendSet("snap.source_session_id", ids, false)

		rows, err := db.getReader().QueryContext(ctx, `
			SELECT snap.source_session_id, snap.project, snap.machine, snap.root_path,
				snap.git_remote, snap.git_remote_name, snap.repository_path,
				snap.worktree_name, snap.worktree_root_path,
				snap.worktree_relationship, snap.checkout_state,
				snap.git_branch, snap.remote_resolution,
				snap.remote_candidate_count, snap.observed_at,
				snap.normalized_remote, snap.key_source, snap.key
			FROM source_session_project_identity_snapshots snap
			JOIN sessions owner ON owner.id = snap.source_session_id
			 AND owner.source_archive_id = snap.source_archive_id
			 AND owner.source_database_generation = snap.source_database_generation
			WHERE `+strings.Join(predicates, " AND ")+`
			ORDER BY snap.source_session_id`, args...)
		if err != nil {
			return nil, fmt.Errorf(
				"listing publishable session project identity snapshots: %w",
				err,
			)
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
				&obs.WorktreeRelationship, &obs.CheckoutState,
				&obs.GitBranch, &obs.RemoteResolution,
				&obs.RemoteCandidateCount, &observedAt,
				&obs.NormalizedRemote, &obs.KeySource, &obs.Key,
			); err != nil {
				return nil, fmt.Errorf(
					"scanning publishable session project identity snapshot: %w",
					err,
				)
			}
			obs.ObservedAt, err = time.Parse(time.RFC3339Nano, observedAt)
			if err != nil {
				return nil, fmt.Errorf(
					"parsing publishable session identity timestamp: %w", err,
				)
			}
			out = append(out, obs)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf(
				"iterating publishable session project identity snapshots: %w",
				err,
			)
		}
		return out, nil
	}

	if sessionIDs == nil {
		return query(nil)
	}
	chunkSize := max(maxSQLVars-len(projects)-len(excludeProjects), 1)
	var out []export.ProjectIdentityObservation
	err := queryChunkedSize(sessionIDs, chunkSize, func(ids []string) error {
		rows, err := query(ids)
		if err != nil {
			return err
		}
		out = append(out, rows...)
		return nil
	})
	return out, err
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
