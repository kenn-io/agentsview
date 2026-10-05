package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/server"
)

func TestVersionAdvertisesFriction(t *testing.T) {
	tests := []struct {
		name   string
		enable bool
	}{
		{name: "disabled", enable: false},
		{name: "enabled", enable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []server.Option
			te := setupWithServerOpts(t, nil)
			if tt.enable {
				runner := &review.Runner{Store: te.db, Loc: time.UTC, Now: time.Now, BackfillDays: 7}
				opts = append(opts, server.WithFriction(runner, nil))
				te = setupWithServerOpts(t, opts)
			}
			version := decode[map[string]any](t, te.get(t, "/api/v1/version"))
			assert.Equal(t, true, version["friction_available"], "stored Friction data remains readable without a builder")
			assert.Equal(t, tt.enable, version["friction_build_available"])
			assert.Equal(t, false, version["kata_filing_available"])
		})
	}
}

func TestVersionHidesFrictionForReadOnlyMirror(t *testing.T) {
	te := setup(t)
	handler := server.New(config.Config{
		Host:    "127.0.0.1",
		DataDir: t.TempDir(),
	}, readOnlyFrictionStore{Store: te.db}, nil).Handler()

	versionReq := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/version", nil,
	)
	versionResp := httptest.NewRecorder()
	handler.ServeHTTP(versionResp, versionReq)
	assertStatus(t, versionResp, http.StatusOK)
	version := decode[map[string]any](t, versionResp)
	assert.Equal(t, false, version["friction_available"])

	frictionReq := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/friction/digests", nil,
	)
	frictionResp := httptest.NewRecorder()
	handler.ServeHTTP(frictionResp, frictionReq)
	assertStatus(t, frictionResp, http.StatusNotImplemented)
}

type readOnlyPGFrictionStore struct{ db.Store }

func (readOnlyPGFrictionStore) FrictionReadAvailable() bool { return true }
func (readOnlyPGFrictionStore) FrictionAvailable() bool     { return false }

func TestVersionAdvertisesReadableFrictionWithoutBuilderCapability(t *testing.T) {
	te := setup(t)
	handler := server.New(config.Config{
		Host:    "127.0.0.1",
		DataDir: t.TempDir(),
	}, readOnlyPGFrictionStore{Store: te.db}, nil).Handler()

	versionReq := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/version", nil,
	)
	versionResp := httptest.NewRecorder()
	handler.ServeHTTP(versionResp, versionReq)
	assertStatus(t, versionResp, http.StatusOK)
	version := decode[map[string]any](t, versionResp)
	assert.Equal(t, true, version["friction_available"])
	assert.Equal(t, false, version["friction_build_available"])

	frictionReq := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/friction/digests", nil,
	)
	frictionResp := httptest.NewRecorder()
	handler.ServeHTTP(frictionResp, frictionReq)
	assertStatus(t, frictionResp, http.StatusOK)
}
