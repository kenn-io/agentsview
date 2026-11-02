//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package rawclient_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawsync"
)

// Hegel ships its native engine for the targets listed in this file's build
// constraint. The capture and HTTP examples run on every supported platform.
// Upload completion must imply custody for arbitrary object and chunk lengths.
func TestRawClientUploadCustodyProperty(t *testing.T) {
	config, _, _ := emptyObjectClientConfig(t, 8)
	hegel.Test(t, func(ht *hegel.T) {
		// Bound materialized data while sampling all length bit patterns.
		length := hegel.Draw(ht, hegel.Integers[uint16](0, ^uint16(0))) % 257
		value := hegel.Draw(ht, hegel.Integers[byte](0, 255))
		config.ChunkBytes = int64(1 + hegel.Draw(ht, hegel.Integers[byte](0, 255))%32)
		client, err := rawclient.NewClient(config)
		require.NoError(ht, err)
		body := bytes.Repeat([]byte{value}, int(length))
		object := e2EObjectFor(ht.T, body)
		require.NoError(ht, client.UploadObject(t.Context(), parser.AgentClaude, object, bytes.NewReader(body)))
		missing, err := client.MissingObjects(t.Context(), parser.AgentClaude, []rawsync.ObjectRef{object})
		require.NoError(ht, err)
		assert.Empty(ht, missing)
	}, hegel.WithTestCases(1000))
}

func TestRawClientEmptyPatchCannotFinalizeIncompleteObjectProperty(t *testing.T) {
	patch := newEmptyPatchPropertyServer(t)
	hegel.Test(t, func(ht *hegel.T) {
		body := hegel.Draw(ht, hegel.Binary(1, 128))
		offset := hegel.Draw(ht, hegel.Integers(0, len(body)-1))
		object := e2EObjectFor(ht.T, body)
		response, session := patch.finalizeEmpty(ht, object, body[:offset])
		defer response.Body.Close()
		assert.Equal(ht, http.StatusBadRequest, response.StatusCode)
		var failure rawclient.APIError
		require.NoError(ht, json.NewDecoder(response.Body).Decode(&failure))
		assert.Equal(ht, rawclient.CodeInvalidRequest, failure.Code)
		assert.Equal(ht, int64(offset), session.Offset)
		assert.False(ht, session.Complete)
		patch.requireMissing(ht, object)
	}, hegel.WithTestCases(1000))
}

type emptyPatchPropertyServer struct {
	server   *httptest.Server
	uploads  *e2EUploadSessionStore
	client   *rawclient.Client
	identity rawsync.AuthIdentity
	token    string
	sequence int
}

func newEmptyPatchPropertyServer(t *testing.T) *emptyPatchPropertyServer {
	t.Helper()
	uploads := newE2EUploadSessionStore()
	srv, auth := newE2ERawSyncServer(t, uploads)
	enrollment, err := auth.EnrollDevice(t.Context(), "tenant-e2e", "device-e2e")
	require.NoError(t, err)
	token, err := auth.IssueToken(t.Context(), enrollment.Identity.DeviceID, enrollment.Credential, rawsync.ScopeUpload)
	require.NoError(t, err)
	client, err := rawclient.NewClient(rawclient.Config{
		BaseURL: srv.URL, DeviceID: enrollment.Identity.DeviceID, Credential: enrollment.Credential,
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	return &emptyPatchPropertyServer{server: srv, uploads: uploads, client: client, identity: enrollment.Identity, token: token.Token}
}

func (p *emptyPatchPropertyServer) finalizeEmpty(ht *hegel.T, object rawsync.ObjectRef, staged []byte) (*http.Response, rawsync.UploadSession) {
	ht.Helper()
	p.sequence++
	id := sha256.Sum256([]byte(strconv.Itoa(p.sequence)))
	uploadID := "upl_" + base64.RawURLEncoding.EncodeToString(id[:16])
	now := time.Now().UTC()
	_, _, err := p.uploads.Create(ht.Context(), rawsync.UploadSession{
		ID: uploadID, Identity: p.identity, Provider: parser.AgentClaude, Object: object,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(ht, err)
	_, err = p.uploads.Append(ht.Context(), p.identity, uploadID, 0, staged, now)
	require.NoError(ht, err)
	req, err := http.NewRequestWithContext(ht.Context(), http.MethodPatch, p.server.URL+"/api/v1/raw-sync/uploads/"+uploadID, http.NoBody)
	require.NoError(ht, err)
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Upload-Offset", strconv.Itoa(len(staged)))
	response, err := p.server.Client().Do(req)
	require.NoError(ht, err)
	session, err := p.uploads.Status(ht.Context(), p.identity, uploadID, now)
	require.NoError(ht, err)
	return response, session
}

func (p *emptyPatchPropertyServer) requireMissing(ht *hegel.T, object rawsync.ObjectRef) {
	ht.Helper()
	missing, err := p.client.MissingObjects(ht.Context(), parser.AgentClaude, []rawsync.ObjectRef{object})
	require.NoError(ht, err)
	assert.Equal(ht, []rawsync.ObjectRef{object}, missing)
}
