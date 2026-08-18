package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/rawsync"
)

const rawIngestBatchRows = 256

// RawIngestStore implements raw custody metadata over PostgreSQL.
type RawIngestStore struct {
	db         *sql.DB
	newReceipt func() (string, error)
}

// NewRawIngestStore constructs a PostgreSQL raw custody metadata store.
func NewRawIngestStore(db *sql.DB) (*RawIngestStore, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: PostgreSQL connection is required", rawsync.ErrInvalid)
	}
	return &RawIngestStore{db: db, newReceipt: generateRawIngestReceipt}, nil
}

// RecordVerifiedObject records an object only after physical verification.
func (s *RawIngestStore) RecordVerifiedObject(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	object rawsync.ObjectRef,
) error {
	return s.RecordVerifiedObjects(ctx, identity, []rawsync.ObjectRef{object})
}

// RecordVerifiedObjects records physically verified objects in bounded batches.
func (s *RawIngestStore) RecordVerifiedObjects(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	objects []rawsync.ObjectRef,
) error {
	if err := validateRawIngestIdentity(identity); err != nil {
		return err
	}
	unique, err := uniqueRawIngestObjects(objects)
	if err != nil {
		return err
	}
	for start := 0; start < len(unique); start += rawIngestBatchRows {
		end := min(start+rawIngestBatchRows, len(unique))
		var query strings.Builder
		query.WriteString(`INSERT INTO raw_objects (tenant_id, sha256, size_bytes) VALUES `)
		args := make([]any, 0, 3*(end-start))
		for i, object := range unique[start:end] {
			if i > 0 {
				query.WriteByte(',')
			}
			argument := i*3 + 1
			fmt.Fprintf(&query, "($%d,$%d,$%d)", argument, argument+1, argument+2)
			args = append(args, identity.TenantID, object.SHA256, object.Length)
		}
		query.WriteString(` ON CONFLICT (tenant_id, sha256) DO UPDATE
			SET verified_at = now()
			WHERE raw_objects.size_bytes = EXCLUDED.size_bytes`)
		result, err := s.db.ExecContext(ctx, query.String(), args...)
		if err != nil {
			return fmt.Errorf("recording verified raw objects: %w", err)
		}
		accepted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking verified raw object registration: %w", err)
		}
		if accepted != int64(end-start) {
			return fmt.Errorf(
				"recording verified raw objects: %w: digest length differs",
				rawsync.ErrConflict,
			)
		}
	}
	return nil
}

// MissingObjects returns absent verified-object metadata in request order.
func (s *RawIngestStore) MissingObjects(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	objects []rawsync.ObjectRef,
) ([]rawsync.ObjectRef, error) {
	if err := validateRawIngestIdentity(identity); err != nil {
		return nil, err
	}
	unique, err := uniqueRawIngestObjects(objects)
	if err != nil {
		return nil, err
	}
	present, err := loadPresentRawObjects(ctx, s.db, identity.TenantID, unique)
	if err != nil {
		return nil, fmt.Errorf("querying verified raw objects: %w", err)
	}
	missing := make([]rawsync.ObjectRef, 0)
	for _, object := range unique {
		if !present[object] {
			missing = append(missing, object)
		}
	}
	return missing, nil
}

