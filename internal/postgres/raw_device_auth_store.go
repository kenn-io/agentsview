package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/agentsview/internal/rawsync"
)

// RawDeviceAuthStore persists raw-transport devices and short-lived tokens.
type RawDeviceAuthStore struct {
	db *sql.DB
}

// NewRawDeviceAuthStore constructs a PostgreSQL raw device auth store.
func NewRawDeviceAuthStore(db *sql.DB) (*RawDeviceAuthStore, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: PostgreSQL connection is required", rawsync.ErrInvalid)
	}
	return &RawDeviceAuthStore{db: db}, nil
}

// EnrollDevice records only the device credential digest.
func (s *RawDeviceAuthStore) EnrollDevice(
	ctx context.Context,
	record rawsync.DeviceEnrollmentRecord,
) error {
	if err := validateRawDeviceEnrollment(record); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO raw_devices (
			device_id, tenant_id, display_name, credential_sha256, created_at
		) VALUES ($1, $2, $3, $4, $5)`,
		record.Identity.DeviceID,
		record.Identity.TenantID,
		record.DisplayName,
		record.CredentialDigest[:],
		record.CreatedAt,
	)
	if err != nil {
		if rawDeviceAuthUniqueViolation(err) {
			return fmt.Errorf("enrolling raw sync device: %w", rawsync.ErrConflict)
		}
		return fmt.Errorf("enrolling raw sync device: %w", err)
	}
	return nil
}

// AuthenticateCredential derives identity from one active device credential.
func (s *RawDeviceAuthStore) AuthenticateCredential(
	ctx context.Context,
	deviceID string,
	credential rawsync.CredentialDigest,
) (rawsync.AuthIdentity, error) {
	if err := validateRawDeviceLookupID(deviceID); err != nil {
		return rawsync.AuthIdentity{}, err
	}
	var identity rawsync.AuthIdentity
	err := s.db.QueryRowContext(ctx, `
		SELECT tenant_id, device_id
		FROM raw_devices
		WHERE device_id = $1
			AND credential_sha256 = $2
			AND revoked_at IS NULL`,
		deviceID, credential[:],
	).Scan(&identity.TenantID, &identity.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
	}
	if err != nil {
		return rawsync.AuthIdentity{}, fmt.Errorf(
			"authenticating raw sync device credential: %w", err,
		)
	}
	return identity, nil
}

// IssueToken atomically verifies one active device credential and records the token.
func (s *RawDeviceAuthStore) IssueToken(
	ctx context.Context,
	deviceID string,
	credential rawsync.CredentialDigest,
	token rawsync.DeviceTokenRecord,
) (rawsync.AuthIdentity, error) {
	if err := validateRawDeviceLookupID(deviceID); err != nil {
		return rawsync.AuthIdentity{}, err
	}
	if err := validateRawDeviceTokenRecord(token); err != nil {
		return rawsync.AuthIdentity{}, err
	}
	var identity rawsync.AuthIdentity
	err := s.db.QueryRowContext(ctx, `
		WITH active_device AS (
			SELECT tenant_id, device_id
			FROM raw_devices
			WHERE device_id = $1
				AND credential_sha256 = $2
				AND revoked_at IS NULL
		)
		INSERT INTO raw_device_tokens (
			token_sha256, tenant_id, device_id, scope_bits, issued_at, expires_at
		)
		SELECT $3, tenant_id, device_id, $4, $5, $6
		FROM active_device
		RETURNING tenant_id, device_id`,
		deviceID,
		credential[:],
		token.Digest[:],
		int16(token.Scopes),
		token.IssuedAt,
		token.ExpiresAt,
	).Scan(&identity.TenantID, &identity.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
	}
	if err != nil {
		if rawDeviceAuthUniqueViolation(err) {
			return rawsync.AuthIdentity{}, fmt.Errorf(
				"issuing raw sync token: %w", rawsync.ErrConflict,
			)
		}
		return rawsync.AuthIdentity{}, fmt.Errorf("issuing raw sync token: %w", err)
	}
	return identity, nil
}

// AuthenticateToken derives identity only from an active, unexpired scoped token.
func (s *RawDeviceAuthStore) AuthenticateToken(
	ctx context.Context,
	digest rawsync.TokenDigest,
	required rawsync.DeviceTokenScope,
	now time.Time,
) (rawsync.AuthIdentity, error) {
	if !rawDeviceAuthSingleScope(required) || now.IsZero() {
		return rawsync.AuthIdentity{}, fmt.Errorf(
			"%w: invalid raw sync token authentication request", rawsync.ErrInvalid,
		)
	}
	var identity rawsync.AuthIdentity
	err := s.db.QueryRowContext(ctx, `
		SELECT devices.tenant_id, devices.device_id
		FROM raw_device_tokens AS tokens
		JOIN raw_devices AS devices
			ON devices.tenant_id = tokens.tenant_id
			AND devices.device_id = tokens.device_id
		WHERE tokens.token_sha256 = $1
			AND tokens.expires_at > $2
			AND devices.revoked_at IS NULL
			AND (tokens.scope_bits & $3) = $3`,
		digest[:], now.UTC(), int16(required),
	).Scan(&identity.TenantID, &identity.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
	}
	if err != nil {
		return rawsync.AuthIdentity{}, fmt.Errorf("authenticating raw sync token: %w", err)
	}
	return identity, nil
}

// RevokeDevice invalidates the device once; token rows remain as audit history.
func (s *RawDeviceAuthStore) RevokeDevice(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	revokedAt time.Time,
) (bool, error) {
	if err := validateRawDeviceAuthIdentity(identity); err != nil {
		return false, err
	}
	if revokedAt.IsZero() {
		return false, fmt.Errorf("%w: revocation time is required", rawsync.ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE raw_devices
		SET revoked_at = $3
		WHERE tenant_id = $1 AND device_id = $2 AND revoked_at IS NULL`,
		identity.TenantID, identity.DeviceID, revokedAt.UTC(),
	)
	if err != nil {
		return false, fmt.Errorf("revoking raw sync device: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking raw sync device revocation: %w", err)
	}
	return updated == 1, nil
}

