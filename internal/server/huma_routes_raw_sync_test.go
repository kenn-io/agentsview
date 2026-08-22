package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawSyncTokenExchangeBypassesLegacyBearerAndUsesNamedScopes(t *testing.T) {
	t.Parallel()

	expiresAt := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	auth := &rawSyncAuthStub{
		authenticateCredential: func(
			_ context.Context,
			deviceID string,
			credential string,
		) (rawsync.AuthIdentity, error) {
			assert.Equal(t, "dev_test", deviceID)
			assert.Equal(t, "avdc_test", credential)
			return rawsync.AuthIdentity{
				TenantID: "tenant-from-auth",
				DeviceID: "dev_test",
			}, nil
		},
		issueToken: func(
			_ context.Context,
			deviceID string,
			credential string,
			scopes rawsync.DeviceTokenScope,
		) (rawsync.IssuedDeviceToken, error) {
			assert.Equal(t, "dev_test", deviceID)
			assert.Equal(t, "avdc_test", credential)
			assert.Equal(t, rawsync.ScopeNegotiate|rawsync.ScopeCommit, scopes)
			return rawsync.IssuedDeviceToken{
				Token: "avdt_test",
				Identity: rawsync.AuthIdentity{
					TenantID: "tenant-from-auth",
					DeviceID: "dev_test",
				},
				Scopes:    scopes,
				ExpiresAt: expiresAt,
			}, nil
		},
	}
	srv := newRawSyncHTTPTestServer(t, auth, new(rawSyncCustodyStub))

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/tokens",
		`{"scopes":["commit","negotiate"]}`,
		"avdc_test", "dev_test",
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		Token     string    `json:"token"`
		DeviceID  string    `json:"device_id"`
		Scopes    []string  `json:"scopes"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "avdt_test", response.Token)
	assert.Equal(t, "dev_test", response.DeviceID)
	assert.Equal(t, []string{"negotiate", "commit"}, response.Scopes)
	assert.Equal(t, expiresAt, response.ExpiresAt)
	assert.Equal(t, 1, auth.issueCalls)
}

func TestRawSyncNegotiationDerivesTenantAndDeviceFromScopedToken(t *testing.T) {
	t.Parallel()

	first := rawsync.ObjectRef{SHA256: strings.Repeat("1", 64), Length: 10}
	second := rawsync.ObjectRef{SHA256: strings.Repeat("2", 64), Length: 20}
	identity := rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	auth := &rawSyncAuthStub{
		authenticateToken: func(
			_ context.Context,
			token string,
			required rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			assert.Equal(t, "avdt_negotiate", token)
			assert.Equal(t, rawsync.ScopeNegotiate, required)
			return identity, nil
		},
	}
	custody := &rawSyncCustodyStub{
		missingObjects: func(
			_ context.Context,
			gotIdentity rawsync.AuthIdentity,
			provider parser.AgentType,
			objects []rawsync.ObjectRef,
		) ([]rawsync.ObjectRef, error) {
			assert.Equal(t, identity, gotIdentity)
			assert.Equal(t, parser.AgentCodex, provider)
			assert.Equal(t, []rawsync.ObjectRef{first, second}, objects)
			return []rawsync.ObjectRef{second}, nil
		},
	}
	srv := newRawSyncHTTPTestServer(t, auth, custody)
	body, err := json.Marshal(map[string]any{
		"provider": parser.AgentCodex,
		"objects":  []rawsync.ObjectRef{first, second},
	})
	require.NoError(t, err)

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/objects/missing",
		string(body), "avdt_negotiate", "",
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		Missing []rawsync.ObjectRef `json:"missing"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, []rawsync.ObjectRef{second}, response.Missing)
	assert.Equal(t, 1, auth.authenticateCalls)
	assert.Equal(t, 1, custody.missingCalls)
}