// CommitManifest atomically records a manifest, advances its head, and queues parsing.
func (s *RawIngestStore) CommitManifest(
	ctx context.Context,
	manifest rawsync.CanonicalManifest,
	processingVersion string,
) (rawsync.CommitResult, error) {
	if err := validateRawIngestProcessingVersion(processingVersion); err != nil {
		return rawsync.CommitResult{}, err
	}
	if err := validateRawIngestCanonicalManifest(manifest); err != nil {
		return rawsync.CommitResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("beginning raw manifest commit: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if result, found, err := lookupRawIngestCapture(ctx, tx, manifest); err != nil {
		return rawsync.CommitResult{}, err
	} else if found {
		return result, nil
	}
	if err := ensureRawIngestHead(ctx, tx, manifest); err != nil {
		return rawsync.CommitResult{}, err
	}
	head, err := lockRawIngestHead(ctx, tx, manifest)
	if err != nil {
		return rawsync.CommitResult{}, err
	}
	// A concurrent first commit may have become visible while this transaction
	// waited for the source-head lock. Recheck capture idempotency under the lock.
	if result, found, err := lookupRawIngestCapture(ctx, tx, manifest); err != nil {
		return rawsync.CommitResult{}, err
	} else if found {
		return result, nil
	}
	if manifest.Manifest.ExpectedParentReceipt != head.Receipt {
		return rawsync.CommitResult{}, &rawsync.HeadConflictError{
			CurrentManifestID: head.ManifestID,
			CurrentReceipt:    head.Receipt,
			CurrentGeneration: head.Generation,
		}
	}
	present, err := loadPresentRawObjects(
		ctx, tx, manifest.Identity.TenantID, manifest.Objects,
	)
	if err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("verifying raw manifest objects: %w", err)
	}
	for _, object := range manifest.Objects {
		if !present[object] {
			return rawsync.CommitResult{}, fmt.Errorf(
				"%w: %s", rawsync.ErrMissingObject, object.SHA256,
			)
		}
	}
	if head.Generation == math.MaxInt64 {
		return rawsync.CommitResult{}, fmt.Errorf("raw source generation exhausted")
	}
	generation := head.Generation + 1
	receipt, err := s.newReceipt()
	if err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("generating raw manifest receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO raw_manifests (
			tenant_id, manifest_id, device_id, provider, configured_root_id,
			source_key, source_key_sha256, capture_id, parent_receipt, receipt,
			generation, kind, captured_at, canonical_json
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		manifest.Identity.TenantID,
		manifest.ManifestID,
		manifest.Identity.DeviceID,
		string(manifest.Manifest.Provider),
		manifest.Manifest.ConfiguredRootID,
		manifest.Manifest.SourceKey,
		rawIngestKeyDigest(manifest.Manifest.SourceKey),
		manifest.Manifest.CaptureID,
		manifest.Manifest.ExpectedParentReceipt,
		receipt,
		generation,
		string(manifest.Manifest.Kind),
		manifest.Manifest.CapturedAt,
		manifest.CanonicalJSON,
	); err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("inserting raw manifest: %w", err)
	}
	if err := insertRawManifestEntries(ctx, tx, manifest); err != nil {
		return rawsync.CommitResult{}, err
	}
	if err := insertRawManifestObjects(ctx, tx, manifest); err != nil {
		return rawsync.CommitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO raw_ingest_jobs (
			tenant_id, manifest_id, stage, processing_version, state
		) VALUES ($1, $2, 'parse', $3, 'ready')`,
		manifest.Identity.TenantID, manifest.ManifestID, processingVersion,
	); err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("enqueuing raw parse job: %w", err)
	}
	updated, err := tx.ExecContext(ctx, `
		UPDATE raw_source_heads
		SET manifest_id = $6, receipt = $7, generation = $8, updated_at = now()
		WHERE tenant_id = $1 AND device_id = $2 AND provider = $3
			AND configured_root_id = $4 AND source_key_sha256 = $5 AND generation = $9`,
		manifest.Identity.TenantID,
		manifest.Identity.DeviceID,
		string(manifest.Manifest.Provider),
		manifest.Manifest.ConfiguredRootID,
		rawIngestKeyDigest(manifest.Manifest.SourceKey),
		manifest.ManifestID,
		receipt,
		generation,
		head.Generation,
	)
	if err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("advancing raw source head: %w", err)
	}
	affected, err := updated.RowsAffected()
	if err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("checking raw source head advance: %w", err)
	}
	if affected != 1 {
		return rawsync.CommitResult{}, fmt.Errorf("advancing raw source head affected %d rows", affected)
	}
	if err := tx.Commit(); err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("committing raw manifest: %w", err)
	}
	committed = true
	return rawsync.CommitResult{
		ManifestID: manifest.ManifestID,
		Receipt:    receipt,
		Generation: generation,
		Created:    true,
	}, nil
}

type rawIngestHead struct {
	ManifestID string
	Receipt    string
	Generation int64
}

func lookupRawIngestCapture(
	ctx context.Context,
	tx *sql.Tx,
	manifest rawsync.CanonicalManifest,
) (rawsync.CommitResult, bool, error) {
	var stored rawsync.CommitResult
	err := tx.QueryRowContext(ctx, `
		SELECT manifest_id, receipt, generation
		FROM raw_manifests
		WHERE tenant_id = $1 AND device_id = $2 AND provider = $3
			AND configured_root_id = $4 AND source_key_sha256 = $5 AND capture_id = $6`,
		manifest.Identity.TenantID,
		manifest.Identity.DeviceID,
		string(manifest.Manifest.Provider),
		manifest.Manifest.ConfiguredRootID,
		rawIngestKeyDigest(manifest.Manifest.SourceKey),
		manifest.Manifest.CaptureID,
	).Scan(&stored.ManifestID, &stored.Receipt, &stored.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return rawsync.CommitResult{}, false, nil
	}
	if err != nil {
		return rawsync.CommitResult{}, false, fmt.Errorf("checking raw capture idempotency: %w", err)
	}
	if stored.ManifestID != manifest.ManifestID {
		return rawsync.CommitResult{}, false, fmt.Errorf(
			"raw capture identifier reused: %w", rawsync.ErrConflict,
		)
	}
	stored.Created = false
	return stored, true, nil
}

func ensureRawIngestHead(
	ctx context.Context,
	tx *sql.Tx,
	manifest rawsync.CanonicalManifest,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO raw_source_heads (
			tenant_id, device_id, provider, configured_root_id, source_key,
			source_key_sha256
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (
			tenant_id, device_id, provider, configured_root_id, source_key_sha256
		) DO NOTHING`,
		manifest.Identity.TenantID,
		manifest.Identity.DeviceID,
		string(manifest.Manifest.Provider),
		manifest.Manifest.ConfiguredRootID,
		manifest.Manifest.SourceKey,
		rawIngestKeyDigest(manifest.Manifest.SourceKey),
	)
	if err != nil {
		return fmt.Errorf("ensuring raw source head: %w", err)
	}
	return nil
}

