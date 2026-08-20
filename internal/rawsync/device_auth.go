package rawsync

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrUnauthorized = errors.New("raw sync authentication failed")

const (
	deviceIDPrefix     = "dev_"
	credentialPrefix   = "avdc_"
	tokenPrefix        = "avdt_"
	deviceIDRandomSize = 16
	secretRandomSize   = 32
	secretEncodedSize  = 43
	maxDeviceNameBytes = 256
	maxDeviceTokenTTL  = 24 * time.Hour
)

// DeviceTokenScope is the fixed authorization surface for raw transport.
type DeviceTokenScope uint8

const (
	ScopeNegotiate DeviceTokenScope = 1 << iota
	ScopeUpload
	ScopeCommit
	ScopeStatus

	ScopeAll = ScopeNegotiate | ScopeUpload | ScopeCommit | ScopeStatus
)

// Allows reports whether the set contains every required scope.
func (s DeviceTokenScope) Allows(required DeviceTokenScope) bool {
	return s.valid() && required.valid() && s&required == required
}

func (s DeviceTokenScope) valid() bool {
	return s != 0 && s&^ScopeAll == 0
}

func (s DeviceTokenScope) single() bool {
	return s.valid() && s&(s-1) == 0
}

// CredentialDigest is the only persisted form of a device credential.
type CredentialDigest [sha256.Size]byte

// TokenDigest is the only persisted form of an upload access token.
type TokenDigest [sha256.Size]byte

// DeviceEnrollmentRecord is the durable server-side device record.
type DeviceEnrollmentRecord struct {
	Identity         AuthIdentity
	DisplayName      string
	CredentialDigest CredentialDigest
	CreatedAt        time.Time
	RevokedAt        *time.Time
}

