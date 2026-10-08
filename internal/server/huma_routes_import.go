package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
)

func (s *Server) registerImportRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/import")
	configureRouteGroup(group, "Import")
	s.api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[importer.ImportStats](), true, "")
	var results sync.Map
	s.stream(group, http.MethodPost, "/claude-ai/sync", "Sync Claude.ai conversations",
		func(ctx context.Context, in *claudeAISyncInput) (*huma.StreamResponse, error) {
			return s.humaSyncClaudeAI(ctx, in, &results)
		}, func(op *huma.Operation) {
			op.Responses["200"].Content["text/event-stream"].Schema.Description = "Server-sent events: fetch requests a browser response with id and path; progress reports import counts; done returns the final counts; error reports a failed sync."
		})
	registerRoute(group, http.MethodPost, "/claude-ai/sync/results/{id}", "Answer Claude.ai browser fetch",
		func(ctx context.Context, in *claudeAISyncResultInput) (*struct{}, error) {
			value, ok := results.LoadAndDelete(in.ID)
			if !ok {
				return nil, apiError(http.StatusNotFound, "fetch request expired or already answered")
			}
			value.(chan claudeAISyncResult) <- claudeAISyncResult{status: in.Status, body: in.RawBody, retryAfter: in.RetryAfter}
			return &struct{}{}, nil
		}, maxBodyBytes(-1), func(op *huma.Operation) {
			op.Middlewares = append(op.Middlewares, func(ctx huma.Context, next func(huma.Context)) {
				if _, ok := results.Load(ctx.Param("id")); !ok {
					ctx.SetStatus(http.StatusNotFound)
					return
				}
				body, err := io.ReadAll(io.LimitReader(ctx.BodyReader(), importer.ClaudeAIResponseLimit+1))
				if len(body) > importer.ClaudeAIResponseLimit {
					err = importer.ErrClaudeAIResponseTooLarge
				}
				if err != nil {
					if value, ok := results.LoadAndDelete(ctx.Param("id")); ok {
						value.(chan claudeAISyncResult) <- claudeAISyncResult{err: err}
						ctx.SetStatus(http.StatusNoContent)
					} else {
						ctx.SetStatus(http.StatusNotFound)
					}
					return
				}
				req, _ := humago.Unwrap(ctx)
				req.Body = io.NopCloser(bytes.NewReader(body))
				next(ctx)
			})
		})

	s.stream(group, http.MethodPost, "/claude-ai",
		"Import Claude.ai archive", s.humaImportClaudeAI,
		streamJSONResponseSchema("ImporterImportStats"),
	)
	s.stream(group, http.MethodPost, "/chatgpt",
		"Import ChatGPT archive", s.humaImportChatGPT,
		streamJSONResponseSchema("ImporterImportStats"),
	)
}

type claudeAISyncInput struct{}

type claudeAISyncResultInput struct {
	ID         string `path:"id"`
	Status     int    `query:"status" minimum:"0" maximum:"599" required:"true"`
	RetryAfter string `header:"Retry-After"`
	RawBody    []byte `contentType:"application/octet-stream"`
}

type claudeAISyncResult struct {
	err        error
	status     int
	body       []byte
	retryAfter string
}

func (s *Server) humaSyncClaudeAI(ctx context.Context, in *claudeAISyncInput, results *sync.Map) (*huma.StreamResponse, error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented, "import not available in read-only mode")
	}
	if err := s.rejectWriterClosedWrite(); err != nil {
		return nil, err
	}
	store, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "sync requires a local archive")
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			return
		}
		ctx, cancel := context.WithCancel(hctx.Context())
		defer cancel()
		fetch := func(ctx context.Context, path string) (importer.ClaudeAIResponse, error) {
			id := rand.Text()
			answer := make(chan claudeAISyncResult, 1)
			results.Store(id, answer)
			defer results.Delete(id)
			if !stream.SendJSON("fetch", map[string]string{"id": id, "path": path}) {
				cancel()
				return importer.ClaudeAIResponse{}, ctx.Err()
			}
			select {
			case <-ctx.Done():
				return importer.ClaudeAIResponse{}, ctx.Err()
			case <-time.After(2 * time.Minute):
				return importer.ClaudeAIResponse{}, errors.New("claude browser fetch timed out")
			case response := <-answer:
				err := response.err
				if err == nil && response.status == 0 {
					err = errors.New(string(response.body))
				}
				return importer.ClaudeAIResponse{Status: response.status, Body: response.body, RetryAfter: response.retryAfter}, err
			}
		}
		stats, err := importer.SyncClaudeAI(ctx, store, fetch, &importer.ImportCallbacks{
			SerializeWrite: func(write func() error) error { return s.serializeArchiveWrite(ctx, write) },
			OnProgress: func(stats importer.ImportStats) {
				if !stream.SendJSON("progress", stats) {
					cancel()
				}
			},
		}, s.cfg.InstallationID)
		if stats.Imported+stats.Updated > 0 {
			if s.broadcaster != nil {
				s.broadcaster.Emit("sessions")
			}
			s.notifySessionMutation()
			s.notifyRecallCorpusMutation()
		}
		if err != nil {
			message := err.Error()
			if errors.Is(err, importer.ErrClaudeAIAuthRequired) {
				message = "Sign in to Claude.ai, then Sync again"
			}
			stream.SendJSON("error", map[string]string{"error": message})
			return
		}
		stream.SendJSON("done", stats)
	}}, nil
}

