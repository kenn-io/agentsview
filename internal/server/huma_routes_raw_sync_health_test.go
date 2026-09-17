package server

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawSyncHealthResponse(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"}
	report := rawsync.JobHealthReport{
		ObservedAt:             time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
		MaxAttempts:            5,
		StaleAfterSeconds:      3600,
		OrphanedManifests:      []rawsync.OrphanedManifest{},
		OrphanedManifestCount:  2,
		ExpiredLeases:          []rawsync.ExpiredLease{},
		ExpiredLeaseCount:      1,
		FailedJobsByErrorClass: []rawsync.JobFailureClass{},
		FailedJobCount:         3,
		RetryingNearLimit:      []rawsync.JobAttemptWarning{},
		RetryingNearLimitCount: 4,
		StaleSourceHeads:       []rawsync.StaleSourceHead{},
		StaleSourceHeadCount:   5,
	}
	auth := &rawSyncAuthStub{
		authenticateToken: func(
			_ context.Context,
			token string,
			required rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			assert.Equal(t, "avdt_status", token)
			assert.Equal(t, rawsync.ScopeStatus, required)
			return identity, nil
		},
	}
	health := &rawSyncJobHealthStub{report: report}
	srv := New(
		config.Config{
			Host: "127.0.0.1", Port: 8080, RequireAuth: true,
			WriteTimeout: 30 * time.Second,
		}, nil, nil,
		WithRawSyncServices(auth, nil), WithRawSyncJobHealth(health),
	)

	recorder := serveRawSyncJSON(
		t, srv, http.MethodGet,
		"/api/v1/raw-sync/health?max_attempts=5&stale_after_seconds=3600",
		"", "avdt_status", "",
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var got rawsync.JobHealthReport
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
	assert.Equal(t, report, got)
	assert.Contains(t, recorder.Body.String(), `"orphaned_manifests":[]`)
	assert.Contains(t, recorder.Body.String(), `"failed_jobs_by_error_class":[]`)
	assert.NotContains(t, recorder.Body.String(), `"last_error":`)
	assert.Equal(t, identity, health.identity)
	assert.Equal(t, rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 3600}, health.query)
	assert.Equal(t, 1, health.calls)
}

func TestRawSyncHealthValidation(t *testing.T) {
	t.Parallel()

	auth := &rawSyncAuthStub{
		authenticateToken: func(
			context.Context,
			string,
			rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			return rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"}, nil
		},
	}
	health := &rawSyncJobHealthStub{}
	srv := newRawSyncHealthServer(t, auth, health)
	for _, query := range []string{
		"",
		"max_attempts=0&stale_after_seconds=1",
		"max_attempts=1&stale_after_seconds=0",
		"max_attempts=-1&stale_after_seconds=1",
		"max_attempts=2147483648&stale_after_seconds=1",
		"max_attempts=1&stale_after_seconds=2147483648",
	} {
		recorder := serveRawSyncJSON(
			t, srv, http.MethodGet,
			"/api/v1/raw-sync/health?"+query, "", "avdt_status", "",
		)
		assert.Equal(t, http.StatusBadRequest, recorder.Code, query)
	}
	assert.Zero(t, health.calls)
}

func TestRawSyncHealthAuth(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"}
	auth := &rawSyncAuthStub{
		authenticateToken: func(
			_ context.Context,
			token string,
			required rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			if token != "avdt_status" || required != rawsync.ScopeStatus {
				return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
			}
			return identity, nil
		},
	}
	health := &rawSyncJobHealthStub{}
	srv := newRawSyncHealthServer(t, auth, health)

	status := serveRawSyncJSON(
		t, srv, http.MethodGet,
		"/api/v1/raw-sync/health?max_attempts=1&stale_after_seconds=1",
		"", "avdt_status", "",
	)
	require.Equal(t, http.StatusOK, status.Code, status.Body.String())

	for _, bearer := range []string{"avdt_commit", "legacy-shared-token", "avdc_credential"} {
		recorder := serveRawSyncJSON(
			t, srv, http.MethodGet,
			"/api/v1/raw-sync/health?max_attempts=1&stale_after_seconds=1",
			"", bearer, "",
		)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code, bearer)
	}
	assert.Equal(t, 1, health.calls)
}

func TestRawSyncHealthErrors(t *testing.T) {
	t.Parallel()

	auth := &rawSyncAuthStub{
		authenticateToken: func(
			context.Context,
			string,
			rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			return rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"}, nil
		},
	}
	health := &rawSyncJobHealthStub{
		err: fmt.Errorf("database details: %w", context.DeadlineExceeded),
	}
	srv := newRawSyncHealthServer(t, auth, health)
	recorder := serveRawSyncJSON(
		t, srv, http.MethodGet,
		"/api/v1/raw-sync/health?max_attempts=1&stale_after_seconds=1",
		"", "avdt_status", "",
	)
	assert.Equal(t, http.StatusGatewayTimeout, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "gateway timeout")
	assert.NotContains(t, recorder.Body.String(), "database details")
}

func TestRawSyncHealthRegistration(t *testing.T) {
	t.Parallel()

	auth := &rawSyncAuthStub{
		authenticateToken: func(
			context.Context,
			string,
			rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			return rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"}, nil
		},
	}
	withoutHealth := New(
		config.Config{Host: "127.0.0.1", Port: 8080}, nil, nil,
		WithRawSyncServices(auth, new(rawSyncCustodyStub)),
	)
	assert.NotContains(t, withoutHealth.api.OpenAPI().Paths, "/api/v1/raw-sync/health")

	withHealth := newRawSyncHealthServer(t, auth, &rawSyncJobHealthStub{})
	assert.Contains(t, withHealth.api.OpenAPI().Paths, "/api/v1/raw-sync/health")

	withoutCustody := New(
		config.Config{
			Host: "127.0.0.1", Port: 8080, RequireAuth: true,
			WriteTimeout: 30 * time.Second,
		}, nil, nil,
		WithRawSyncServices(auth, nil), WithRawSyncJobHealth(&rawSyncJobHealthStub{}),
	)
	recorder := serveRawSyncJSON(
		t, withoutCustody, http.MethodGet,
		"/api/v1/raw-sync/health?max_attempts=1&stale_after_seconds=1",
		"", "avdt_status", "",
	)
	assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}

func newRawSyncHealthServer(
	t *testing.T,
	auth RawSyncDeviceAuth,
	health RawSyncJobHealth,
) *Server {
	t.Helper()
	return New(
		config.Config{
			Host: "127.0.0.1", Port: 8080, RequireAuth: true,
			WriteTimeout: 30 * time.Second,
		}, nil, nil,
		WithRawSyncServices(auth, nil), WithRawSyncJobHealth(health),
	)
}

type rawSyncJobHealthStub struct {
	report   rawsync.JobHealthReport
	err      error
	identity rawsync.AuthIdentity
	query    rawsync.JobHealthQuery
	calls    int
}

func (s *rawSyncJobHealthStub) RawJobHealth(
	_ context.Context,
	identity rawsync.AuthIdentity,
	query rawsync.JobHealthQuery,
) (rawsync.JobHealthReport, error) {
	s.calls++
	s.identity = identity
	s.query = query
	if s.err != nil {
		return rawsync.JobHealthReport{}, s.err
	}
	return s.report, nil
}

var _ RawSyncJobHealth = (*rawSyncJobHealthStub)(nil)
