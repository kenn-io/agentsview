//go:build pgtest

package postgres

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawsync"
)

type hostedFixture struct {
	admin, runtime            *sql.DB
	schema, role, tenant, dsn string
}

func newHostedFixture(t *testing.T, tenant string) hostedFixture {
	t.Helper()
	schema := fmt.Sprintf("hosted_test_%x", rand.Uint64())
	role := schema + "_runtime"
	admin, err := Open(testPGURL(t), schema, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
		assert.NoError(t, err)
		_, err = admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS "`+role+`"`)
		assert.NoError(t, err)
		assert.NoError(t, admin.Close())
	})
	require.NoError(t, EnsureHostedTenant(t.Context(), admin, schema, tenant))
	_, err = admin.ExecContext(t.Context(), `CREATE ROLE "`+role+`" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT`)
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `GRANT USAGE ON SCHEMA "`+schema+`" TO "`+role+`";
 GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA "`+schema+`" TO "`+role+`";
 GRANT USAGE ON ALL SEQUENCES IN SCHEMA "`+schema+`" TO "`+role+`"`)
	require.NoError(t, err)
	password := setHostedFixturePassword(t, admin, role)
	dsn, err := appendConnParams(testPGURL(t), map[string]string{"password": password, "user": role, "search_path": "public", "agentsview.tenant_id": "wrong"})
	require.NoError(t, err)
	runtime, err := OpenHosted(dsn, schema, tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	return hostedFixture{admin, runtime, schema, role, tenant, dsn}
}

func TestHostedTenantIsolation(t *testing.T) {
	a := newHostedFixture(t, "tenant-a")
	b := newHostedFixture(t, "tenant-b")
	for _, f := range []hostedFixture{a, b} {
		_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES ('same-id',$1,'device','claude')`, f.tenant)
		require.NoError(t, err)
		var got string
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT project FROM sessions WHERE id='same-id'`).Scan(&got))
		assert.Equal(t, f.tenant, got)
		require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
		assert.Error(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, "different"))
	}
	_, err := a.runtime.ExecContext(t.Context(), `SELECT * FROM "`+b.schema+`".sessions`)
	assert.Error(t, err)
	_, err = a.runtime.ExecContext(t.Context(), `INSERT INTO sessions(tenant_id,id,project,machine,agent) VALUES ('tenant-b','bad','p','m','claude')`)
	assert.Error(t, err)
	// A caller changing the setting cannot escape either the RLS or literal CHECK.
	conn, err := a.runtime.Conn(t.Context())
	require.NoError(t, err)
	for _, value := range []string{"", "tenant-b"} {
		_, err = conn.ExecContext(t.Context(), `SELECT set_config('agentsview.tenant_id',$1,false)`, value)
		require.NoError(t, err)
		var count int
		require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&count))
		assert.Zero(t, count)
		_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES ('bad','p','m','claude')`)
		assert.Error(t, err)
	}
	require.NoError(t, conn.Close())
	require.NoError(t, CheckHostedTenant(t.Context(), a.runtime, a.schema, a.tenant))
	assert.Error(t, CheckHostedTenant(t.Context(), a.admin, a.schema, a.tenant))
	assert.Error(t, CheckHostedTenant(t.Context(), a.runtime, b.schema, a.tenant))
	// Binding metadata must not be mutable even with table DML grants.
	_, err = a.runtime.ExecContext(t.Context(), `UPDATE hosted_tenant_binding SET tenant_id='tenant-b'`)
	assert.Error(t, err)
	_, err = a.runtime.ExecContext(t.Context(), `DELETE FROM hosted_tenant_binding`)
	assert.Error(t, err)
}