type importArchiveInput struct {
	Accept  string   `header:"Accept" doc:"Use text/event-stream to stream progress"`
	Replace []string `query:"replace,explode" doc:"Session IDs whose archived messages this import may replace when the default import refuses the export; the previous version moves to the trash. Repeatable"`
	RawBody huma.MultipartFormFiles[importArchiveForm]
}

type importArchiveForm struct {
	File huma.FormFile `form:"file" contentType:"application/octet-stream" required:"true"`
}

func (s *Server) humaImportClaudeAI(
	ctx context.Context,
	in *importArchiveInput,
) (*huma.StreamResponse, error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"import not available in read-only mode")
	}
	if err := s.rejectWriterClosedWrite(); err != nil {
		return nil, err
	}
	file := in.RawBody.Data().File
	if !file.IsSet {
		return nil, apiError(http.StatusBadRequest,
			"missing 'file' field in form data")
	}
	opts := importer.ImportOptions{Replace: in.Replace}
	if !strings.Contains(in.Accept, "text/event-stream") {
		stats, err := s.importClaudeAIFromFile(ctx, file, opts)
		if err != nil {
			return nil, err
		}
		return jsonStreamResponse(stats), nil
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiResponseError{Message: "streaming not supported"})
			return
		}
		stats, err := s.importClaudeAIFromFileWithCallbacks(hctx.Context(), file, &importer.ImportCallbacks{
			OnProgress: func(stats importer.ImportStats) {
				stream.SendJSON("progress", stats)
			},
			OnIndexing: func() {
				stream.SendJSON("indexing", struct{}{})
			},
		}, opts)
		if err != nil {
			stream.SendJSON("error", map[string]string{"error": err.Error()})
			return
		}
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) importClaudeAIFromFile(
	ctx context.Context,
	file huma.FormFile,
	opts importer.ImportOptions,
) (importer.ImportStats, error) {
	return s.importClaudeAIFromFileWithCallbacks(ctx, file, nil, opts)
}

func (s *Server) importClaudeAIFromFileWithCallbacks(
	ctx context.Context,
	file huma.FormFile,
	cb *importer.ImportCallbacks,
	opts importer.ImportOptions,
) (importer.ImportStats, error) {
	reader, cleanup, err := claudeImportReader(file)
	if err != nil {
		return importer.ImportStats{}, err
	}
	defer cleanup()
	var stats importer.ImportStats
	err = s.serializeArchiveWrite(ctx, func() error {
		var importErr error
		stats, importErr = importer.ImportClaudeAIWithOptions(ctx, s.db, reader, cb, opts)
		return importErr
	})
	if err != nil {
		if errors.Is(err, db.ErrWriterClosed) {
			return importer.ImportStats{}, writerClosedError()
		}
		return importer.ImportStats{}, apiError(http.StatusInternalServerError,
			"import failed: "+err.Error())
	}
	return stats, nil
}

