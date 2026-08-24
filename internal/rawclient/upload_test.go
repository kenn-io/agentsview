package rawclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

// withTokenRoute mounts the device-credential token exchange next to a data
// route so newTestClient's authenticated calls succeed. The issued avdt token
// outlives every test here.
func withTokenRoute(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/raw-sync/tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":"avdt_up","device_id":"dev_test",`+
			`"scopes":["negotiate","upload","commit"],`+
			`"expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
	})
	mux.Handle("/", next)
	return mux
}

func TestMissingObjectsRoundTrip(t *testing.T) {
	t.Parallel()
	digest := "aa00000000000000000000000000000000000000000000000000000000000000"
	mux := withTokenRoute(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler goroutines use assert, never require/FailNow.
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/raw-sync/objects/missing", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		if assert.NoError(t, err) {
			assert.Equal(t, `{"provider":"claude","objects":[`+
				`{"sha256":"`+digest+`","length":3}]}`, string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"missing":[{"sha256":"`+digest+`","length":3}]}`)
	}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	missing, err := client.MissingObjects(t.Context(), parser.AgentClaude,
		[]rawsync.ObjectRef{{SHA256: digest, Length: 3}})
	require.NoError(t, err)
	require.Len(t, missing, 1)
	assert.Equal(t, digest, missing[0].SHA256)
	assert.Equal(t, int64(3), missing[0].Length)
}

// uploadScript is a scripted resumable-upload backend. startOffset is what
// the POST reports; offset is the authoritative offset the PATCH route
// defends — keeping them distinct lets a conflict response rewind a client
// that raced ahead. lieInBody makes successful PATCH bodies carry stale
// progress so header preference is observable. postIncomplete makes the
// POST report an incomplete session even at the full offset; deferComplete
// holds the completion report until an empty finalization PATCH arrives;
// neverComplete makes the PATCH route refuse to ever complete.
type uploadScript struct {
	mu             sync.Mutex
	startOffset    int64
	offset         int64
	length         int64
	patchBytes     [][]byte
	failFirst      string // "offset_conflict" | "checksum_mismatch"
	lieInBody      bool
	postIncomplete bool
	deferComplete  bool
	neverComplete  bool
}