func TestHostedTenantPoolAndCatalog(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	assert.Equal(t, 5, f.runtime.Stats().MaxOpenConnections)
	for round := 0; round < 2; round++ {
		var connections []*sql.Conn
		for range 5 {
			c, err := f.runtime.Conn(t.Context())
			require.NoError(t, err)
			connections = append(connections, c)
			var schema, tenant string
			require.NoError(t, c.QueryRowContext(t.Context(), `SELECT current_schema(),current_setting('agentsview.tenant_id')`).Scan(&schema, &tenant))
			assert.Equal(t, f.schema, schema)
			assert.Equal(t, f.tenant, tenant)
		}
		for _, c := range connections {
			require.NoError(t, c.Close())
		}
		f.runtime.SetConnMaxLifetime(time.Nanosecond)
	}
	var unsafe int
	require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind='r' AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity OR NOT EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attname='tenant_id' AND a.attnotnull) OR NOT EXISTS (SELECT 1 FROM pg_constraint k JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=k.conkey[1] WHERE k.conrelid=c.oid AND k.contype='p' AND a.attname='tenant_id'))`, f.schema).Scan(&unsafe))
	assert.Zero(t, unsafe)
	require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND k.contype='f' AND ((SELECT attname FROM pg_attribute WHERE attrelid=k.conrelid AND attnum=k.conkey[1]) <> 'tenant_id' OR (SELECT attname FROM pg_attribute WHERE attrelid=k.confrelid AND attnum=k.confkey[1]) <> 'tenant_id')`, f.schema).Scan(&unsafe))
	assert.Zero(t, unsafe)
	_, err := f.admin.ExecContext(t.Context(), `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`)
	require.NoError(t, err)
	assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
}

