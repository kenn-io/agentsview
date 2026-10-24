package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/vector"
)

func TestHubHTTPBoundedWire(t *testing.T) {
	for _, mode := range []string{"plain", "zstd", "oversize", "changed", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
				if mode == "changed" {
					w.WriteHeader(http.StatusConflict)
					return
				}
				var body []byte
				if r.Method == http.MethodGet {
					body = []byte(`{"fingerprint":"fp","model":"m","dimension":1,"include_automated":false}`)
				} else {
					var req vector.HubLookupRequest
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
						return
					}
					assert.Equal(t, "fp", req.Fingerprint)
					body = []byte(`{"version":"vectors.v1","fingerprint":"fp","chunks":[{"index":0,"chunk_index":0,"chunk_count":1,"vector":"AACAPw=="}],"absent":[]}`)
				}
				if mode == "malformed" {
					body = []byte(`{"private":"response-secret",`)
				}
				if mode == "oversize" {
					body = append(body, []byte(strings.Repeat(" ", vector.MaxHubLookupBytes))...)
				}
				if mode != "plain" {
					enc, err := zstd.NewWriter(nil)
					if !assert.NoError(t, err) {
						return
					}
					defer enc.Close()
					body = enc.EncodeAll(body, nil)
					w.Header().Set("Content-Encoding", "zstd")
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			hub := &httpHub{URL: server.URL, Token: "token", Client: server.Client()}
			gen, err := hub.HubVectorGeneration(t.Context())
			if mode == "malformed" {
				require.EqualError(t, err, "invalid hub vector response")
				return
			}
			if mode == "oversize" {
				require.ErrorContains(t, err, "exceeds 32 MiB")
				return
			}
			if mode == "changed" {
				require.ErrorIs(t, err, vector.ErrHubGenerationChanged)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "fp", gen.Fingerprint)
			reply, err := hub.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "fp", Identities: []vector.HubIdentity{{SessionID: "node~s", UnitKind: "user", ContentHash: "hash"}}})
			require.NoError(t, err)
			assert.Equal(t, vector.HubVectorVersion, reply.Version)
			assert.Equal(t, "AACAPw==", reply.Chunks[0].Vector)
		})
	}
}
