package rawclient_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawClientEmptyPatchFinalizationValidatesStagedBytes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       []byte
		staged     []byte
		length     int64
		wantStatus int
		wantCode   string
	}{
		{name: "empty", wantStatus: http.StatusOK},
		{name: "resumed full object", body: []byte("abc"), staged: []byte("abc"), length: 3, wantStatus: http.StatusOK},
		{name: "before end", body: []byte("abc"), length: 3, wantStatus: http.StatusBadRequest, wantCode: rawclient.CodeInvalidRequest},
		{name: "wrong empty digest", body: []byte("abc"), wantStatus: http.StatusConflict, wantCode: rawclient.CodeChecksumMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uploads := newE2EUploadSessionStore()
			srv, auth := newE2ERawSyncServer(t, uploads)
			enrollment, err := auth.EnrollDevice(t.Context(), "tenant-e2e", "device-e2e")
			require.NoError(t, err)
			token, err := auth.IssueToken(t.Context(), enrollment.Identity.DeviceID, enrollment.Credential, rawsync.ScopeUpload)
			require.NoError(t, err)
			object := e2EObjectFor(t, tc.body)
			object.Length = tc.length
			now := time.Now().UTC()
			const uploadID = "upl_AQEBAQEBAQEBAQEBAQEBAQ"
			_, _, err = uploads.Create(t.Context(), rawsync.UploadSession{
				ID: uploadID, Identity: enrollment.Identity, Provider: parser.AgentClaude,
				Object: object, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			})
			require.NoError(t, err)
			_, err = uploads.Append(t.Context(), enrollment.Identity, uploadID, 0, tc.staged, now)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, srv.URL+"/api/v1/raw-sync/uploads/"+uploadID, http.NoBody)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+token.Token)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Upload-Offset", strconv.Itoa(len(tc.staged)))
			response, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer response.Body.Close()
			var result struct {
				Complete bool   `json:"complete"`
				Code     string `json:"code"`
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
			assert.Equal(t, tc.wantStatus, response.StatusCode)
			assert.Equal(t, tc.wantCode, result.Code)
			assert.Equal(t, tc.wantStatus == http.StatusOK, result.Complete)
			client, err := rawclient.NewClient(rawclient.Config{BaseURL: srv.URL, DeviceID: enrollment.Identity.DeviceID, Credential: enrollment.Credential, HTTPClient: srv.Client()})
			require.NoError(t, err)
			missing, err := client.MissingObjects(t.Context(), parser.AgentClaude, []rawsync.ObjectRef{object})
			require.NoError(t, err)
			if tc.wantStatus == http.StatusOK {
				assert.Empty(t, missing)
			} else {
				assert.Equal(t, []rawsync.ObjectRef{object}, missing)
			}
			if tc.name == "wrong empty digest" {
				session, err := uploads.Status(t.Context(), enrollment.Identity, uploadID, now)
				require.NoError(t, err)
				assert.Zero(t, session.Offset)
				assert.False(t, session.Complete,
					"a checksum mismatch must leave the persisted session incomplete")
			}
		})
	}
}