func TestHostedTenantRejectsForeignRawRowsAndUnknownTables(t *testing.T) {
	pg := newHostedLegacyFixture(t)
	var schema string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	_, err := pg.ExecContext(t.Context(), `INSERT INTO raw_objects(tenant_id,sha256,size_bytes) VALUES ('tenant-b',repeat('a',64),1)`)
	require.NoError(t, err)
	assert.Error(t, EnsureHostedTenant(t.Context(), pg, schema, "tenant-a"))
	var tenant string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT tenant_id FROM raw_objects`).Scan(&tenant))
	assert.Equal(t, "tenant-b", tenant)
	f := newHostedFixture(t, "tenant-a")
	_, err = f.admin.ExecContext(t.Context(), `CREATE TABLE untracked_data(id integer)`)
	require.NoError(t, err)
	assert.Error(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
}

func TestHostedTenantBoundCustodyRejectsMismatch(t *testing.T) {
	pg := newHostedLegacyFixture(t)
	s, err := NewTenantRawIngestStore(pg, "tenant-a")
	require.NoError(t, err)
	identity := rawIngestIdentity(t, "tenant-b")
	assert.ErrorIs(t, s.RecordVerifiedObjects(t.Context(), identity, nil), rawsync.ErrUnauthorized)
	_, err = NewTenantRawIngestStore(pg, "")
	assert.Error(t, err)
}

func TestHostedTenantAuthAndJobFences(t *testing.T) {
	// Deliberately use an unrestricted legacy pool: explicit library fences must
	// work independently of the hosted role's redundant RLS enforcement.
	pg := newHostedLegacyFixture(t)
	legacy, err := NewRawIngestStore(pg)
	require.NoError(t, err)
	auth, err := NewRawDeviceAuthStore(pg)
	require.NoError(t, err)
	service, err := rawsync.NewDeviceAuthService(auth, time.Hour)
	require.NoError(t, err)
	first, err := service.EnrollDevice(t.Context(), "tenant-a", "first")
	require.NoError(t, err)
	second, err := service.EnrollDevice(t.Context(), "tenant-b", "second")
	require.NoError(t, err)
	boundAuth, err := NewTenantRawDeviceAuthStore(pg, "tenant-a")
	require.NoError(t, err)
	boundService, err := rawsync.NewDeviceAuthService(boundAuth, time.Hour)
	require.NoError(t, err)
	_, err = boundService.AuthenticateCredential(t.Context(), first.Identity.DeviceID, first.Credential)
	require.NoError(t, err)
	_, err = boundService.AuthenticateCredential(t.Context(), second.Identity.DeviceID, second.Credential)
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)
	_, err = boundService.IssueToken(t.Context(), second.Identity.DeviceID, second.Credential, rawsync.ScopeAll)
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)
	foreignToken, err := service.IssueToken(t.Context(), second.Identity.DeviceID, second.Credential, rawsync.ScopeAll)
	require.NoError(t, err)
	_, err = boundService.AuthenticateToken(t.Context(), foreignToken.Token, rawsync.ScopeStatus)
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)
	_, err = boundService.EnrollDevice(t.Context(), "tenant-b", "foreign")
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)
	for _, identity := range []rawsync.AuthIdentity{first.Identity, second.Identity} {
		object := rawIngestObject(t, "a", 1)
		require.NoError(t, legacy.RecordVerifiedObject(t.Context(), identity, object))
		manifest := rawIngestManifest(t, identity, "capture-a", "", rawIngestCapturedAt(), object)
		_, err = legacy.CommitManifest(t.Context(), manifest, "v1")
		require.NoError(t, err)
	}
	bound, err := NewTenantRawIngestStore(pg, "tenant-a")
	require.NoError(t, err)
	leases, err := bound.ClaimRawParseJobs(t.Context(), "worker", 2, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	assert.Equal(t, "tenant-a", leases[0].Identity.TenantID)
	other, err := NewTenantRawIngestStore(pg, "tenant-b")
	require.NoError(t, err)
	foreign, err := other.ClaimRawParseJobs(t.Context(), "worker", 2, time.Minute)
	require.NoError(t, err)
	require.Len(t, foreign, 1)
	assert.Error(t, bound.CompleteRawParseJob(t.Context(), foreign[0]))
	assert.Error(t, bound.HeartbeatRawParseJob(t.Context(), foreign[0], time.Minute))
	assert.Error(t, bound.RetryRawParseJob(t.Context(), foreign[0], time.Now(), "retry", "retry"))
	assert.Error(t, bound.FailRawParseJob(t.Context(), foreign[0], "failed", "failed"))
	require.NoError(t, bound.HeartbeatRawParseJob(t.Context(), leases[0], time.Minute))
	require.NoError(t, bound.CompleteRawParseJob(t.Context(), leases[0]))
	var state string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE tenant_id='tenant-b'`).Scan(&state))
	assert.Equal(t, "leased", state)
	_, err = pg.ExecContext(t.Context(), `UPDATE raw_source_heads SET manifest_id=NULL,receipt=NULL,generation=0 WHERE tenant_id='tenant-b'; UPDATE raw_ingest_jobs SET state='ready',lease_owner='',lease_expires_at=NULL WHERE tenant_id='tenant-b'`)
	require.NoError(t, err)
	leases, err = bound.ClaimRawParseJobs(t.Context(), "worker", 2, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, leases)
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE tenant_id='tenant-b'`).Scan(&state))
	assert.Equal(t, "ready", state, "tenant A idle cleanup cannot retire tenant B obsolete jobs")

}

func TestHostedTenantRejectsDynamicVectorStorageBeforeMutation(t *testing.T) {
	pg := newHostedLegacyFixture(t)
	var schema string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	_, err := pg.ExecContext(t.Context(), `CREATE TABLE vector_chunks_123(doc_key text PRIMARY KEY); INSERT INTO vector_chunks_123 VALUES ('kept')`)
	require.NoError(t, err)
	require.ErrorContains(t, EnsureHostedTenant(t.Context(), pg, schema, "tenant-a"), "hosted vector schema is unsupported")
	var value string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT doc_key FROM vector_chunks_123`).Scan(&value))
	assert.Equal(t, "kept", value)
	var changed bool
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT to_regclass('hosted_tenant_binding') IS NOT NULL OR EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='sessions'::regclass AND attname='tenant_id')`).Scan(&changed))
	assert.False(t, changed)
}

func TestHostedTenantRuntimeRoleGate(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	for _, test := range []struct{ name, grant, revoke string }{
		{"bypass", `ALTER ROLE "` + f.role + `" BYPASSRLS`, `ALTER ROLE "` + f.role + `" NOBYPASSRLS`},
		{"superuser", `ALTER ROLE "` + f.role + `" SUPERUSER`, `ALTER ROLE "` + f.role + `" NOSUPERUSER`},
		{"ddl", `GRANT CREATE ON SCHEMA "` + f.schema + `" TO "` + f.role + `"`, `REVOKE CREATE ON SCHEMA "` + f.schema + `" FROM "` + f.role + `"`},
		{"truncate", `GRANT TRUNCATE ON sessions TO "` + f.role + `"`, `REVOKE TRUNCATE ON sessions FROM "` + f.role + `"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.admin.ExecContext(t.Context(), test.grant)
			require.NoError(t, err)
			assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
			_, err = f.admin.ExecContext(t.Context(), test.revoke)
			require.NoError(t, err)
			require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
		})
	}
	sibling := newHostedFixture(t, "tenant-b")
	_, err := f.admin.ExecContext(t.Context(), `GRANT "`+sibling.role+`" TO "`+f.role+`"`)
	require.NoError(t, err)
	assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	_, err = f.admin.ExecContext(t.Context(), `REVOKE "`+sibling.role+`" FROM "`+f.role+`"`)
	require.NoError(t, err)
	_, err = f.admin.ExecContext(t.Context(), `GRANT USAGE ON SCHEMA "`+sibling.schema+`" TO "`+f.role+`"`)
	require.NoError(t, err)
	assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	_, err = f.admin.ExecContext(t.Context(), `REVOKE USAGE ON SCHEMA "`+sibling.schema+`" FROM "`+f.role+`"`)
	require.NoError(t, err)
}

