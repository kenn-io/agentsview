//go:build pgtest

package postgres

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawDeviceAuthStoreLifecycleAndTenantIsolation(t *testing.T) {
	pg, store := newRawDeviceAuthTestStore(t)
	service, err := rawsync.NewDeviceAuthService(store, time.Hour)
	require.NoError(t, err)

	first, err := service.EnrollDevice(t.Context(), "tenant-a", "work laptop")
	require.NoError(t, err)
	second, err := service.EnrollDevice(t.Context(), "tenant-b", "build host")
	require.NoError(t, err)

	var storedTenant, storedName, storedDigest string
	err = pg.QueryRowContext(t.Context(), `
		SELECT tenant_id, display_name, encode(credential_sha256, 'hex')
		FROM raw_devices
		WHERE device_id = $1`, first.Identity.DeviceID).Scan(
		&storedTenant, &storedName, &storedDigest,
	)
	require.NoError(t, err)
	assert.Equal(t, "tenant-a", storedTenant)
	assert.Equal(t, "work laptop", storedName)
	wantCredentialDigest := sha256.Sum256([]byte(first.Credential))
	assert.Equal(t, hex.EncodeToString(wantCredentialDigest[:]), storedDigest)
	assert.NotContains(t, storedDigest, first.Credential)

	_, err = service.IssueToken(
		t.Context(), first.Identity.DeviceID, second.Credential, rawsync.ScopeAll,
	)
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)
	assert.NotContains(t, err.Error(), second.Credential)

	firstToken, err := service.IssueToken(
		t.Context(), first.Identity.DeviceID, first.Credential, rawsync.ScopeAll,
	)
	require.NoError(t, err)
	assert.Equal(t, first.Identity, firstToken.Identity)
	for _, scope := range []rawsync.DeviceTokenScope{
		rawsync.ScopeNegotiate,
		rawsync.ScopeUpload,
		rawsync.ScopeCommit,
		rawsync.ScopeStatus,
	} {
		identity, authErr := service.AuthenticateToken(t.Context(), firstToken.Token, scope)
		require.NoError(t, authErr)
		assert.Equal(t, first.Identity, identity)
	}

	tokenHash := sha256.Sum256([]byte(firstToken.Token))
	_, err = store.AuthenticateToken(
		t.Context(), rawsync.TokenDigest(tokenHash), rawsync.ScopeStatus,
		firstToken.ExpiresAt,
	)
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)

	revoked, err := service.RevokeDevice(t.Context(), first.Identity)
	require.NoError(t, err)
	assert.True(t, revoked)
	revoked, err = service.RevokeDevice(t.Context(), first.Identity)
	require.NoError(t, err)
	assert.False(t, revoked)

	_, err = service.AuthenticateToken(t.Context(), firstToken.Token, rawsync.ScopeUpload)
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)
	_, err = service.IssueToken(
		t.Context(), first.Identity.DeviceID, first.Credential, rawsync.ScopeUpload,
	)
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)

	secondToken, err := service.IssueToken(
		t.Context(), second.Identity.DeviceID, second.Credential, rawsync.ScopeStatus,
	)
	require.NoError(t, err)
	identity, err := service.AuthenticateToken(
		t.Context(), secondToken.Token, rawsync.ScopeStatus,
	)
	require.NoError(t, err)
	assert.Equal(t, second.Identity, identity)

	var tokenCount int
	require.NoError(t, pg.QueryRowContext(
		t.Context(), `SELECT count(*) FROM raw_device_tokens`,
	).Scan(&tokenCount))
	assert.Equal(t, 2, tokenCount,
		"revocation keeps token audit rows while invalidating them through the device")
}

func newRawDeviceAuthTestStore(t *testing.T) (*sql.DB, *RawDeviceAuthStore) {
	t.Helper()
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	store, err := NewRawDeviceAuthStore(pg)
	require.NoError(t, err)
	return pg, store
}
