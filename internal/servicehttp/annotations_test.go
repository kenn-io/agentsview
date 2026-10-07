package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestSetSessionLabelsMapsRemoteReadOnly(t *testing.T) {
	for _, tt := range []struct {
		method, path string
		call         func(*httpBackend) error
	}{
		{http.MethodPatch, "/api/v1/sessions/worker/labels", func(b *httpBackend) error {
			_, err := b.UpdateSessionLabels(t.Context(), "worker", []string{"nightly"}, nil)
			return err
		}},
		{http.MethodPut, "/api/v1/sessions/worker/labels", func(b *httpBackend) error {
			_, err := b.SetSessionLabels(t.Context(), "worker", nil)
			return err
		}},
		{http.MethodPut, "/api/v1/sessions/worker/parent", func(b *httpBackend) error {
			_, err := b.SetSessionParent(t.Context(), "worker", "manager")
			return err
		}},
		{http.MethodDelete, "/api/v1/sessions/worker/parent", func(b *httpBackend) error {
			_, err := b.ClearSessionParent(t.Context(), "worker")
			return err
		}},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, tt.method, r.Method)
				assert.Equal(t, tt.path, r.URL.Path)
				w.WriteHeader(http.StatusNotImplemented)
			}))
			t.Cleanup(srv.Close)
			backend := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
			backend.apiVersion = service.SessionAnnotationsAPIVersion
			err := tt.call(backend)
			require.ErrorIs(t, err, db.ErrReadOnly)
			assert.ErrorContains(t, err, "stop the read-only serve process and use the local DB, or start a local daemon")
		})
	}
}

func TestSessionAnnotationsRefuseOlderServer(t *testing.T) {
	for _, versionSource := range []string{"probed", "known"} {
		for _, tt := range []struct {
			name string
			call func(*httpBackend) error
		}{
			{"label filter", func(b *httpBackend) error {
				_, err := b.List(t.Context(), service.ListFilter{Labels: []string{"ticket=A"}})
				return err
			}},
			{"pr filter", func(b *httpBackend) error {
				_, err := b.List(t.Context(), service.ListFilter{PR: "owner/repo"})
				return err
			}},
			{"get parent", func(b *httpBackend) error {
				_, err := b.SessionParent(t.Context(), "worker")
				return err
			}},
			{"set parent", func(b *httpBackend) error {
				_, err := b.SetSessionParent(t.Context(), "worker", "manager")
				return err
			}},
			{"update labels", func(b *httpBackend) error {
				_, err := b.UpdateSessionLabels(t.Context(), "worker", []string{"nightly"}, nil)
				return err
			}},
			{"plain list", func(b *httpBackend) error {
				_, err := b.List(t.Context(), service.ListFilter{Project: "p"})
				return err
			}},
		} {
			t.Run(versionSource+"/"+tt.name, func(t *testing.T) {
				paths := []string{}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					paths = append(paths, r.URL.Path)
					assert.Equal(t, http.MethodGet, r.Method)
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/api/v1/version" {
						_, _ = w.Write([]byte(`{"api_version":10}`))
						return
					}
					_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
				}))
				t.Cleanup(srv.Close)
				backend := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
				if versionSource == "known" {
					backend = NewHTTPBackendForServer(srv.URL, "", HTTPServerCapabilities{APIVersion: 10}).(*httpBackend)
				}
				err := tt.call(backend)
				if tt.name == "plain list" {
					require.NoError(t, err)
					assert.Equal(t, []string{"/api/v1/sessions"}, paths)
					return
				}
				require.ErrorContains(t, err, "restart or upgrade the server")
				if versionSource == "probed" {
					assert.Equal(t, []string{"/api/v1/version"}, paths)
				} else {
					assert.Empty(t, paths)
				}
			})
		}
	}
}