func lockRawIngestHead(
	ctx context.Context,
	tx *sql.Tx,
	manifest rawsync.CanonicalManifest,
) (rawIngestHead, error) {
	var head rawIngestHead
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(manifest_id, ''), COALESCE(receipt, ''), generation
		FROM raw_source_heads
		WHERE tenant_id = $1 AND device_id = $2 AND provider = $3
			AND configured_root_id = $4 AND source_key_sha256 = $5
		FOR UPDATE`,
		manifest.Identity.TenantID,
		manifest.Identity.DeviceID,
		string(manifest.Manifest.Provider),
		manifest.Manifest.ConfiguredRootID,
		rawIngestKeyDigest(manifest.Manifest.SourceKey),
	).Scan(&head.ManifestID, &head.Receipt, &head.Generation)
	if err != nil {
		return rawIngestHead{}, fmt.Errorf("locking raw source head: %w", err)
	}
	return head, nil
}

type rawObjectQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadPresentRawObjects(
	ctx context.Context,
	queryer rawObjectQueryer,
	tenantID string,
	objects []rawsync.ObjectRef,
) (map[rawsync.ObjectRef]bool, error) {
	present := make(map[rawsync.ObjectRef]bool, len(objects))
	for start := 0; start < len(objects); start += rawIngestBatchRows {
		end := min(start+rawIngestBatchRows, len(objects))
		var query strings.Builder
		query.WriteString(`SELECT sha256, size_bytes FROM raw_objects WHERE tenant_id = $1 AND (sha256, size_bytes) IN (`)
		args := make([]any, 1, 1+2*(end-start))
		args[0] = tenantID
		for i, object := range objects[start:end] {
			if i > 0 {
				query.WriteByte(',')
			}
			argument := 2 + i*2
			fmt.Fprintf(&query, "($%d,$%d)", argument, argument+1)
			args = append(args, object.SHA256, object.Length)
		}
		query.WriteByte(')')
		rows, err := queryer.QueryContext(ctx, query.String(), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var object rawsync.ObjectRef
			if err := rows.Scan(&object.SHA256, &object.Length); err != nil {
				_ = rows.Close()
				return nil, err
			}
			present[object] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return present, nil
}

func insertRawManifestEntries(
	ctx context.Context,
	tx *sql.Tx,
	manifest rawsync.CanonicalManifest,
) error {
	entries := manifest.Manifest.Entries
	for start := 0; start < len(entries); start += rawIngestBatchRows {
		end := min(start+rawIngestBatchRows, len(entries))
		var query strings.Builder
		query.WriteString(`INSERT INTO raw_manifest_entries (tenant_id, manifest_id, entry_index, path, path_sha256, entry_type, size_bytes) VALUES `)
		args := make([]any, 0, 7*(end-start))
		for i, entry := range entries[start:end] {
			if i > 0 {
				query.WriteByte(',')
			}
			argument := i*7 + 1
			fmt.Fprintf(&query, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				argument, argument+1, argument+2, argument+3, argument+4, argument+5,
				argument+6,
			)
			args = append(args,
				manifest.Identity.TenantID, manifest.ManifestID, start+i,
				entry.Path, rawIngestKeyDigest(entry.Path), entry.Type, entry.Length,
			)
		}
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			return fmt.Errorf("inserting raw manifest entries: %w", err)
		}
	}
	return nil
}

type rawManifestObjectRow struct {
	EntryIndex  int
	ObjectIndex int
	Object      rawsync.ObjectRef
}

func insertRawManifestObjects(
	ctx context.Context,
	tx *sql.Tx,
	manifest rawsync.CanonicalManifest,
) error {
	rows := make([]rawManifestObjectRow, 0, len(manifest.Objects))
	for entryIndex, entry := range manifest.Manifest.Entries {
		for objectIndex, object := range entry.Objects {
			rows = append(rows, rawManifestObjectRow{
				EntryIndex: entryIndex, ObjectIndex: objectIndex, Object: object,
			})
		}
	}
	for start := 0; start < len(rows); start += rawIngestBatchRows {
		end := min(start+rawIngestBatchRows, len(rows))
		var query strings.Builder
		query.WriteString(`INSERT INTO raw_manifest_objects (tenant_id, manifest_id, entry_index, object_index, sha256, size_bytes) VALUES `)
		args := make([]any, 0, 6*(end-start))
		for i, row := range rows[start:end] {
			if i > 0 {
				query.WriteByte(',')
			}
			argument := i*6 + 1
			fmt.Fprintf(&query, "($%d,$%d,$%d,$%d,$%d,$%d)",
				argument, argument+1, argument+2, argument+3, argument+4, argument+5,
			)
			args = append(args,
				manifest.Identity.TenantID, manifest.ManifestID,
				row.EntryIndex, row.ObjectIndex, row.Object.SHA256, row.Object.Length,
			)
		}
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			return fmt.Errorf("inserting raw manifest object references: %w", err)
		}
	}
	return nil
}

func validateRawIngestCanonicalManifest(manifest rawsync.CanonicalManifest) error {
	return rawsync.ValidateCanonicalManifest(manifest)
}

func validateRawIngestIdentity(identity rawsync.AuthIdentity) error {
	validated, err := rawsync.NewAuthIdentity(identity.TenantID, identity.DeviceID)
	if err != nil || validated != identity {
		return fmt.Errorf("%w: authenticated identity is not canonical", rawsync.ErrInvalid)
	}
	return nil
}

func validateRawIngestObject(object rawsync.ObjectRef) error {
	validated, err := rawsync.NewObjectRef(object.SHA256, object.Length)
	if err != nil || validated != object {
		return fmt.Errorf("%w: raw object reference is not canonical", rawsync.ErrInvalid)
	}
	return nil
}

func uniqueRawIngestObjects(objects []rawsync.ObjectRef) ([]rawsync.ObjectRef, error) {
	seen := make(map[string]rawsync.ObjectRef, len(objects))
	unique := make([]rawsync.ObjectRef, 0, len(objects))
	for _, object := range objects {
		if err := validateRawIngestObject(object); err != nil {
			return nil, err
		}
		if previous, ok := seen[object.SHA256]; ok {
			if previous.Length != object.Length {
				return nil, fmt.Errorf("%w: digest has conflicting lengths", rawsync.ErrConflict)
			}
			continue
		}
		seen[object.SHA256] = object
		unique = append(unique, object)
	}
	return unique, nil
}

func validateRawIngestProcessingVersion(value string) error {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: processing version is not canonical", rawsync.ErrInvalid)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: processing version contains a control character", rawsync.ErrInvalid)
		}
	}
	return nil
}

// rawIngestKeyDigest returns the fixed-size key used in composite indexes for
// values whose full text may exceed the PostgreSQL B-tree entry limit.
func rawIngestKeyDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func generateRawIngestReceipt() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