func TestRawSyncManifestCommitReturnsDurableReceipt(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	manifest := rawHTTPTestManifest()
	auth := &rawSyncAuthStub{
		authenticateToken: func(
			_ context.Context,
			token string,
			required rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			assert.Equal(t, "avdt_commit", token)
			assert.Equal(t, rawsync.ScopeCommit, required)
			return identity, nil
		},
	}
	custody := &rawSyncCustodyStub{
		commitManifest: func(
			_ context.Context,
			gotIdentity rawsync.AuthIdentity,
			gotManifest rawsync.Manifest,
		) (rawsync.CommitResult, error) {
			assert.Equal(t, identity, gotIdentity)
			assert.Equal(t, manifest, gotManifest)
			return rawsync.CommitResult{
				ManifestID: strings.Repeat("a", 64),
				Receipt:    strings.Repeat("b", 64),
				Generation: 7,
				Created:    true,
			}, nil
		},
	}
	srv := newRawSyncHTTPTestServer(t, auth, custody)
	body, err := json.Marshal(manifest)
	require.NoError(t, err)

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/manifests",
		string(body), "avdt_commit", "",
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		ManifestID string `json:"manifest_id"`
		Receipt    string `json:"receipt"`
		Generation int64  `json:"generation"`
		Created    bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, strings.Repeat("a", 64), response.ManifestID)
	assert.Equal(t, strings.Repeat("b", 64), response.Receipt)
	assert.Equal(t, int64(7), response.Generation)
	assert.True(t, response.Created)
	assert.Equal(t, 1, custody.commitCalls)
}

func TestRawSyncRejectsUnauthorizedTokenBeforeCustody(t *testing.T) {
	t.Parallel()

	auth := &rawSyncAuthStub{
		authenticateToken: func(
			_ context.Context,
			token string,
			required rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			assert.Equal(t, "avdt_revoked", token)
			assert.Equal(t, rawsync.ScopeNegotiate, required)
			return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
		},
	}
	custody := &rawSyncCustodyStub{
		missingObjects: func(
			context.Context,
			rawsync.AuthIdentity,
			parser.AgentType,
			[]rawsync.ObjectRef,
		) ([]rawsync.ObjectRef, error) {
			require.FailNow(t, "custody must not run after authentication failure")
			return nil, nil
		},
	}
	srv := newRawSyncHTTPTestServer(t, auth, custody)

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/objects/missing",
		`{"provider":"codex","objects":[]}`,
		"avdt_revoked", "",
	)

	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "avdt_revoked")
	assert.Zero(t, custody.missingCalls)
}

func TestRawSyncIgnoresInjectedTenantAndUsesAuthenticatedIdentity(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "dev-auth"}
	auth := &rawSyncAuthStub{
		authenticateToken: func(
			context.Context,
			string,
			rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			return identity, nil
		},
	}
	custody := &rawSyncCustodyStub{
		missingObjects: func(
			_ context.Context,
			gotIdentity rawsync.AuthIdentity,
			_ parser.AgentType,
			_ []rawsync.ObjectRef,
		) ([]rawsync.ObjectRef, error) {
			assert.Equal(t, identity, gotIdentity)
			return []rawsync.ObjectRef{}, nil
		},
	}
	srv := newRawSyncHTTPTestServer(t, auth, custody)

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/objects/missing",
		`{"tenant_id":"tenant-attacker","device_id":"dev-attacker","provider":"codex","objects":[]}`,
		"avdt_negotiate", "",
	)

	assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	assert.Zero(t, custody.missingCalls)
}

func TestRawSyncHeadConflictReturnsReconciliationState(t *testing.T) {
	t.Parallel()

	currentManifest := strings.Repeat("c", 64)
	currentReceipt := strings.Repeat("d", 64)
	auth := &rawSyncAuthStub{
		authenticateToken: func(
			context.Context,
			string,
			rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			return rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}, nil
		},
	}
	custody := &rawSyncCustodyStub{
		commitManifest: func(
			context.Context,
			rawsync.AuthIdentity,
			rawsync.Manifest,
		) (rawsync.CommitResult, error) {
			return rawsync.CommitResult{}, &rawsync.HeadConflictError{
				CurrentManifestID: currentManifest,
				CurrentReceipt:    currentReceipt,
				CurrentGeneration: 9,
			}
		},
	}
	srv := newRawSyncHTTPTestServer(t, auth, custody)
	body, err := json.Marshal(rawHTTPTestManifest())
	require.NoError(t, err)

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/manifests",
		string(body), "avdt_commit", "",
	)

	require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	var response apiErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "head_conflict", response.Code)
	assert.Equal(t, currentManifest, response.CurrentManifestID)
	assert.Equal(t, currentReceipt, response.CurrentReceipt)
	assert.Equal(t, int64(9), response.CurrentGeneration)
}