// DeviceTokenRecord is the durable server-side token record.
type DeviceTokenRecord struct {
	Digest    TokenDigest
	Identity  AuthIdentity
	Scopes    DeviceTokenScope
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// DeviceEnrollment returns the credential exactly once to its caller.
type DeviceEnrollment struct {
	Identity    AuthIdentity
	DisplayName string
	Credential  string
	CreatedAt   time.Time
}

// IssuedDeviceToken is a short-lived scoped raw-transport credential.
type IssuedDeviceToken struct {
	Token     string
	Identity  AuthIdentity
	Scopes    DeviceTokenScope
	ExpiresAt time.Time
}

// DeviceAuthStore owns durable enrollment, token exchange, and revocation.
type DeviceAuthStore interface {
	EnrollDevice(context.Context, DeviceEnrollmentRecord) error
	IssueToken(
		context.Context, string, CredentialDigest, DeviceTokenRecord,
	) (AuthIdentity, error)
	AuthenticateToken(
		context.Context, TokenDigest, DeviceTokenScope, time.Time,
	) (AuthIdentity, error)
	RevokeDevice(context.Context, AuthIdentity, time.Time) (bool, error)
}

// DeviceAuthService creates digest-only credentials and scoped access tokens.
type DeviceAuthService struct {
	store  DeviceAuthStore
	ttl    time.Duration
	random io.Reader
	now    func() time.Time
}

// NewDeviceAuthService constructs the device authentication boundary.
func NewDeviceAuthService(
	store DeviceAuthStore,
	tokenTTL time.Duration,
) (*DeviceAuthService, error) {
	if isNilServiceDependency(store) {
		return nil, fmt.Errorf("%w: device auth store is required", ErrInvalid)
	}
	if tokenTTL <= 0 || tokenTTL > maxDeviceTokenTTL {
		return nil, fmt.Errorf(
			"%w: device token lifetime must be greater than zero and at most %s", ErrInvalid,
			maxDeviceTokenTTL,
		)
	}
	return &DeviceAuthService{
		store: store, ttl: tokenTTL, random: rand.Reader, now: time.Now,
	}, nil
}

// EnrollDevice creates an immutable device identity and one-time credential.
func (s *DeviceAuthService) EnrollDevice(
	ctx context.Context,
	tenantID string,
	displayName string,
) (DeviceEnrollment, error) {
	if err := validateOpaqueID("tenant", tenantID); err != nil {
		return DeviceEnrollment{}, err
	}
	if err := validateDeviceDisplayName(displayName); err != nil {
		return DeviceEnrollment{}, err
	}
	deviceID, err := s.randomValue(deviceIDPrefix, deviceIDRandomSize)
	if err != nil {
		return DeviceEnrollment{}, fmt.Errorf("generating raw sync device identity: %w", err)
	}
	identity, err := NewAuthIdentity(tenantID, deviceID)
	if err != nil {
		return DeviceEnrollment{}, err
	}
	credential, err := s.randomValue(credentialPrefix, secretRandomSize)
	if err != nil {
		return DeviceEnrollment{}, fmt.Errorf("generating raw sync device credential: %w", err)
	}
	createdAt := s.now().UTC()
	record := DeviceEnrollmentRecord{
		Identity:         identity,
		DisplayName:      displayName,
		CredentialDigest: CredentialDigest(sha256.Sum256([]byte(credential))),
		CreatedAt:        createdAt,
	}
	if err := s.store.EnrollDevice(ctx, record); err != nil {
		return DeviceEnrollment{}, fmt.Errorf("enrolling raw sync device: %w", err)
	}
	return DeviceEnrollment{
		Identity: identity, DisplayName: displayName,
		Credential: credential, CreatedAt: createdAt,
	}, nil
}

// IssueToken exchanges an active device credential for a scoped access token.
func (s *DeviceAuthService) IssueToken(
	ctx context.Context,
	deviceID string,
	credential string,
	scopes DeviceTokenScope,
) (IssuedDeviceToken, error) {
	if !scopes.valid() {
		return IssuedDeviceToken{}, fmt.Errorf("%w: invalid device token scopes", ErrInvalid)
	}
	if err := validateOpaqueID("device", deviceID); err != nil {
		return IssuedDeviceToken{}, err
	}
	credentialDigest, err := digestDeviceSecret(credential, credentialPrefix)
	if err != nil {
		return IssuedDeviceToken{}, err
	}
	token, err := s.randomValue(tokenPrefix, secretRandomSize)
	if err != nil {
		return IssuedDeviceToken{}, fmt.Errorf("generating raw sync access token: %w", err)
	}
	now := s.now().UTC()
	record := DeviceTokenRecord{
		Digest:    TokenDigest(sha256.Sum256([]byte(token))),
		Scopes:    scopes,
		IssuedAt:  now,
		ExpiresAt: now.Add(s.ttl),
	}
	identity, err := s.store.IssueToken(
		ctx, deviceID, CredentialDigest(credentialDigest), record,
	)
	if err != nil {
		return IssuedDeviceToken{}, fmt.Errorf("issuing raw sync access token: %w", err)
	}
	if err := validateServiceIdentity(identity); err != nil {
		return IssuedDeviceToken{}, fmt.Errorf("validating raw sync token identity: %w", err)
	}
	return IssuedDeviceToken{
		Token: token, Identity: identity, Scopes: scopes, ExpiresAt: record.ExpiresAt,
	}, nil
}

// AuthenticateToken derives tenant and device identity from one scoped token.
func (s *DeviceAuthService) AuthenticateToken(
	ctx context.Context,
	token string,
	required DeviceTokenScope,
) (AuthIdentity, error) {
	if !required.single() {
		return AuthIdentity{}, fmt.Errorf("%w: exactly one required scope is required", ErrInvalid)
	}
	digest, err := digestDeviceSecret(token, tokenPrefix)
	if err != nil {
		return AuthIdentity{}, err
	}
	identity, err := s.store.AuthenticateToken(
		ctx, TokenDigest(digest), required, s.now().UTC(),
	)
	if err != nil {
		return AuthIdentity{}, fmt.Errorf("authenticating raw sync access token: %w", err)
	}
	if err := validateServiceIdentity(identity); err != nil {
		return AuthIdentity{}, fmt.Errorf("validating raw sync token identity: %w", err)
	}
	return identity, nil
}

// RevokeDevice invalidates a device and all of its outstanding tokens.
func (s *DeviceAuthService) RevokeDevice(
	ctx context.Context,
	identity AuthIdentity,
) (bool, error) {
	if err := validateServiceIdentity(identity); err != nil {
		return false, err
	}
	revoked, err := s.store.RevokeDevice(ctx, identity, s.now().UTC())
	if err != nil {
		return false, fmt.Errorf("revoking raw sync device: %w", err)
	}
	return revoked, nil
}

func (s *DeviceAuthService) randomValue(prefix string, size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := io.ReadFull(s.random, buffer); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func digestDeviceSecret(value, prefix string) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	encoded, ok := strings.CutPrefix(value, prefix)
	if !ok || len(encoded) != secretEncodedSize {
		return zero, ErrUnauthorized
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != secretRandomSize ||
		base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return zero, ErrUnauthorized
	}
	return sha256.Sum256([]byte(value)), nil
}

func validateDeviceDisplayName(value string) error {
	if value == "" || len(value) > maxDeviceNameBytes || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: device display name is not canonical", ErrInvalid)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: device display name contains a control character", ErrInvalid)
		}
	}
	return nil
}
