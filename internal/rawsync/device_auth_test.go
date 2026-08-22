package rawsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeviceAuthServiceEnrollmentTokenAndRevocation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 7, 30, 0, 0, time.UTC)
	store := newMemoryDeviceAuthStore()
	service, err := NewDeviceAuthService(store, time.Hour)
	require.NoError(t, err)
	service.random = bytes.NewReader(append(
		append(bytes.Repeat([]byte{0}, 16), bytes.Repeat([]byte{1}, 32)...),
		bytes.Repeat([]byte{2}, 32)...,
	))
	service.now = func() time.Time { return now }

	enrollment, err := service.EnrollDevice(t.Context(), "tenant-a", "work laptop")
	require.NoError(t, err)
	assert.Equal(t, AuthIdentity{
		TenantID: "tenant-a",
		DeviceID: "dev_AAAAAAAAAAAAAAAAAAAAAA",
	}, enrollment.Identity)
	assert.Equal(t, "avdc_AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", enrollment.Credential)
	assert.Equal(t, "work laptop", enrollment.DisplayName)
	assert.Equal(t, now, enrollment.CreatedAt)

	stored := store.devices[enrollment.Identity.DeviceID]
	assert.Equal(t,
		CredentialDigest(sha256.Sum256([]byte(enrollment.Credential))),
		stored.CredentialDigest,
	)
	assert.NotContains(t, string(stored.CredentialDigest[:]), enrollment.Credential)

	issued, err := service.IssueToken(
		t.Context(), enrollment.Identity.DeviceID, enrollment.Credential,
		ScopeNegotiate|ScopeUpload,
	)
	require.NoError(t, err)
	assert.Equal(t, "avdt_AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI", issued.Token)
	assert.Equal(t, enrollment.Identity, issued.Identity)
	assert.Equal(t, ScopeNegotiate|ScopeUpload, issued.Scopes)
	assert.Equal(t, now.Add(time.Hour), issued.ExpiresAt)

	identity, err := service.AuthenticateToken(t.Context(), issued.Token, ScopeUpload)
	require.NoError(t, err)
	assert.Equal(t, enrollment.Identity, identity)

	_, err = service.AuthenticateToken(t.Context(), issued.Token, ScopeCommit)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.NotContains(t, err.Error(), issued.Token)

	revoked, err := service.RevokeDevice(t.Context(), enrollment.Identity)
	require.NoError(t, err)
	assert.True(t, revoked)

	_, err = service.AuthenticateToken(t.Context(), issued.Token, ScopeUpload)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.NotContains(t, err.Error(), issued.Token)
}

func TestDeviceAuthServiceCredentialAndExpiryFailuresStayOpaque(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 8, 0, 0, 0, time.UTC)
	store := newMemoryDeviceAuthStore()
	service, err := NewDeviceAuthService(store, 15*time.Minute)
	require.NoError(t, err)
	service.random = bytes.NewReader(append(
		append(
			append(bytes.Repeat([]byte{3}, 16), bytes.Repeat([]byte{4}, 32)...),
			bytes.Repeat([]byte{5}, 32)...,
		),
		bytes.Repeat([]byte{6}, 32)...,
	))
	service.now = func() time.Time { return now }

	enrollment, err := service.EnrollDevice(t.Context(), "tenant-b", "build host")
	require.NoError(t, err)

	_, err = service.IssueToken(
		t.Context(), enrollment.Identity.DeviceID,
		"avdc_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ScopeStatus,
	)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.NotContains(t, err.Error(), enrollment.Credential)

	issued, err := service.IssueToken(
		t.Context(), enrollment.Identity.DeviceID, enrollment.Credential, ScopeStatus,
	)
	require.NoError(t, err)

	now = issued.ExpiresAt
	_, err = service.AuthenticateToken(t.Context(), issued.Token, ScopeStatus)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.NotContains(t, err.Error(), issued.Token)
}

