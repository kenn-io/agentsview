package rawclient

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

// rawHTTPTestManifest builds one valid snapshot manifest with a unique
// capture ID per call, so parallel scripted commits never collide.
func rawHTTPTestManifest() rawsync.Manifest {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic("rawclient: reading capture nonce: " + err.Error())
	}
	return rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         parser.AgentClaude,
		ConfiguredRootID: "av_root_test",
		SourceKey:        "~/.claude/projects",
		CaptureID:        hex.EncodeToString(nonce[:]),
		CapturedAt:       time.Now().UTC().Round(0),
		Kind:             rawsync.ManifestSnapshot,
		Entries: []rawsync.Entry{{
			Path:   "session-01.jsonl",
			Type:   "file",
			Length: 3,
			Objects: []rawsync.ObjectRef{{
				SHA256: "aa00000000000000000000000000000000000000000000000000000000000000",
				Length: 3,
			}},
		}},
	}
}

func TestCommitManifestDecodesReceipt(t *testing.T) {
	t.Parallel()
	manifest := rawHTTPTestManifest()
	manifestID := strings.Repeat("a", 64)
	receipt := strings.Repeat("b", 64)
	server := httptest.NewServer(withTokenRoute(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler goroutines use assert, never require/FailNow.
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/raw-sync/manifests", r.URL.Path)
		var sent rawsync.Manifest
		if assert.NoError(t, jsonDecode(r.Body, &sent)) {
			assert.Equal(t, manifest, sent)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"manifest_id":%q,"receipt":%q,"generation":2,"created":true}`,
			manifestID, receipt)
	})))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	result, err := client.CommitManifest(t.Context(), manifest)
	require.NoError(t, err)
	assert.Equal(t, manifestID, result.ManifestID)
	assert.Equal(t, receipt, result.Receipt)
	assert.Equal(t, int64(2), result.Generation)
	assert.True(t, result.Created)
}

func TestCommitManifestRejectsInvalidResult(t *testing.T) {
	t.Parallel()
	validManifestID := strings.Repeat("a", 64)
	validReceipt := strings.Repeat("b", 64)
	tests := []struct {
		name       string
		manifestID string
		receipt    string
		generation int64
		want       string
	}{
		{
			name:       "noncanonical manifest ID",
			manifestID: "rm_1",
			receipt:    validReceipt,
			generation: 1,
			want:       "manifest ID",
		},
		{
			name:       "uppercase receipt",
			manifestID: validManifestID,
			receipt:    strings.Repeat("B", 64),
			generation: 1,
			want:       "receipt",
		},
		{
			name:       "zero generation",
			manifestID: validManifestID,
			receipt:    validReceipt,
			generation: 0,
			want:       "generation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(withTokenRoute(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w,
					`{"manifest_id":%q,"receipt":%q,"generation":%d,"created":true}`,
					tt.manifestID, tt.receipt, tt.generation)
			})))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL, time.Minute)

			result, err := client.CommitManifest(t.Context(), rawHTTPTestManifest())
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
			assert.Equal(t, rawsync.CommitResult{}, result)
		})
	}
}

func TestCommitManifestSurfacesHeadConflict(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(withTokenRoute(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler goroutines use assert, never require/FailNow.
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/raw-sync/manifests", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"code":"head_conflict","error":"raw source head changed",`+
			`"current_manifest_id":"rm_9","current_receipt":"rr_9","current_generation":7}`)
	})))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	_, err := client.CommitManifest(t.Context(), rawHTTPTestManifest())
	require.Error(t, err)
	var apiErr APIError
	require.True(t, AsAPIError(err, &apiErr))
	assert.Equal(t, http.StatusConflict, apiErr.Status)
	assert.Equal(t, CodeHeadConflict, apiErr.Code)
	assert.Equal(t, "rm_9", apiErr.CurrentManifestID)
	assert.Equal(t, "rr_9", apiErr.CurrentReceipt)
	assert.Equal(t, int64(7), apiErr.CurrentGeneration)
}

func TestCommitManifestPassesMissingObjectThrough(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(withTokenRoute(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler goroutines use assert, never require/FailNow.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"code":"missing_object","error":"manifest references missing object"}`)
	})))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	_, err := client.CommitManifest(t.Context(), rawHTTPTestManifest())
	require.Error(t, err)
	var apiErr APIError
	require.True(t, AsAPIError(err, &apiErr))
	assert.Equal(t, CodeMissingObject, apiErr.Code)
}

func TestCommitManifestRejectsMissingReceipt(t *testing.T) {
	t.Parallel()
	manifestID := strings.Repeat("a", 64)
	server := httptest.NewServer(withTokenRoute(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler goroutines use assert, never require/FailNow.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"manifest_id":%q,"receipt":"","generation":3,"created":true}`,
			manifestID)
	})))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	result, err := client.CommitManifest(t.Context(), rawHTTPTestManifest())
	require.Error(t, err)
	assert.ErrorContains(t, err, "missing receipt")
	assert.Equal(t, rawsync.CommitResult{}, result)
}
