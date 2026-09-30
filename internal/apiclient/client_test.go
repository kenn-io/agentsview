package apiclient

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostLedgerEventsAcceptsCreatedResponseAndFullChecksumRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"zone":"default","source":"host-a","source_seq":1,"checksum":4294967295,"event_ids":["event-1"]}`))
	}))
	t.Cleanup(server.Close)

	api, err := NewHTTPClient(server.URL, "", server.Client())
	require.NoError(t, err)
	response, err := api.PostAPIV1LedgerEventsWithResponse(
		t.Context(), &PostAPIV1LedgerEventsRequestOptions{
			Body: &LedgerAppendRequest{
				Events: []LedgerAppendEvent{{EventClass: "health"}},
			},
		},
	)
	require.NoError(t, err)
	require.NotNil(t, response)
	require.NotNil(t, response.JSON201)
	assert.Equal(t, http.StatusCreated, response.StatusCode)
	assert.Equal(t, int64(4294967295), response.JSON201.Checksum)
}
