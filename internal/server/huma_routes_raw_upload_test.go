package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawUploadStartUsesScopedIdentityAndReturnsResumeState(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	object := rawHTTPUploadObject(t, []byte("hello world"))
	auth := rawUploadAuthStub(t, identity)
	uploads := &rawSyncUploadsStub{
		start: func(
			_ context.Context,
			gotIdentity rawsync.AuthIdentity,
			provider parser.AgentType,
			gotObject rawsync.ObjectRef,
		) (rawsync.UploadSession, bool, error) {
			assert.Equal(t, identity, gotIdentity)
			assert.Equal(t, parser.AgentCodex, provider)
			assert.Equal(t, object, gotObject)
			return rawsync.UploadSession{
				ID: "upl_AQEBAQEBAQEBAQEBAQEBAQ", Identity: identity,
				Provider: provider, Object: object, Offset: 5,
				CreatedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
				ExpiresAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
			}, false, nil
		},
	}
	srv := newRawUploadHTTPTestServer(t, auth, uploads)
	body, err := json.Marshal(map[string]any{
		"provider": parser.AgentCodex,
		"object":   object,
	})
	require.NoError(t, err)

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/api/v1/raw-sync/uploads",
		string(body), "avdt_upload", "",
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response rawSyncUploadResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "upl_AQEBAQEBAQEBAQEBAQEBAQ", response.UploadID)
	assert.Equal(t, object, response.Object)
	assert.Equal(t, int64(5), response.Offset)
	assert.False(t, response.Complete)
	assert.False(t, response.Created)
	assert.Equal(t, 1, uploads.startCalls)
}

func TestRawUploadStartLocationIncludesBasePath(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	uploadID := "upl_AQEBAQEBAQEBAQEBAQEBAQ"
	object := rawHTTPUploadObject(t, []byte("hello world"))
	uploads := &rawSyncUploadsStub{
		start: func(
			context.Context,
			rawsync.AuthIdentity,
			parser.AgentType,
			rawsync.ObjectRef,
		) (rawsync.UploadSession, bool, error) {
			return rawsync.UploadSession{
				ID: uploadID, Identity: identity, Provider: parser.AgentCodex,
				Object:    object,
				CreatedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
				ExpiresAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
			}, true, nil
		},
	}
	srv := newRawUploadHTTPTestServer(
		t, rawUploadAuthStub(t, identity), uploads, WithBasePath("/viewer"),
	)
	body, err := json.Marshal(map[string]any{
		"provider": parser.AgentCodex,
		"object":   object,
	})
	require.NoError(t, err)

	recorder := serveRawSyncJSON(
		t, srv, http.MethodPost, "/viewer/api/v1/raw-sync/uploads",
		string(body), "avdt_upload", "",
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, "/viewer/api/v1/raw-sync/uploads/"+uploadID,
		recorder.Header().Get("Location"))
}