func (s *uploadScript) handler(t *testing.T) http.Handler {
	return withTokenRoute(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Handler goroutines use assert, never require/FailNow.
		switch {
		case r.URL.Path == "/api/v1/raw-sync/uploads" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			complete := s.offset >= s.length
			if s.postIncomplete {
				complete = false
			}
			fmt.Fprintf(w, `{"upload_id":"up_1","offset":%d,"complete":%t}`,
				s.startOffset, complete)
		case r.URL.Path == "/api/v1/raw-sync/uploads/up_1" && r.Method == http.MethodPatch:
			assert.NotEmpty(t, r.Header.Get("Authorization"))
			assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
			offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
			if !assert.NoError(t, err) {
				http.Error(w, "bad offset header", http.StatusInternalServerError)
				return
			}
			body, err := io.ReadAll(r.Body)
			if !assert.NoError(t, err) {
				http.Error(w, "bad body", http.StatusInternalServerError)
				return
			}
			if s.failFirst == "offset_conflict" {
				s.failFirst = ""
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				fmt.Fprintf(w, `{"code":"upload_offset_conflict",`+
					`"error":"raw upload offset changed","upload_offset":%d}`, s.offset)
				return
			}
			if s.failFirst == "checksum_mismatch" {
				s.failFirst = ""
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				fmt.Fprintf(w, `{"code":"checksum_mismatch",`+
					`"error":"raw upload checksum did not match","upload_offset":0}`)
				return
			}
			if !assert.Equal(t, s.offset, offset,
				"PATCH must target the server-confirmed offset") {
				http.Error(w, "offset mismatch", http.StatusConflict)
				return
			}
			s.patchBytes = append(s.patchBytes, body)
			s.offset += int64(len(body))
			complete := s.offset >= s.length
			if s.deferComplete && len(body) > 0 {
				// Finalization answers only the empty finalization PATCH.
				complete = false
			}
			if s.neverComplete {
				complete = false
			}
			w.Header().Set("Upload-Offset", strconv.FormatInt(s.offset, 10))
			w.Header().Set("Upload-Complete", strconv.FormatBool(complete))
			if s.lieInBody {
				// Stale body: only the headers carry the truth.
				fmt.Fprintf(w, `{"upload_id":"up_1","offset":0,"complete":false}`)
				return
			}
			fmt.Fprintf(w, `{"upload_id":"up_1","offset":%d,"complete":%t}`,
				s.offset, complete)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
}

// newUploadObject builds a validated ObjectRef for body.
func newUploadObject(t *testing.T, body []byte) rawsync.ObjectRef {
	t.Helper()
	digest := sha256.Sum256(body)
	object, err := rawsync.NewObjectRef(hex.EncodeToString(digest[:]), int64(len(body)))
	require.NoError(t, err)
	return object
}

func TestUploadObjectSingleChunkCompletes(t *testing.T) {
	t.Parallel()
	body := []byte("hello")
	script := &uploadScript{length: int64(len(body)), lieInBody: true}
	server := httptest.NewServer(script.handler(t))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	require.NoError(t, client.UploadObject(t.Context(),
		parser.AgentClaude, newUploadObject(t, body), bytes.NewReader(body)))
	// One PATCH carried the whole object; the stale body did not mislead the
	// client because the response headers won.
	require.Len(t, script.patchBytes, 1)
	assert.Equal(t, body, script.patchBytes[0])
}

func TestUploadObjectResumesFromServerOffset(t *testing.T) {
	t.Parallel()
	body := []byte("0123456789abcdefghij")
	script := &uploadScript{length: int64(len(body)), failFirst: "offset_conflict"}
	// The POST reports offset 0 while the server's authoritative offset is
	// 10: the first PATCH races ahead and the conflict must rewind it.
	script.startOffset, script.offset = 0, 10
	server := httptest.NewServer(script.handler(t))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	require.NoError(t, client.UploadObject(t.Context(),
		parser.AgentClaude, newUploadObject(t, body), bytes.NewReader(body)))
	// Exactly the tail beyond the authoritative offset was transferred.
	require.NotEmpty(t, script.patchBytes)
	var uploaded int
	for _, chunk := range script.patchBytes {
		uploaded += len(chunk)
	}
	assert.Equal(t, 10, uploaded)
}

func TestUploadObjectChecksumMismatchIsTerminal(t *testing.T) {
	t.Parallel()
	body := []byte("terminal")
	script := &uploadScript{length: int64(len(body)), failFirst: "checksum_mismatch"}
	server := httptest.NewServer(script.handler(t))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	err := client.UploadObject(t.Context(),
		parser.AgentClaude, newUploadObject(t, body), bytes.NewReader(body))
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.Status)
	assert.Equal(t, CodeChecksumMismatch, apiErr.Code)
	require.NotNil(t, apiErr.CurrentUploadOffset)
	assert.EqualValues(t, 0, *apiErr.CurrentUploadOffset)
	// No successful PATCH and no retry: the mismatch was terminal.
	assert.Empty(t, script.patchBytes)
}

func TestUploadObjectRejectsOversizedChunk(t *testing.T) {
	t.Parallel()
	body := []byte("0123456789abcdefghij")
	script := &uploadScript{length: int64(len(body))}
	server := httptest.NewServer(script.handler(t))
	t.Cleanup(server.Close)
	client, err := NewClient(Config{
		BaseURL: server.URL, DeviceID: "dev_test",
		Credential: "avdc_test", TokenMargin: time.Minute, ChunkBytes: 8,
	})
	require.NoError(t, err)

	require.NoError(t, client.UploadObject(t.Context(),
		parser.AgentClaude, newUploadObject(t, body), bytes.NewReader(body)))
	// 20 bytes at ChunkBytes 8 → PATCHes of 8, 8, 4 — never more than 8.
	require.Len(t, script.patchBytes, 3)
	var got []byte
	for _, chunk := range script.patchBytes {
		assert.LessOrEqual(t, len(chunk), 8)
		got = append(got, chunk...)
	}
	assert.Equal(t, body, got)

	// The configured ceiling never exceeds the transport's hard cap.
	capped, err := NewClient(Config{
		BaseURL: server.URL, DeviceID: "dev_test",
		Credential: "avdc_test",
		ChunkBytes: rawsync.DefaultUploadChunkBytes + 1,
	})
	require.NoError(t, err)
	assert.Equal(t, rawsync.DefaultUploadChunkBytes, capped.chunkBytes)
}

func TestUploadObjectZeroLengthSkipsPatch(t *testing.T) {
	t.Parallel()
	script := &uploadScript{length: 0}
	server := httptest.NewServer(script.handler(t))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	require.NoError(t, client.UploadObject(t.Context(),
		parser.AgentClaude, newUploadObject(t, nil), bytes.NewReader(nil)))
	assert.Empty(t, script.patchBytes)
}

func TestUploadObjectFinalizesFullOffsetSession(t *testing.T) {
	t.Parallel()
	body := []byte("finalize")
	script := &uploadScript{length: int64(len(body)), postIncomplete: true}
	// Custody already holds every byte (offset == length) but the session is
	// not finalized: one empty PATCH at the confirmed offset must complete it.
	script.startOffset, script.offset = int64(len(body)), int64(len(body))
	server := httptest.NewServer(script.handler(t))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	require.NoError(t, client.UploadObject(t.Context(),
		parser.AgentClaude, newUploadObject(t, body), bytes.NewReader(body)))
	// Exactly one zero-byte PATCH finalized the full-offset session.
	require.Len(t, script.patchBytes, 1)
	assert.Empty(t, script.patchBytes[0])
}

func TestUploadObjectFinalizesAfterDeferredCompletion(t *testing.T) {
	t.Parallel()
	body := []byte("deferred")
	script := &uploadScript{length: int64(len(body)), deferComplete: true}
	server := httptest.NewServer(script.handler(t))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	require.NoError(t, client.UploadObject(t.Context(),
		parser.AgentClaude, newUploadObject(t, body), bytes.NewReader(body)))
	// The data PATCH reported the full offset without completion; the empty
	// finalization PATCH then confirmed it.
	require.Len(t, script.patchBytes, 2)
	assert.Equal(t, body, script.patchBytes[0])
	assert.Empty(t, script.patchBytes[1])
}

func TestUploadObjectErrorsWhenFinalizationStaysIncomplete(t *testing.T) {
	t.Parallel()
	body := []byte("never")
	script := &uploadScript{
		length: int64(len(body)), postIncomplete: true, neverComplete: true,
	}
	script.startOffset, script.offset = int64(len(body)), int64(len(body))
	server := httptest.NewServer(script.handler(t))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	err := client.UploadObject(t.Context(),
		parser.AgentClaude, newUploadObject(t, body), bytes.NewReader(body))
	require.Error(t, err)
	assert.ErrorContains(t, err, "not finalized")
	// Exactly one finalization attempt: the client never spins on a session
	// the server refuses to complete.
	require.Len(t, script.patchBytes, 1)
	assert.Empty(t, script.patchBytes[0])
}