func TestDeviceAuthServiceAuthenticatesCredentialWithoutIssuingToken(t *testing.T) {
	t.Parallel()

	store := newMemoryDeviceAuthStore()
	service, err := NewDeviceAuthService(store, time.Hour)
	require.NoError(t, err)
	service.random = bytes.NewReader(append(
		bytes.Repeat([]byte{7}, deviceIDRandomSize),
		bytes.Repeat([]byte{8}, secretRandomSize)...,
	))

	enrollment, err := service.EnrollDevice(t.Context(), "tenant-c", "uploader")
	require.NoError(t, err)
	identity, err := service.AuthenticateCredential(
		t.Context(), enrollment.Identity.DeviceID, enrollment.Credential,
	)
	require.NoError(t, err)
	assert.Equal(t, enrollment.Identity, identity)
	assert.Zero(t, store.issueCalls)

	_, err = service.AuthenticateCredential(
		t.Context(), enrollment.Identity.DeviceID,
		"avdc_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.NotContains(t, err.Error(), enrollment.Credential)

	_, err = service.RevokeDevice(t.Context(), enrollment.Identity)
	require.NoError(t, err)
	_, err = service.AuthenticateCredential(
		t.Context(), enrollment.Identity.DeviceID, enrollment.Credential,
	)
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestDeviceAuthServiceRejectsInvalidScopeRequestsBeforeStoreAccess(t *testing.T) {
	t.Parallel()

	store := newMemoryDeviceAuthStore()
	service, err := NewDeviceAuthService(store, time.Hour)
	require.NoError(t, err)

	_, err = service.IssueToken(
		t.Context(), "device-a", "avdc_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 0,
	)
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Zero(t, store.issueCalls)

	_, err = service.AuthenticateToken(
		t.Context(), "avdt_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		ScopeNegotiate|ScopeUpload,
	)
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Zero(t, store.authenticateCalls)
}

func TestDeviceAuthServiceRejectsCredentialStoreIdentityMismatch(t *testing.T) {
	t.Parallel()

	store := &mismatchedDeviceAuthStore{identity: AuthIdentity{
		TenantID: "tenant-other",
		DeviceID: "dev_other",
	}}
	service, err := NewDeviceAuthService(store, time.Hour)
	require.NoError(t, err)
	service.random = bytes.NewReader(bytes.Repeat([]byte{9}, secretRandomSize))
	credential := "avdc_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	_, err = service.AuthenticateCredential(t.Context(), "dev_requested", credential)
	assert.ErrorIs(t, err, ErrConflict)

	_, err = service.IssueToken(
		t.Context(), "dev_requested", credential, ScopeNegotiate,
	)
	assert.ErrorIs(t, err, ErrConflict)
}

func TestDeviceTokenScopeNamesRoundTripCanonicalWireValues(t *testing.T) {
	t.Parallel()

	scope, err := ParseDeviceTokenScopeNames([]string{
		"commit", "negotiate", "status", "upload",
	})
	require.NoError(t, err)
	assert.Equal(t, ScopeAll, scope)
	assert.Equal(t, []string{
		"negotiate", "upload", "commit", "status",
	}, scope.Names())
}

func TestParseDeviceTokenScopeNamesRejectsAmbiguousValues(t *testing.T) {
	t.Parallel()

	for _, names := range [][]string{
		{},
		{"upload", "upload"},
		{"Upload"},
		{"unknown"},
	} {
		_, err := ParseDeviceTokenScopeNames(names)
		assert.ErrorIs(t, err, ErrInvalid, "names: %v", names)
	}
	assert.Nil(t, DeviceTokenScope(0).Names())
}

func TestDigestDeviceSecretRejectsOversizedInputWithoutProportionalAllocation(t *testing.T) {
	t.Parallel()

	oversized := credentialPrefix + strings.Repeat("A", 8*1024)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_, _ = digestDeviceSecret(oversized, credentialPrefix)
		}
	})

	assert.Less(t, result.AllocedBytesPerOp(), int64(1024),
		"invalid unauthenticated secrets must be rejected before proportional decoding")
}

type memoryDeviceAuthStore struct {
	devices           map[string]DeviceEnrollmentRecord
	tokens            map[TokenDigest]DeviceTokenRecord
	issueCalls        int
	authenticateCalls int
}

func newMemoryDeviceAuthStore() *memoryDeviceAuthStore {
	return &memoryDeviceAuthStore{
		devices: make(map[string]DeviceEnrollmentRecord),
		tokens:  make(map[TokenDigest]DeviceTokenRecord),
	}
}

func (s *memoryDeviceAuthStore) EnrollDevice(
	_ context.Context,
	record DeviceEnrollmentRecord,
) error {
	if _, exists := s.devices[record.Identity.DeviceID]; exists {
		return ErrConflict
	}
	s.devices[record.Identity.DeviceID] = record
	return nil
}

func (s *memoryDeviceAuthStore) AuthenticateCredential(
	_ context.Context,
	deviceID string,
	credential CredentialDigest,
) (AuthIdentity, error) {
	device, exists := s.devices[deviceID]
	if !exists || device.RevokedAt != nil || device.CredentialDigest != credential {
		return AuthIdentity{}, ErrUnauthorized
	}
	return device.Identity, nil
}

func (s *memoryDeviceAuthStore) IssueToken(
	_ context.Context,
	deviceID string,
	credential CredentialDigest,
	token DeviceTokenRecord,
) (AuthIdentity, error) {
	s.issueCalls++
	device, exists := s.devices[deviceID]
	if !exists || device.RevokedAt != nil || device.CredentialDigest != credential {
		return AuthIdentity{}, ErrUnauthorized
	}
	token.Identity = device.Identity
	s.tokens[token.Digest] = token
	return device.Identity, nil
}

func (s *memoryDeviceAuthStore) AuthenticateToken(
	_ context.Context,
	digest TokenDigest,
	required DeviceTokenScope,
	now time.Time,
) (AuthIdentity, error) {
	s.authenticateCalls++
	token, exists := s.tokens[digest]
	if !exists || !now.Before(token.ExpiresAt) || !token.Scopes.Allows(required) {
		return AuthIdentity{}, ErrUnauthorized
	}
	device, exists := s.devices[token.Identity.DeviceID]
	if !exists || device.RevokedAt != nil {
		return AuthIdentity{}, ErrUnauthorized
	}
	return token.Identity, nil
}

func (s *memoryDeviceAuthStore) RevokeDevice(
	_ context.Context,
	identity AuthIdentity,
	revokedAt time.Time,
) (bool, error) {
	device, exists := s.devices[identity.DeviceID]
	if !exists || device.Identity != identity || device.RevokedAt != nil {
		return false, nil
	}
	device.RevokedAt = new(revokedAt)
	s.devices[identity.DeviceID] = device
	return true, nil
}

var _ DeviceAuthStore = (*memoryDeviceAuthStore)(nil)

type mismatchedDeviceAuthStore struct {
	DeviceAuthStore
	identity AuthIdentity
}

func (s *mismatchedDeviceAuthStore) AuthenticateCredential(
	context.Context,
	string,
	CredentialDigest,
) (AuthIdentity, error) {
	return s.identity, nil
}

func (s *mismatchedDeviceAuthStore) IssueToken(
	context.Context,
	string,
	CredentialDigest,
	DeviceTokenRecord,
) (AuthIdentity, error) {
	return s.identity, nil
}