func TestRawUploadHeadReportsAuthoritativeOffset(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	uploadID := "upl_AQEBAQEBAQEBAQEBAQEBAQ"
	object := rawHTTPUploadObject(t, []byte("hello world"))
	uploads := &rawSyncUploadsStub{
		status: func(
			_ context.Context,
			gotIdentity rawsync.AuthIdentity,
			gotUploadID string,
		) (rawsync.UploadSession, error) {
			assert.Equal(t, identity, gotIdentity)
			assert.Equal(t, uploadID, gotUploadID)
			return rawsync.UploadSession{
				ID: uploadID, Identity: identity, Provider: parser.AgentCodex,
				Object: object, Offset: 5,
				CreatedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
				ExpiresAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	srv := newRawUploadHTTPTestServer(t, rawUploadAuthStub(t, identity), uploads)

	recorder := serveRawUploadChunk(
		t, srv, http.MethodHead, "/api/v1/raw-sync/uploads/"+uploadID,
		"avdt_upload", 0, nil,
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Empty(t, recorder.Body.String())
	assert.Equal(t, "5", recorder.Header().Get(rawSyncUploadOffsetHeader))
	assert.Equal(t, strconv.FormatInt(object.Length, 10),
		recorder.Header().Get(rawSyncUploadLengthHeader))
	assert.Equal(t, "false", recorder.Header().Get(rawSyncUploadCompleteHeader))
	assert.Equal(t, 1, uploads.statusCalls)
}

func TestRawUploadPatchAppendsOpaqueChunkAndReturnsCompletion(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	uploadID := "upl_AQEBAQEBAQEBAQEBAQEBAQ"
	chunk := []byte{0, 1, 2, 3}
	object := rawHTTPUploadObject(t, chunk)
	uploads := &rawSyncUploadsStub{
		appendChunk: func(
			_ context.Context,
			gotIdentity rawsync.AuthIdentity,
			gotUploadID string,
			offset int64,
			gotChunk []byte,
		) (rawsync.UploadSession, error) {
			assert.Equal(t, identity, gotIdentity)
			assert.Equal(t, uploadID, gotUploadID)
			assert.Zero(t, offset)
			assert.Equal(t, chunk, gotChunk)
			return rawsync.UploadSession{
				ID: uploadID, Identity: identity, Provider: parser.AgentCodex,
				Object: object, Offset: object.Length, Complete: true,
				CreatedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
				ExpiresAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	srv := newRawUploadHTTPTestServer(t, rawUploadAuthStub(t, identity), uploads)

	recorder := serveRawUploadChunk(
		t, srv, http.MethodPatch, "/api/v1/raw-sync/uploads/"+uploadID,
		"avdt_upload", 0, chunk,
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, strconv.FormatInt(object.Length, 10),
		recorder.Header().Get(rawSyncUploadOffsetHeader))
	assert.Equal(t, "true", recorder.Header().Get(rawSyncUploadCompleteHeader))
	var response rawSyncUploadResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Complete)
	assert.Equal(t, object.Length, response.Offset)
	assert.Equal(t, 1, uploads.appendCalls)
}

func TestRawUploadPatchIsNotWrappedByShortWriteTimeout(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	uploadID := "upl_AQEBAQEBAQEBAQEBAQEBAQ"
	object := rawHTTPUploadObject(t, []byte("chunk"))
	uploads := &rawSyncUploadsStub{
		appendChunk: func(
			ctx context.Context,
			_ rawsync.AuthIdentity,
			_ string,
			_ int64,
			_ []byte,
		) (rawsync.UploadSession, error) {
			time.Sleep(50 * time.Millisecond)
			_, hasDeadline := ctx.Deadline()
			assert.False(t, hasDeadline)
			assert.NoError(t, ctx.Err())
			return rawsync.UploadSession{
				ID: uploadID, Identity: identity, Provider: parser.AgentCodex,
				Object: object, Offset: object.Length, Complete: true,
				CreatedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
				ExpiresAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	srv := New(config.Config{
		Host: "127.0.0.1", Port: 8080, AuthToken: "legacy-shared-token",
		RequireAuth: true, WriteTimeout: 10 * time.Millisecond,
	}, nil, nil,
		WithRawSyncServices(rawUploadAuthStub(t, identity), new(rawSyncCustodyStub)),
		WithRawSyncUploads(uploads),
	)

	recorder := serveRawUploadChunk(
		t, srv, http.MethodPatch, "/api/v1/raw-sync/uploads/"+uploadID,
		"avdt_upload", 0, []byte("chunk"),
	)

	assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}

func TestRawUploadPatchExtendsRealServerReadDeadline(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	uploadID := "upl_AQEBAQEBAQEBAQEBAQEBAQ"
	chunk := []byte("chunk")
	object := rawHTTPUploadObject(t, chunk)
	uploads := &rawSyncUploadsStub{
		appendChunk: func(
			_ context.Context,
			_ rawsync.AuthIdentity,
			_ string,
			_ int64,
			gotChunk []byte,
		) (rawsync.UploadSession, error) {
			assert.Equal(t, chunk, gotChunk)
			return rawsync.UploadSession{
				ID: uploadID, Identity: identity, Provider: parser.AgentCodex,
				Object: object, Offset: object.Length, Complete: true,
				CreatedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
				ExpiresAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	srv := newRawUploadHTTPTestServer(t, rawUploadAuthStub(t, identity), uploads)
	srv.httpReadTimeout = 50 * time.Millisecond
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
		err := <-serveErr
		require.True(t, errors.Is(err, http.ErrServerClosed), "Serve returned %v", err)
	})

	reader, writer := io.Pipe()
	request, err := http.NewRequest(
		http.MethodPatch,
		"http://"+listener.Addr().String()+"/api/v1/raw-sync/uploads/"+uploadID,
		reader,
	)
	require.NoError(t, err)
	request.ContentLength = int64(len(chunk))
	request.Header.Set("Authorization", "Bearer avdt_upload")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set(rawSyncUploadOffsetHeader, "0")
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		result, err := http.DefaultClient.Do(request)
		if err != nil {
			requestErr <- err
			return
		}
		response <- result
	}()
	require.NoError(t, writeSlowUploadBody(writer, chunk, 150*time.Millisecond))

	select {
	case err := <-requestErr:
		t.Fatalf("slow upload request failed: %v", err)
	case result := <-response:
		defer result.Body.Close()
		assert.Equal(t, http.StatusOK, result.StatusCode)
	case <-time.After(3 * time.Second):
		t.Fatal("slow upload request did not finish")
	}
}

func writeSlowUploadBody(writer *io.PipeWriter, body []byte, delay time.Duration) error {
	if _, err := writer.Write(body[:1]); err != nil {
		return err
	}
	time.Sleep(delay)
	if _, err := writer.Write(body[1:]); err != nil {
		return err
	}
	return writer.Close()
}

func TestRawUploadOffsetConflictReturnsResumeOffset(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	uploads := &rawSyncUploadsStub{
		appendChunk: func(
			context.Context,
			rawsync.AuthIdentity,
			string,
			int64,
			[]byte,
		) (rawsync.UploadSession, error) {
			return rawsync.UploadSession{}, &rawsync.UploadOffsetConflictError{
				CurrentOffset: 5,
			}
		},
	}
	srv := newRawUploadHTTPTestServer(t, rawUploadAuthStub(t, identity), uploads)

	recorder := serveRawUploadChunk(
		t, srv, http.MethodPatch,
		"/api/v1/raw-sync/uploads/upl_AQEBAQEBAQEBAQEBAQEBAQ",
		"avdt_upload", 0, []byte("chunk"),
	)

	require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	var response apiErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "upload_offset_conflict", response.Code)
	require.NotNil(t, response.CurrentUploadOffset)
	assert.Equal(t, int64(5), *response.CurrentUploadOffset)
}

func TestRawUploadChecksumMismatchReturnsResetOffset(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	uploads := &rawSyncUploadsStub{
		appendChunk: func(
			context.Context,
			rawsync.AuthIdentity,
			string,
			int64,
			[]byte,
		) (rawsync.UploadSession, error) {
			return rawsync.UploadSession{}, &rawsync.UploadChecksumMismatchError{
				CurrentOffset: 0,
			}
		},
	}
	srv := newRawUploadHTTPTestServer(t, rawUploadAuthStub(t, identity), uploads)

	recorder := serveRawUploadChunk(
		t, srv, http.MethodPatch,
		"/api/v1/raw-sync/uploads/upl_AQEBAQEBAQEBAQEBAQEBAQ",
		"avdt_upload", 0, []byte("chunk"),
	)

	require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	var response apiErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "checksum_mismatch", response.Code)
	require.NotNil(t, response.CurrentUploadOffset)
	assert.Zero(t, *response.CurrentUploadOffset)
}

func TestRawUploadBoundsChunkBeforeService(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	uploads := &rawSyncUploadsStub{
		appendChunk: func(
			context.Context,
			rawsync.AuthIdentity,
			string,
			int64,
			[]byte,
		) (rawsync.UploadSession, error) {
			require.FailNow(t, "oversized chunk must not reach upload service")
			return rawsync.UploadSession{}, nil
		},
	}
	srv := newRawUploadHTTPTestServer(t, rawUploadAuthStub(t, identity), uploads)

	recorder := serveRawUploadChunk(
		t, srv, http.MethodPatch,
		"/api/v1/raw-sync/uploads/upl_AQEBAQEBAQEBAQEBAQEBAQ",
		"avdt_upload", 0, bytes.Repeat([]byte{1}, int(rawsync.DefaultUploadChunkBytes+1)),
	)

	assert.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code, recorder.Body.String())
	assert.Zero(t, uploads.appendCalls)
}

func TestRawUploadCORSAllowsAndExposesResumeHeaders(t *testing.T) {
	t.Parallel()

	identity := rawsync.AuthIdentity{TenantID: "tenant-auth", DeviceID: "device-auth"}
	uploadID := "upl_AQEBAQEBAQEBAQEBAQEBAQ"
	object := rawHTTPUploadObject(t, []byte("hello world"))
	uploads := &rawSyncUploadsStub{
		status: func(
			context.Context,
			rawsync.AuthIdentity,
			string,
		) (rawsync.UploadSession, error) {
			return rawsync.UploadSession{
				ID: uploadID, Identity: identity, Provider: parser.AgentCodex,
				Object:    object,
				CreatedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
				ExpiresAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	srv := newRawUploadHTTPTestServer(t, rawUploadAuthStub(t, identity), uploads)
	preflight := httptest.NewRequest(
		http.MethodOptions, "/api/v1/raw-sync/uploads/"+uploadID, nil,
	)
	preflight.Host = "127.0.0.1:8080"
	preflight.RemoteAddr = "192.0.2.10:54321"
	preflight.Header.Set("Origin", "https://client.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPatch)
	preflight.Header.Set(
		"Access-Control-Request-Headers", "authorization, content-type, upload-offset",
	)
	preflightRecorder := httptest.NewRecorder()

	srv.Handler().ServeHTTP(preflightRecorder, preflight)

	require.Equal(t, http.StatusNoContent, preflightRecorder.Code)
	assert.Contains(t,
		preflightRecorder.Header().Get("Access-Control-Allow-Headers"),
		rawSyncUploadOffsetHeader,
	)

	req := httptest.NewRequest(http.MethodHead, "/api/v1/raw-sync/uploads/"+uploadID, nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.0.2.10:54321"
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Authorization", "Bearer avdt_upload")
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t,
		recorder.Header().Get("Access-Control-Expose-Headers"),
		rawSyncUploadOffsetHeader,
	)
}

func rawUploadAuthStub(
	t *testing.T,
	identity rawsync.AuthIdentity,
) *rawSyncAuthStub {
	t.Helper()
	return &rawSyncAuthStub{
		authenticateToken: func(
			_ context.Context,
			token string,
			required rawsync.DeviceTokenScope,
		) (rawsync.AuthIdentity, error) {
			assert.Equal(t, "avdt_upload", token)
			assert.Equal(t, rawsync.ScopeUpload, required)
			return identity, nil
		},
	}
}

func newRawUploadHTTPTestServer(
	t *testing.T,
	auth RawSyncDeviceAuth,
	uploads RawSyncUploads,
	options ...Option,
) *Server {
	t.Helper()
	options = append([]Option{
		WithRawSyncServices(auth, new(rawSyncCustodyStub)),
		WithRawSyncUploads(uploads),
	}, options...)
	return New(config.Config{
		Host: "127.0.0.1", Port: 8080,
		AuthToken: "legacy-shared-token", RequireAuth: true,
		WriteTimeout: 30 * time.Second,
	}, nil, nil, options...)
}

func serveRawUploadChunk(
	t *testing.T,
	srv *Server,
	method string,
	path string,
	bearer string,
	offset int64,
	body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.0.2.10:54321"
	req.Header.Set("Authorization", "Bearer "+bearer)
	if method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set(rawSyncUploadOffsetHeader, strconv.FormatInt(offset, 10))
	}
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, req)
	return recorder
}

func rawHTTPUploadObject(t *testing.T, body []byte) rawsync.ObjectRef {
	t.Helper()
	manifest := rawHTTPTestManifest()
	manifest.Entries[0].Objects[0].Length = int64(len(body))
	manifest.Entries[0].Length = int64(len(body))
	object := manifest.Entries[0].Objects[0]
	digest := sha256.Sum256(body)
	object.SHA256 = hex.EncodeToString(digest[:])
	return object
}

type rawSyncUploadsStub struct {
	start func(
		context.Context,
		rawsync.AuthIdentity,
		parser.AgentType,
		rawsync.ObjectRef,
	) (rawsync.UploadSession, bool, error)
	status func(
		context.Context,
		rawsync.AuthIdentity,
		string,
	) (rawsync.UploadSession, error)
	appendChunk func(
		context.Context,
		rawsync.AuthIdentity,
		string,
		int64,
		[]byte,
	) (rawsync.UploadSession, error)
	startCalls  int
	statusCalls int
	appendCalls int
}

func (s *rawSyncUploadsStub) Start(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	provider parser.AgentType,
	object rawsync.ObjectRef,
) (rawsync.UploadSession, bool, error) {
	s.startCalls++
	return s.start(ctx, identity, provider, object)
}

func (s *rawSyncUploadsStub) Status(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
) (rawsync.UploadSession, error) {
	s.statusCalls++
	return s.status(ctx, identity, uploadID)
}

func (s *rawSyncUploadsStub) Append(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	offset int64,
	chunk []byte,
) (rawsync.UploadSession, error) {
	s.appendCalls++
	return s.appendChunk(ctx, identity, uploadID, offset, chunk)
}

var _ RawSyncUploads = (*rawSyncUploadsStub)(nil)