func TestHostedTenantAdoptsLegacyAndEnforcesRelationships(t *testing.T) {
	pg := newHostedLegacyFixture(t)
	var schema string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	_, err := pg.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('preserved','project','device','claude'); INSERT INTO starred_sessions(session_id) VALUES('preserved')`)
	require.NoError(t, err)
	require.NoError(t, EnsureHostedTenant(t.Context(), pg, schema, "tenant-a"))
	var id, tenant string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT session_id,tenant_id FROM starred_sessions`).Scan(&id, &tenant))
	assert.Equal(t, "preserved", id)
	assert.Equal(t, "tenant-a", tenant)
	_, err = pg.ExecContext(t.Context(), `INSERT INTO starred_sessions(tenant_id,session_id) VALUES('tenant-b','preserved')`)
	assert.Error(t, err)
	// Missing device/source relationships are enforced even for an owner.
	_, err = pg.ExecContext(t.Context(), `INSERT INTO raw_source_heads(tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256) VALUES('tenant-a','missing','claude','root','source',repeat('a',64))`)
	assert.Error(t, err)
	_, err = pg.ExecContext(t.Context(), `DELETE FROM sessions WHERE id='preserved'`)
	require.NoError(t, err)
	var count int
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT count(*) FROM starred_sessions`).Scan(&count))
	assert.Zero(t, count, "composite FK retains legacy CASCADE")
}

func newHostedLegacyFixture(t *testing.T) *sql.DB {
	t.Helper()
	schema := fmt.Sprintf("hosted_legacy_%x", rand.Uint64())
	pg, err := Open(testPGURL(t), schema, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pg.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		assert.NoError(t, err)
		assert.NoError(t, pg.Close())
	})
	require.NoError(t, EnsureSchema(t.Context(), pg, schema))
	return pg
}

func TestHostedTenantCustodyRoundTrip(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	auth, err := NewTenantRawDeviceAuthStore(f.runtime, f.tenant)
	require.NoError(t, err)
	service, err := rawsync.NewDeviceAuthService(auth, time.Hour)
	require.NoError(t, err)
	enrollment, err := service.EnrollDevice(t.Context(), f.tenant, "device")
	require.NoError(t, err)
	custody, err := NewTenantRawIngestStore(f.runtime, f.tenant)
	require.NoError(t, err)
	object := rawIngestObject(t, "a", 1)
	require.NoError(t, custody.RecordVerifiedObject(t.Context(), enrollment.Identity, object))
	manifest := rawIngestManifest(t, enrollment.Identity, "capture-a", "", rawIngestCapturedAt(), object)
	_, err = custody.CommitManifest(t.Context(), manifest, "v1")
	require.NoError(t, err)
	leases, err := custody.ClaimRawParseJobs(t.Context(), "worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, custody.CompleteRawParseJob(t.Context(), leases[0]))
	var state string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs`).Scan(&state))
	assert.Equal(t, "complete", state)
}

