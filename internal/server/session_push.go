package server

import (
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

const sessionPushReadTimeout = 2 * time.Minute

type sessionPushResponse struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Status    string `json:"status"`
}

// This native transfer route validates the packet before requesting the
// archive write barrier. Huma describes it for clients without pre-decoding
// untrusted nested collections or replacing its stricter byte limits.
func (s *Server) describeSessionPushRoute() {
	schemas := s.api.OpenAPI().Components.Schemas
	requestSchema := schemas.Schema(reflect.TypeFor[artifact.SessionPushEnvelope](), true, "")
	request := schemas.SchemaFromRef(requestSchema.Ref)
	// Raw JSON is deliberately preflighted by the native handler. Describe its
	// actual wire shape rather than exposing an untyped raw-message schema.
	request.Properties["manifest"] = &huma.Schema{Type: "object", AdditionalProperties: true}
	maxSegments := 16
	request.Properties["segments"] = &huma.Schema{Type: "array", MaxItems: &maxSegments, Items: &huma.Schema{
		Type: "object", AdditionalProperties: false, Required: []string{"sha256", "ndjson"},
		Properties: map[string]*huma.Schema{
			"sha256": {Type: "string", Pattern: "^[0-9a-f]{64}$"},
			"ndjson": {Type: "string"},
		},
	}}
	op := &huma.Operation{
		OperationID: operationID(http.MethodPost, "/api/v1/sessions/ingest"), Method: http.MethodPost, Path: "/api/v1/sessions/ingest", Summary: "Ingest one explicitly pushed normalized session", Tags: []string{"Sessions"},
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/json": {Schema: requestSchema}}},
		Responses:   map[string]*huma.Response{"200": {Description: "Created or identical retry", Content: map[string]*huma.MediaType{"application/json": {Schema: schemas.Schema(reflect.TypeFor[sessionPushResponse](), true, "")}}}},
	}
	for _, status := range []string{"400", "401", "403", "404", "405", "409", "413", "415", "500", "501", "503"} {
		op.Responses[status] = &huma.Response{Description: "Push rejected", Content: map[string]*huma.MediaType{"text/plain": {Schema: &huma.Schema{Type: "string"}}}}
	}
	s.api.OpenAPI().AddOperation(op)
	s.handleHTTP(op, s.handleSessionPush)
	s.handleHTTP(&huma.Operation{Method: http.MethodGet, Path: op.Path, Hidden: true}, s.handleSessionPush)
}

func (s *Server) handleSessionPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	enabled, auth, token := s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken
	s.mu.RUnlock()
	if !enabled {
		http.Error(w, "session ingest is disabled", http.StatusNotFound)
		return
	}
	if !auth || token == "" {
		http.Error(w, "session ingest requires bearer authentication", http.StatusForbidden)
		return
	}
	local, ok := s.db.(*db.DB)
	if !ok || local.ReadOnly() {
		http.Error(w, "session ingest requires writable SQLite", http.StatusNotImplemented)
		return
	}
	if local.ArchiveContent().UsageOnly() {
		http.Error(w, "usage-only archives cannot receive transcripts", http.StatusForbidden)
		return
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		http.Error(w, "only identity content encoding is supported", http.StatusUnsupportedMediaType)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "application/json is required", http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > artifact.MaxSessionPushBytes {
		http.Error(w, "session exceeds 64 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	if !s.sessionPushMu.TryLock() {
		http.Error(w, "another session ingest is active", http.StatusConflict)
		return
	}
	defer s.sessionPushMu.Unlock()
	if err := extendRequestReadDeadline(w, sessionPushReadTimeout); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("extending session push read deadline: %v", err)
		http.Error(w, "cannot prepare session upload", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, artifact.MaxSessionPushBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "cannot read bounded session payload", status)
		return
	}
	receipt, write, err := artifact.DecodeSessionPush(body)
	if err != nil {
		http.Error(w, "invalid normalized session payload", http.StatusBadRequest)
		return
	}
	var created bool
	err = s.tryArchiveWrite(r.Context(), func() error {
		var applyErr error
		created, applyErr = local.ApplySessionPush(r.Context(), receipt, write)
		if applyErr == nil && created {
			s.notifySessionMutation()
			if s.broadcaster != nil {
				s.broadcaster.Emit("sessions")
			}
		}
		return applyErr
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, db.ErrSessionPushConflict) || errors.Is(err, syncpkg.ErrSyncInProgress) || errors.Is(err, db.ErrCompactInProgress) {
			status = http.StatusConflict
		}
		if errors.Is(err, db.ErrWriterClosed) || r.Context().Err() != nil {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "session ingest failed", status)
		return
	}
	status := "unchanged"
	if created {
		status = "created"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, sessionPushResponse{SessionID: write.Session.ID, Agent: write.Session.Agent, Status: status})
}
