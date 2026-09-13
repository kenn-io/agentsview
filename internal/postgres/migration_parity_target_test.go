package postgres

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParityTargetConfigDigestExcludesCredentialsAndBindsEffectiveTarget(t *testing.T) {
	const targetID = "00000000-0000-4000-8000-000000000001"
	base, err := ParityTargetConfigDigest("postgres://first:secret@localhost:5432/archive?sslmode=disable", "hosted", "tenant-a", "before", targetID, true)
	require.NoError(t, err)
	credentialsOnly, err := ParityTargetConfigDigest("postgres://second:different@localhost:5432/archive?sslmode=disable", "hosted", "tenant-a", "before", targetID, true)
	require.NoError(t, err)
	assert.Equal(t, base, credentialsOnly)

	cases := []struct {
		name                                  string
		dsn, schema, tenant, target, identity string
		allow                                 bool
	}{
		{name: "endpoint", dsn: "postgres://first:secret@localhost:5433/archive?sslmode=disable", schema: "hosted", tenant: "tenant-a", target: "before", identity: targetID, allow: true},
		{name: "database", dsn: "postgres://first:secret@localhost:5432/other?sslmode=disable", schema: "hosted", tenant: "tenant-a", target: "before", identity: targetID, allow: true},
		{name: "schema", dsn: "postgres://first:secret@localhost:5432/archive?sslmode=disable", schema: "other", tenant: "tenant-a", target: "before", identity: targetID, allow: true},
		{name: "tenant", dsn: "postgres://first:secret@localhost:5432/archive?sslmode=disable", schema: "hosted", tenant: "tenant-b", target: "before", identity: targetID, allow: true},
		{name: "target", dsn: "postgres://first:secret@localhost:5432/archive?sslmode=disable", schema: "hosted", tenant: "tenant-a", target: "after", identity: targetID, allow: true},
		{name: "identity", dsn: "postgres://first:secret@localhost:5432/archive?sslmode=disable", schema: "hosted", tenant: "tenant-a", target: "before", identity: "00000000-0000-4000-8000-000000000002", allow: true},
		{name: "tls", dsn: "postgres://first:secret@localhost:5432/archive?sslmode=require", schema: "hosted", tenant: "tenant-a", target: "before", identity: targetID, allow: true},
		{name: "override", dsn: "postgres://first:secret@localhost:5432/archive?sslmode=disable", schema: "hosted", tenant: "tenant-a", target: "before", identity: targetID, allow: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParityTargetConfigDigest(tc.dsn, tc.schema, tc.tenant, tc.target, tc.identity, tc.allow)
			require.NoError(t, err)
			assert.NotEqual(t, base, got)
		})
	}
	requireTLS, err := ParityTargetConfigDigest("postgres://first:secret@example.com/archive?sslmode=require", "hosted", "tenant-a", "before", targetID, false)
	require.NoError(t, err)
	verifyCA, err := ParityTargetConfigDigest("postgres://first:secret@example.com/archive?sslmode=verify-ca", "hosted", "tenant-a", "before", targetID, false)
	require.NoError(t, err)
	assert.NotEqual(t, requireTLS, verifyCA, "effective certificate verification policy is binding input")

	const systemRoots = "/etc/ssl/certs/ca-certificates.crt"
	if _, statErr := os.Stat(systemRoots); statErr == nil {
		_, err = ParityTargetConfigDigest("postgres://first:secret@example.com/archive?sslmode=verify-full&sslrootcert="+systemRoots, "hosted", "tenant-a", "before", targetID, false)
		require.ErrorContains(t, err, "custom trust", "unrepresentable custom roots must fail closed")
	}
}