func TestHostedTenantReadStore(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('read-id','kept','device','claude')`)
	require.NoError(t, err)
	store, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	defer store.Close()
	session, err := store.GetSession(t.Context(), "read-id")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "kept", session.Project)
	_, err = NewHostedStore(f.dsn, f.schema, "tenant-b", false)
	assert.Error(t, err)
}

func TestHostedTenantRejectsPrivilegedFunctions(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	_, err := f.admin.ExecContext(t.Context(), `CREATE FUNCTION privileged_read() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`)
	require.NoError(t, err)
	assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
}

func TestHostedTenantProvisioningUsesOneConnection(t *testing.T) {
	pg := newHostedLegacyFixture(t)
	var schema string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	pg.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, EnsureHostedTenant(ctx, pg, schema, "tenant-a"))
}

func TestHostedTenantConcurrentProvisioning(t *testing.T) {
	pg := newHostedLegacyFixture(t)
	var schema string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 5)
	for range 5 {
		go func() { done <- EnsureHostedTenant(ctx, pg, schema, "tenant-a") }()
	}
	for range 5 {
		require.NoError(t, <-done)
	}
	var count int
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_tenant_binding`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestHostedTenantFailedAdoptionRollsBack(t *testing.T) {
	pg := newHostedLegacyFixture(t)
	var schema string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	legacy, err := NewRawIngestStore(pg)
	require.NoError(t, err)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 1)
	require.NoError(t, legacy.RecordVerifiedObject(t.Context(), identity, object))
	manifest := rawIngestManifest(t, identity, "capture-a", "", rawIngestCapturedAt(), object)
	_, err = legacy.CommitManifest(t.Context(), manifest, "v1")
	require.NoError(t, err)
	// Legacy library callers may have stored custody before device enrollment.
	// The new required relationship cannot invent an authenticated device.
	require.ErrorContains(t, EnsureHostedTenant(t.Context(), pg, schema, "tenant-a"), "foreign key")
	var changed bool
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT to_regclass('hosted_tenant_binding') IS NOT NULL OR EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='sessions'::regclass AND attname='tenant_id')`).Scan(&changed))
	assert.False(t, changed)
	var count int
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_manifests`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestHostedTenantRejectsColumnOnlyPrivileges(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	table := fmt.Sprintf("hosted_column_%x", rand.Uint64())
	_, err := f.admin.ExecContext(t.Context(), `CREATE TABLE public."`+table+`"(value text); INSERT INTO public."`+table+`" VALUES ('sibling-row')`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.admin.ExecContext(context.Background(), `DROP TABLE public."`+table+`"`)
		assert.NoError(t, err)
	})
	for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "REFERENCES"} {
		t.Run(privilege, func(t *testing.T) {
			_, err := f.admin.ExecContext(t.Context(), `GRANT `+privilege+`(value) ON public."`+table+`" TO "`+f.role+`"`)
			require.NoError(t, err)
			if privilege == "SELECT" {
				var value string
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT value FROM public."`+table+`"`).Scan(&value))
				assert.Equal(t, "sibling-row", value)
			}
			assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
			_, err = f.admin.ExecContext(t.Context(), `REVOKE `+privilege+`(value) ON public."`+table+`" FROM "`+f.role+`"`)
			require.NoError(t, err)
			require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
		})
	}
	_, err = f.admin.ExecContext(t.Context(), `GRANT REFERENCES(id) ON sessions TO "`+f.role+`"`)
	require.NoError(t, err)
	assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
}

func TestHostedTenantTemporaryTableCannotShadowSessions(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	f.runtime.SetMaxOpenConns(1)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('fixed-id','protected','device','claude')`)
	require.NoError(t, err)
	conn, err := f.runtime.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	var beforePID int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&beforePID))
	_, err = conn.ExecContext(t.Context(), `CREATE TEMPORARY TABLE sessions(id text,project text); INSERT INTO pg_temp.sessions VALUES('fixed-id','shadow')`)
	require.NoError(t, err)
	var project string
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT project FROM sessions WHERE id='fixed-id'`).Scan(&project))
	assert.Equal(t, "protected", project)
	require.NoError(t, conn.Close())
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	var afterPID int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pg_backend_pid(),project FROM sessions WHERE id='fixed-id'`).Scan(&afterPID, &project))
	assert.Equal(t, beforePID, afterPID, "exercise reuse of the connection holding the temporary table")
	assert.Equal(t, "protected", project)
}

