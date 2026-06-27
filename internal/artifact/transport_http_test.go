package artifact

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPTransportPostArtifactSetsPeerOrigin(t *testing.T) {
	var gotOrigin string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrigin = r.Header.Get("Origin")
		w.WriteHeader(http.StatusCreated)
	}))
	defer peer.Close()

	tr, err := newHTTPTransport(peer.URL+"/api/v1/artifacts", "")
	require.NoError(t, err)

	err = tr.postArtifact(context.Background(), "peer-a1b2c3", KindSegments, strings64("a"), []byte("artifact"))
	require.NoError(t, err)

	assert.Equal(t, peer.URL, gotOrigin)
}