func TestRawSyncMissingObjectErrorDoesNotLeakBackendDetail(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("e", 64)
	auth := &rawSyncAuthStub{
		authenticateToken: func(
			context.Context,
			string,
			rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			return rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}, nil
		},
	}
	custody := &rawSyncCustodyStub{
		commitManifest: func(
			context.Context,
			rawsync.AuthIdentity,
			rawsync.Manifest,
		) (rawsync.CommitResult, error) {
			return rawsync.CommitResult{}, fmt.Errorf(
				"object %s unavailable: %w", digest, rawsync.ErrMissingObject,
			)
		},
	}
	srv := newRawSyncHTTPTestServer(t, auth, custody)
	body, err := json.Marshal(rawHTTPTestManifest())
	require.NoError(t, err)

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/manifests",
		string(body), "avdt_commit", "",
	)

	assert.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"code":"missing_object"`)
	assert.NotContains(t, recorder.Body.String(), digest)
}

func TestRawSyncCustodyDeadlineReturnsGatewayTimeout(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	auth := &rawSyncAuthStub{
		authenticateToken: func(
			context.Context,
			string,
			rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			return identity, nil
		},
	}

	tests := []struct {
		name    string
		path    string
		body    string
		bearer  string
		custody *rawSyncCustodyStub
	}{
		{
			name:   "negotiation",
			path:   "/api/v1/raw-sync/objects/missing",
			body:   `{"provider":"codex","objects":[]}`,
			bearer: "avdt_negotiate",
			custody: &rawSyncCustodyStub{
				missingObjects: func(
					context.Context,
					rawsync.AuthIdentity,
					parser.AgentType,
					[]rawsync.ObjectRef,
				) ([]rawsync.ObjectRef, error) {
					return nil, fmt.Errorf("custody detail: %w", context.DeadlineExceeded)
				},
			},
		},
		{
			name:   "manifest",
			path:   "/api/v1/raw-sync/manifests",
			bearer: "avdt_commit",
			custody: &rawSyncCustodyStub{
				commitManifest: func(
					context.Context,
					rawsync.AuthIdentity,
					rawsync.Manifest,
				) (rawsync.CommitResult, error) {
					return rawsync.CommitResult{}, fmt.Errorf(
						"custody detail: %w", context.DeadlineExceeded,
					)
				},
			},
		},
	}
	manifestBody, err := json.Marshal(rawHTTPTestManifest())
	require.NoError(t, err)
	tests[1].body = string(manifestBody)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newRawSyncHTTPTestServer(t, auth, tt.custody)
			recorder := serveRawSyncJSON(
				t, srv, http.MethodPost, tt.path, tt.body, tt.bearer, "",
			)

			require.Equal(t, http.StatusGatewayTimeout, recorder.Code)
			assert.Contains(t, recorder.Body.String(), "gateway timeout")
			assert.NotContains(t, recorder.Body.String(), "custody detail")
		})
	}
}

func TestRawSyncBoundsJSONBeforeCustody(t *testing.T) {
	t.Parallel()

	auth := &rawSyncAuthStub{
		authenticateToken: func(
			context.Context,
			string,
			rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			return rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}, nil
		},
	}
	custody := &rawSyncCustodyStub{
		missingObjects: func(
			context.Context,
			rawsync.AuthIdentity,
			parser.AgentType,
			[]rawsync.ObjectRef,
		) ([]rawsync.ObjectRef, error) {
			require.FailNow(t, "oversized JSON must not reach custody")
			return nil, nil
		},
	}
	srv := newRawSyncHTTPTestServer(t, auth, custody)
	body := `{"provider":"codex","objects":[],"padding":"` +
		strings.Repeat("x", (1<<20)+1) + `"}`

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/objects/missing",
		body, "avdt_negotiate", "",
	)

	assert.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code, recorder.Body.String())
	assert.Zero(t, custody.missingCalls)
}

func TestRawSyncBoundsTokenExchangeJSON(t *testing.T) {
	t.Parallel()

	auth := &rawSyncAuthStub{
		authenticateCredential: func(
			context.Context,
			string,
			string,
		) (rawsync.AuthIdentity, error) {
			return rawsync.AuthIdentity{
				TenantID: "tenant-a",
				DeviceID: "dev-a",
			}, nil
		},
		issueToken: func(
			context.Context,
			string,
			string,
			rawsync.DeviceTokenScope,
		) (rawsync.IssuedDeviceToken, error) {
			require.FailNow(t, "oversized JSON must not reach token issuance")
			return rawsync.IssuedDeviceToken{}, nil
		},
	}
	srv := newRawSyncHTTPTestServer(t, auth, new(rawSyncCustodyStub))
	body := `{"scopes":["negotiate"],"padding":"` +
		strings.Repeat("x", (4<<10)+1) + `"}`

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/tokens",
		body, "avdc_test", "dev-a",
	)

	assert.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code, recorder.Body.String())
	assert.Zero(t, auth.issueCalls)
}

