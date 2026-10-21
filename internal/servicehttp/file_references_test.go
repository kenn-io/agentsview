package servicehttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestFileReferencesHTTPAdapter(t *testing.T) {
	for status, sentinel := range map[int]error{200: nil, 400: service.ErrFileReferencesInput, 401: nil, 404: service.ErrFileReferencesNotFound, 409: service.ErrSourceChanged, 503: nil} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/sessions/example/file-references", r.URL.Path)
				assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
				assert.Equal(t, "docbank", r.Header.Get(service.FileReferencesIntentHeader))
				assert.Equal(t, "bound", r.URL.Query().Get("binding"))
				assert.Equal(t, "selected", r.URL.Query().Get("occurrence_id"))
				assert.Equal(t, "3", r.URL.Query().Get("reference_offset"))
				assert.Equal(t, "2", r.URL.Query().Get("first_ordinal"))
				assert.Equal(t, "9", r.URL.Query().Get("last_ordinal"))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == 200 {
					_, err := w.Write([]byte(`{"binding":"daemon-generation","revision":"r","coverage":"complete","capability":"available","integration":"enabled","occurrences":[],"total":0,"next_offset":null}`))
					assert.NoError(t, err)
				} else {
					_, err := w.Write([]byte(`{"error":"request rejected"}`))
					assert.NoError(t, err)
				}
			}))
			defer daemon.Close()
			svc := servicehttp.NewHTTPBackend(daemon.URL, "test-token", true, "")
			out, err := svc.FileReferences(t.Context(), service.FileReferencesInput{SessionID: "example", Binding: "bound", OccurrenceID: "selected", ReferenceOffset: 3, FirstOrdinal: new(2), LastOrdinal: new(9)})
			if status == 200 {
				require.NoError(t, err)
				assert.Equal(t, "daemon-generation", out.Binding)
			} else {
				require.Error(t, err)
				assert.Nil(t, out)
				if sentinel != nil {
					assert.ErrorIs(t, err, sentinel)
				}
			}
		})
	}
}