func claudeImportReader(file huma.FormFile) (io.Reader, func(), error) {
	cleanup := func() {}
	reader := io.Reader(file)
	if strings.HasSuffix(strings.ToLower(file.Filename), ".zip") {
		tmpFile, tmpErr := os.CreateTemp("", "claude-import-*.zip")
		if tmpErr != nil {
			return nil, cleanup, apiError(http.StatusInternalServerError,
				"failed to create temp file")
		}
		tmpName := tmpFile.Name()
		cleanup = func() { _ = os.Remove(tmpName) }
		if _, tmpErr = io.Copy(tmpFile, file); tmpErr != nil {
			_ = tmpFile.Close()
			cleanup()
			return nil, func() {}, apiError(http.StatusInternalServerError,
				"failed to save upload")
		}
		_ = tmpFile.Close()
		dir, zipCleanup, extractErr := importer.ExtractZip(tmpName)
		if extractErr != nil {
			cleanup()
			return nil, func() {}, apiError(http.StatusBadRequest,
				"failed to extract zip: "+extractErr.Error())
		}
		cleanup = func() {
			zipCleanup()
			_ = os.Remove(tmpName)
		}
		jsonPath := filepath.Join(dir, "conversations.json")
		jsonFile, openErr := os.Open(jsonPath)
		if openErr != nil {
			cleanup()
			return nil, func() {}, apiError(http.StatusBadRequest,
				"no conversations.json found in zip")
		}
		oldCleanup := cleanup
		cleanup = func() {
			_ = jsonFile.Close()
			oldCleanup()
		}
		reader = jsonFile
	}
	return reader, cleanup, nil
}

func (s *Server) humaImportChatGPT(
	ctx context.Context,
	in *importArchiveInput,
) (*huma.StreamResponse, error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"import not available in read-only mode")
	}
	if err := s.rejectWriterClosedWrite(); err != nil {
		return nil, err
	}
	file := in.RawBody.Data().File
	if !file.IsSet {
		return nil, apiError(http.StatusBadRequest,
			"missing 'file' field in form data")
	}
	if !strings.HasSuffix(strings.ToLower(file.Filename), ".zip") {
		return nil, apiError(http.StatusBadRequest,
			"ChatGPT import requires a .zip file")
	}
	if !strings.Contains(in.Accept, "text/event-stream") {
		stats, err := s.importChatGPTFromFile(ctx, file, nil, importer.ImportOptions{Replace: in.Replace})
		if err != nil {
			return nil, err
		}
		return jsonStreamResponse(stats), nil
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiResponseError{Message: "streaming not supported"})
			return
		}
		stats, err := s.importChatGPTFromFile(hctx.Context(), file, &importer.ImportCallbacks{
			OnProgress: func(stats importer.ImportStats) {
				stream.SendJSON("progress", stats)
			},
			OnIndexing: func() {
				stream.SendJSON("indexing", struct{}{})
			},
		}, importer.ImportOptions{Replace: in.Replace})
		if err != nil {
			stream.SendJSON("error", map[string]string{"error": err.Error()})
			return
		}
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) importChatGPTFromFile(
	ctx context.Context,
	file huma.FormFile,
	cb *importer.ImportCallbacks,
	opts importer.ImportOptions,
) (importer.ImportStats, error) {
	tmpFile, err := os.CreateTemp("", "chatgpt-import-*.zip")
	if err != nil {
		return importer.ImportStats{}, apiError(http.StatusInternalServerError,
			"failed to create temp file")
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)
	if _, err = io.Copy(tmpFile, file); err != nil {
		_ = tmpFile.Close()
		return importer.ImportStats{}, apiError(http.StatusInternalServerError,
			"failed to save upload")
	}
	_ = tmpFile.Close()
	dir, cleanup, err := importer.ExtractZip(tmpName)
	if err != nil {
		return importer.ImportStats{}, apiError(http.StatusBadRequest,
			"failed to extract zip: "+err.Error())
	}
	defer cleanup()
	var stats importer.ImportStats
	err = s.serializeArchiveWrite(ctx, func() error {
		var importErr error
		stats, importErr = importer.ImportChatGPTWithOptions(ctx, s.db, dir,
			filepath.Join(s.cfg.DataDir, "assets"), cb, opts)
		return importErr
	})
	if err != nil {
		if errors.Is(err, db.ErrWriterClosed) {
			return importer.ImportStats{}, writerClosedError()
		}
		return importer.ImportStats{}, apiError(http.StatusInternalServerError,
			"import failed: "+err.Error())
	}
	return stats, nil
}

func jsonStreamResponse(value any) *huma.StreamResponse {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		hctx.SetHeader("Content-Type", "application/json")
		_ = json.MarshalEncode(jsontext.NewEncoder(hctx.BodyWriter()), value)
	}}
}