func TestRawSyncRoutesDeclareBodyLimits(t *testing.T) {
	t.Parallel()

	srv := newRawSyncHTTPTestServer(
		t, new(rawSyncAuthStub), new(rawSyncCustodyStub),
	)
	paths := srv.api.OpenAPI().Paths

	require.Equal(t, int64(4<<10), paths["/api/v1/raw-sync/tokens"].Post.MaxBodyBytes)
	for _, path := range []string{
		"/api/v1/raw-sync/objects/missing",
		"/api/v1/raw-sync/manifests",
	} {
		require.Equal(t, int64(1<<20), paths[path].Post.MaxBodyBytes, path)
	}
}

func TestRawSyncCanonicalOpenAPISpecIncludesRoutes(t *testing.T) {
	t.Parallel()

	paths := OpenAPISpec(VersionInfo{}).Paths
	for _, path := range []string{
		"/api/v1/raw-sync/tokens",
		"/api/v1/raw-sync/objects/missing",
		"/api/v1/raw-sync/manifests",
		"/api/v1/raw-sync/uploads",
		"/api/v1/raw-sync/uploads/{upload_id}",
	} {
		assert.Contains(t, paths, path)
	}
}

func TestRawSyncTokenPreflightAllowsDeviceIDHeader(t *testing.T) {
	t.Parallel()

	srv := newRawSyncHTTPTestServer(
		t, new(rawSyncAuthStub), new(rawSyncCustodyStub),
	)
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/raw-sync/tokens", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.0.2.10:54321"
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set(
		"Access-Control-Request-Headers",
		"authorization, content-type, x-agentsview-device-id",
	)
	recorder := httptest.NewRecorder()

	srv.Handler().ServeHTTP(recorder, req)

	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
	assert.Equal(t, "https://client.example", recorder.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(
		t, recorder.Header().Get("Access-Control-Allow-Headers"), rawSyncDeviceIDHeader,
	)
}

func TestRawSyncAuthExemptionDoesNotApplyToOtherAPIs(t *testing.T) {
	t.Parallel()

	auth := &rawSyncAuthStub{}
	srv := newRawSyncHTTPTestServer(t, auth, new(rawSyncCustodyStub))

	recorder := serveRawSyncJSON(
		t, srv, http.MethodGet, "/api/ping", "", "avdt_raw", "",
	)

	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestRawSyncBearerRejectsOversizedHeader(t *testing.T) {
	t.Parallel()

	_, err := rawSyncBearer("Bearer " + strings.Repeat("a", 8*1024))
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)
}

func TestRawSyncAuthenticationUsesWriteTimeout(t *testing.T) {
	t.Parallel()

	auth := &rawSyncAuthStub{
		authenticateToken: func(
			ctx context.Context,
			_ string,
			_ rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			<-ctx.Done()
			return rawsync.AuthIdentity{}, ctx.Err()
		},
	}
	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		AuthToken:    "legacy-shared-token",
		RequireAuth:  true,
		WriteTimeout: 20 * time.Millisecond,
	}, nil, nil, WithRawSyncServices(auth, new(rawSyncCustodyStub)))

	started := time.Now()
	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/objects/missing",
		`{"provider":"codex","objects":[]}`,
		"avdt_negotiate", "",
	)

	assert.Equal(t, http.StatusGatewayTimeout, recorder.Code, recorder.Body.String())
	assert.Less(t, time.Since(started), time.Second)
	assert.Equal(t, 1, auth.authenticateCalls)
}

func TestRawSyncAuthenticationAndHandlerShareWriteDeadline(t *testing.T) {
	t.Parallel()

	type observedDeadline struct {
		deadline time.Time
		ok       bool
	}
	authDeadlines := make(chan observedDeadline, 1)
	custodyDeadlines := make(chan observedDeadline, 1)
	auth := &rawSyncAuthStub{
		authenticateToken: func(
			ctx context.Context,
			_ string,
			_ rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			deadline, ok := ctx.Deadline()
			authDeadlines <- observedDeadline{deadline: deadline, ok: ok}
			return rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}, nil
		},
	}
	custody := &rawSyncCustodyStub{
		missingObjects: func(
			ctx context.Context,
			_ rawsync.AuthIdentity,
			_ parser.AgentType,
			_ []rawsync.ObjectRef,
		) ([]rawsync.ObjectRef, error) {
			deadline, ok := ctx.Deadline()
			custodyDeadlines <- observedDeadline{deadline: deadline, ok: ok}
			return []rawsync.ObjectRef{}, nil
		},
	}
	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		AuthToken:    "legacy-shared-token",
		RequireAuth:  true,
		WriteTimeout: time.Second,
	}, nil, nil, WithRawSyncServices(auth, custody))

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/objects/missing",
		`{"provider":"codex","objects":[]}`,
		"avdt_negotiate", "",
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	authDeadline := <-authDeadlines
	custodyDeadline := <-custodyDeadlines
	require.True(t, authDeadline.ok)
	require.True(t, custodyDeadline.ok)
	assert.Equal(t, authDeadline.deadline, custodyDeadline.deadline)
}

