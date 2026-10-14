package rawclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/requestsign"
)

type locationCaptureTransport struct {
	next     http.RoundTripper
	location string
}

func (t *locationCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/raw-sync/uploads") {
		t.location = resp.Header.Get("Location")
	}
	return resp, err
}

func TestSignedRawSyncTLSRewritingPrefix(t *testing.T) {
	uploads := newE2EUploadSessionStore()
	app, auth := newE2ERawSyncApplication(t, uploads)
	enrolled, err := auth.EnrollDevice(t.Context(), "tenant-e2e", "device-e2e")
	require.NoError(t, err)
	var machine http.Handler
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.Clone(r.Context())
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/native")
		r.RequestURI = r.URL.RequestURI()
		// Spoofed proxy authority must never replace the configured external URI.
		r.Header.Set("X-Forwarded-Host", "attacker.example")
		r.Header.Set("X-Forwarded-Proto", "http")
		machine.ServeHTTP(w, r)
	}))
	defer ts.Close()
	dir := t.TempDir()
	file := filepath.Join(dir, "key")
	require.NoError(t, requestsign.GenerateSecretFile(file))
	secret, err := requestsign.ReadSecret(file)
	require.NoError(t, err)
	replay := filepath.Join(dir, "replay.db")
	require.NoError(t, requestsign.InitReplay(replay))
	policy := requestsign.Policy{ExternalURL: ts.URL + "/native", StripPrefix: true, ReplayDB: replay, Keys: []requestsign.PolicyKey{{ID: "device-key", File: file, Grant: "contributor", DeviceID: enrolled.Identity.DeviceID}}}
	policyPath := filepath.Join(dir, "policy.json")
	writePolicy := func() {
		data, err := json.Marshal(policy)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(policyPath, data, 0o600))
	}
	writePolicy()
	machine, closeState, err := app.RestrictedHandler(policyPath)
	require.NoError(t, err)
	defer closeState()
	signer, err := requestsign.NewSigner(ts.URL+"/native", requestsign.Key{ID: "device-key", Secret: secret})
	require.NoError(t, err)
	capture := &locationCaptureTransport{next: ts.Client().Transport}
	httpClient, err := signer.Client(&http.Client{Transport: capture})
	require.NoError(t, err)
	client, err := NewClient(Config{BaseURL: ts.URL + "/native", DeviceID: enrolled.Identity.DeviceID, Credential: enrolled.Credential, HTTPClient: httpClient, ChunkBytes: 8})
	require.NoError(t, err)
	body := []byte("signed raw object with multiple resumable chunks")
	object := e2EObjectFor(t, body)
	missing, err := client.MissingObjects(t.Context(), parser.AgentClaude, []rawsync.ObjectRef{object})
	require.NoError(t, err)
	assert.Equal(t, []rawsync.ObjectRef{object}, missing)
	require.NoError(t, client.UploadObject(t.Context(), parser.AgentClaude, object, bytes.NewReader(body)))
	assert.Regexp(t, `^/native/api/v1/raw-sync/uploads/upl_`, capture.location)
	// Reconnect with fresh client/token; a completed object is reused idempotently.
	resumed, err := NewClient(Config{BaseURL: ts.URL + "/native", DeviceID: enrolled.Identity.DeviceID, Credential: enrolled.Credential, HTTPClient: httpClient, ChunkBytes: 8})
	require.NoError(t, err)
	require.NoError(t, resumed.UploadObject(t.Context(), parser.AgentClaude, object, bytes.NewReader(body)))
	manifest := rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parser.AgentClaude, ConfiguredRootID: "fixture-root", SourceKey: "session.jsonl", CaptureID: "signed-capture", CapturedAt: time.Now().UTC(), Kind: rawsync.ManifestSnapshot, Entries: []rawsync.Entry{{Path: "session.jsonl", Type: "file", Length: object.Length, Objects: []rawsync.ObjectRef{object}}}}
	committed, err := resumed.CommitManifest(t.Context(), manifest)
	require.NoError(t, err)
	assert.NotEmpty(t, committed.Receipt)
	again, err := resumed.CommitManifest(t.Context(), manifest)
	require.NoError(t, err)
	assert.Equal(t, committed.Receipt, again.Receipt)
	assert.False(t, again.Created)
	// Signature validity never adds missing native token scope.
	issued, err := auth.IssueToken(t.Context(), enrolled.Identity.DeviceID, enrolled.Credential, rawsync.ScopeStatus)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/native/api/v1/raw-sync/objects/missing", strings.NewReader(`{"provider":"claude","objects":[]}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+issued.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 401, resp.StatusCode)
	// Bind the signing grant to a different device while native auth still succeeds.
	policy.Keys[0].DeviceID = "different-device"
	writePolicy()
	_, err = resumed.MissingObjects(t.Context(), parser.AgentClaude, []rawsync.ObjectRef{object})
	require.Error(t, err)
	var denied APIError
	require.True(t, AsAPIError(err, &denied))
	assert.Equal(t, 403, denied.Status)
	policy.Keys[0].DeviceID = enrolled.Identity.DeviceID
	writePolicy()
	// Remove/revoke the old key: no cached server key remains usable.
	policy.Keys[0].ID = "replacement-key"
	writePolicy()
	_, err = resumed.MissingObjects(t.Context(), parser.AgentClaude, []rawsync.ObjectRef{object})
	require.Error(t, err)
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/native/api/v1/version", nil)
	require.NoError(t, err)
	unsigned, err := ts.Client().Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, unsigned.Body)
	require.NoError(t, err)
	require.NoError(t, unsigned.Body.Close())
	assert.Equal(t, 401, unsigned.StatusCode)
}
