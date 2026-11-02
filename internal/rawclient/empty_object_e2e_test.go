package rawclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawupload"
)

// Capture, upload, commit, and acknowledge through the real HTTP handlers.
// The zero case protects the empty finalizing PATCH; neighboring cases protect
// transfer completion on either side of a chunk boundary.
func TestRawClientCapturesAndAcknowledgesEmptyAndChunkBoundaryFiles(t *testing.T) {
	for _, length := range []int{0, 1, 7, 8, 9, 16, 17} {
		t.Run(strconv.Itoa(length), func(t *testing.T) {
			client, uploads, identity := emptyObjectClient(t, 8)
			base := t.TempDir()
			checkpoint, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(base, "checkpoint.db"), rawcheckpoint.Options{
				SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, checkpoint.Close()) })
			require.NoError(t, checkpoint.SetDevice(t.Context(), identity.DeviceID))
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			body := bytes.Repeat([]byte("x"), length)
			path := filepath.Join(root, "session.jsonl")
			require.NoError(t, os.WriteFile(path, body, 0o600))
			provider := &emptyObjectCaptureProvider{
				parser.ProviderBase{
					Def: parser.AgentDef{Type: parser.AgentClaude},
					Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
						Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeFiles,
						Append: parser.RawCaptureAppendOne, Snapshot: parser.RawCaptureSnapshotNone,
					}},
				},
				parser.RawCapturePlan{
					ConfiguredRoot: root, CaptureRoot: root, SourceKey: "session",
					Entries: []parser.RawCaptureEntry{{Path: "session.jsonl", LocalPath: path, Appendable: true}},
				},
			}
			capture, err := rawcapture.New(checkpoint).Capture(t.Context(), provider, parser.SourceRef{Provider: parser.AgentClaude, Key: "session"})
			require.NoError(t, err)
			require.Equal(t, rawcapture.StatusCaptured, capture.Status)
			queued, found, err := checkpoint.NextGeneration(t.Context())
			require.NoError(t, err)
			require.True(t, found)
			require.Len(t, queued.Entries, 1)
			require.Equal(t, int64(length), queued.Entries[0].Length)
			require.Equal(t, []rawsync.ObjectRef{e2EObjectFor(t, body)}, queued.Entries[0].Objects)

			uploader := rawupload.New(checkpoint, client, identity.DeviceID)
			result, found, err := uploader.UploadNext(t.Context())
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, capture.CaptureID, result.CaptureID)
			assert.NotEmpty(t, result.ManifestID)
			assert.NotEmpty(t, result.Receipt)
			assert.Equal(t, int64(1), result.Generation)
			assert.Equal(t, int64(length), result.UploadedBytes)
			head, found, err := checkpoint.SourceHead(t.Context(), capture.Source.Provider, capture.Source.ConfiguredRootID, capture.Source.SourceKey)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, result.ManifestID, head.ManifestID)
			assert.Equal(t, result.Receipt, head.Receipt)
			assert.Equal(t, int64(1), head.Generation)
			_, found, err = uploader.UploadNext(t.Context())
			require.NoError(t, err)
			assert.False(t, found, "acknowledged capture must leave the queue")
			missing, err := client.MissingObjects(t.Context(), parser.AgentClaude, queued.Entries[0].Objects)
			require.NoError(t, err)
			assert.Empty(t, missing, "acknowledgement requires verified physical custody")
			counts := uploads.appendCounts()
			require.Len(t, counts, 1)
			for _, count := range counts {
				assert.Equal(t, max(1, (length+7)/8), count)
			}
		})
	}
}

func emptyObjectClient(t *testing.T, chunkBytes int64) (*rawclient.Client, *e2EUploadSessionStore, rawsync.AuthIdentity) {
	t.Helper()
	config, uploads, identity := emptyObjectClientConfig(t, chunkBytes)
	client, err := rawclient.NewClient(config)
	require.NoError(t, err)
	return client, uploads, identity
}

func emptyObjectClientConfig(t *testing.T, chunkBytes int64) (rawclient.Config, *e2EUploadSessionStore, rawsync.AuthIdentity) {
	t.Helper()
	uploads := newE2EUploadSessionStore()
	srv, auth := newE2ERawSyncServer(t, uploads)
	enrollment, err := auth.EnrollDevice(t.Context(), "tenant-e2e", "device-e2e")
	require.NoError(t, err)
	return rawclient.Config{
		BaseURL: srv.URL, DeviceID: enrollment.Identity.DeviceID, Credential: enrollment.Credential,
		HTTPClient: srv.Client(), ChunkBytes: chunkBytes, TokenMargin: time.Minute,
	}, uploads, enrollment.Identity
}

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

type emptyObjectCaptureProvider struct {
	parser.ProviderBase
	plan parser.RawCapturePlan
}

func (p *emptyObjectCaptureProvider) Parse(context.Context, parser.ParseRequest) (parser.ParseOutcome, error) {
	panic("raw capture must not parse source bytes")
}

func (p *emptyObjectCaptureProvider) PlanRawCapture(context.Context, parser.SourceRef) (parser.RawCapturePlan, error) {
	return p.plan, nil
}