func newRawSyncHTTPTestServer(
	t *testing.T,
	auth RawSyncDeviceAuth,
	custody RawSyncCustody,
) *Server {
	t.Helper()
	return New(config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		AuthToken:    "legacy-shared-token",
		RequireAuth:  true,
		WriteTimeout: 30 * time.Second,
	}, nil, nil, WithRawSyncServices(auth, custody))
}

func serveRawSyncJSON(
	t *testing.T,
	srv *Server,
	method string,
	path string,
	body string,
	bearer string,
	deviceID string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.0.2.10:54321"
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if deviceID != "" {
		req.Header.Set("X-AgentsView-Device-ID", deviceID)
	}
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, req)
	return recorder
}

func rawHTTPTestManifest() rawsync.Manifest {
	return rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         parser.AgentCodex,
		ConfiguredRootID: "root-a",
		SourceKey:        "sessions/a.jsonl",
		CaptureID:        "capture-a",
		CapturedAt: time.Date(
			2026, time.August, 19, 10, 0, 0, 0, time.UTC,
		),
		Kind: rawsync.ManifestSnapshot,
		Entries: []rawsync.Entry{{
			Path:   "sessions/a.jsonl",
			Type:   "file",
			Length: 10,
			Objects: []rawsync.ObjectRef{{
				SHA256: strings.Repeat("1", 64),
				Length: 10,
			}},
		}},
	}
}

type rawSyncAuthStub struct {
	authenticateCredential func(
		context.Context,
		string,
		string,
	) (rawsync.AuthIdentity, error)
	issueToken func(
		context.Context,
		string,
		string,
		rawsync.DeviceTokenScope,
	) (rawsync.IssuedDeviceToken, error)
	authenticateToken func(
		context.Context,
		string,
		rawsync.DeviceTokenScope,
	) (rawsync.AuthIdentity, error)
	issueCalls        int
	authenticateCalls int
}

func (s *rawSyncAuthStub) AuthenticateCredential(
	ctx context.Context,
	deviceID string,
	credential string,
) (rawsync.AuthIdentity, error) {
	return s.authenticateCredential(ctx, deviceID, credential)
}

func (s *rawSyncAuthStub) IssueToken(
	ctx context.Context,
	deviceID string,
	credential string,
	scopes rawsync.DeviceTokenScope,
) (rawsync.IssuedDeviceToken, error) {
	s.issueCalls++
	return s.issueToken(ctx, deviceID, credential, scopes)
}

func (s *rawSyncAuthStub) AuthenticateToken(
	ctx context.Context,
	token string,
	required rawsync.DeviceTokenScope,
) (rawsync.AuthIdentity, error) {
	s.authenticateCalls++
	return s.authenticateToken(ctx, token, required)
}

type rawSyncCustodyStub struct {
	missingObjects func(
		context.Context,
		rawsync.AuthIdentity,
		parser.AgentType,
		[]rawsync.ObjectRef,
	) ([]rawsync.ObjectRef, error)
	commitManifest func(
		context.Context,
		rawsync.AuthIdentity,
		rawsync.Manifest,
	) (rawsync.CommitResult, error)
	missingCalls int
	commitCalls  int
}

func (s *rawSyncCustodyStub) MissingObjects(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	provider parser.AgentType,
	objects []rawsync.ObjectRef,
) ([]rawsync.ObjectRef, error) {
	s.missingCalls++
	return s.missingObjects(ctx, identity, provider, objects)
}

func (s *rawSyncCustodyStub) CommitManifest(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	manifest rawsync.Manifest,
) (rawsync.CommitResult, error) {
	s.commitCalls++
	return s.commitManifest(ctx, identity, manifest)
}

var _ RawSyncDeviceAuth = (*rawSyncAuthStub)(nil)
var _ RawSyncCustody = (*rawSyncCustodyStub)(nil)