func TestHostedTenantFixtureUsesSCRAMCredential(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	var hasVerifier bool
	require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT COALESCE(rolpassword LIKE 'SCRAM-SHA-256$%',false) FROM pg_authid WHERE rolname=$1`, f.role).Scan(&hasVerifier))
	assert.True(t, hasVerifier, "runtime fixture must authenticate on SCRAM test databases")
}

// setHostedFixturePassword supplies a fresh credential for SCRAM test servers.
// Suppress statement/parameter logging only in this provisioning transaction;
// the secret is bound as a parameter and never appears in a SQL literal here.
func setHostedFixturePassword(t *testing.T, admin *sql.DB, role string) string {
	t.Helper()
	var random [32]byte
	_, err := cryptorand.Read(random[:])
	require.NoError(t, err)
	password := hex.EncodeToString(random[:])
	tx, err := admin.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(t.Context(), `SET LOCAL log_statement='none';
 SET LOCAL log_min_duration_statement=-1; SET LOCAL log_min_duration_sample=-1;
 SET LOCAL log_min_error_statement='panic'; SET LOCAL log_parameter_max_length=0;
 SET LOCAL log_parameter_max_length_on_error=0; SET LOCAL password_encryption='scram-sha-256'`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `SELECT set_config('hosted_test.role',$1,true),set_config('hosted_test.password',$2,true)`, role, password)
	require.True(t, err == nil, "binding synthetic runtime credential failed")
	_, err = tx.ExecContext(t.Context(), `DO $credential$ BEGIN
 EXECUTE format('ALTER ROLE %I PASSWORD %L',current_setting('hosted_test.role'),current_setting('hosted_test.password'));
 END $credential$`)
	require.True(t, err == nil, "provisioning synthetic runtime credential failed")
	require.NoError(t, tx.Commit())
	return password
}

func TestHostedTenantRejectsIncompleteSearchPath(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	quoted, err := quoteIdentifier(f.schema)
	require.NoError(t, err)
	dsn, err := appendConnParams(f.dsn, map[string]string{"search_path": quoted, "agentsview.tenant_id": f.tenant})
	require.NoError(t, err)
	unpinned, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer unpinned.Close()
	assert.Error(t, CheckHostedTenant(t.Context(), unpinned, f.schema, f.tenant), "schema-only path leaves temporary relations implicitly first")
}