func validateRawDeviceEnrollment(record rawsync.DeviceEnrollmentRecord) error {
	if err := validateRawDeviceAuthIdentity(record.Identity); err != nil {
		return err
	}
	if record.DisplayName == "" || len(record.DisplayName) > 256 ||
		!utf8.ValidString(record.DisplayName) ||
		strings.TrimSpace(record.DisplayName) != record.DisplayName {
		return fmt.Errorf("%w: device display name is not canonical", rawsync.ErrInvalid)
	}
	for _, r := range record.DisplayName {
		if unicode.IsControl(r) {
			return fmt.Errorf(
				"%w: device display name contains a control character", rawsync.ErrInvalid,
			)
		}
	}
	if record.CreatedAt.IsZero() || record.CreatedAt != record.CreatedAt.UTC() ||
		record.RevokedAt != nil {
		return fmt.Errorf("%w: device enrollment time is not canonical", rawsync.ErrInvalid)
	}
	return nil
}

func validateRawDeviceTokenRecord(token rawsync.DeviceTokenRecord) error {
	if !token.Scopes.Allows(token.Scopes) || token.Identity != (rawsync.AuthIdentity{}) ||
		token.IssuedAt.IsZero() || token.IssuedAt != token.IssuedAt.UTC() ||
		token.ExpiresAt != token.ExpiresAt.UTC() ||
		!token.ExpiresAt.After(token.IssuedAt) {
		return fmt.Errorf("%w: raw sync token record is not canonical", rawsync.ErrInvalid)
	}
	return nil
}

func validateRawDeviceLookupID(deviceID string) error {
	_, err := rawsync.NewAuthIdentity("auth-lookup", deviceID)
	return err
}

func validateRawDeviceAuthIdentity(identity rawsync.AuthIdentity) error {
	validated, err := rawsync.NewAuthIdentity(identity.TenantID, identity.DeviceID)
	if err != nil || validated != identity {
		return fmt.Errorf("%w: authenticated identity is not canonical", rawsync.ErrInvalid)
	}
	return nil
}

func rawDeviceAuthSingleScope(scope rawsync.DeviceTokenScope) bool {
	switch scope {
	case rawsync.ScopeNegotiate, rawsync.ScopeUpload,
		rawsync.ScopeCommit, rawsync.ScopeStatus:
		return true
	default:
		return false
	}
}

func rawDeviceAuthUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr != nil && pgErr.Code == "23505"
}

var _ rawsync.DeviceAuthStore = (*RawDeviceAuthStore)(nil)
