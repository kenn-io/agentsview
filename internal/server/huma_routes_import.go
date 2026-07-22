package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
)

func (s *Server) registerImportRoutes() {
	group := newRouteGroup(s.api, "/api/v1/import", "Import")

	stream(
		s, group, http.MethodPost, "/claude-ai",
		"Import Claude.ai archive", s.humaImportClaudeAI,
		streamJSONResponse(),
	)
	stream(
		s, group, http.MethodPost, "/chatgpt",
		"Import ChatGPT archive", s.humaImportChatGPT,
		streamJSONResponse(),
	)
}

type importArchiveInput struct {
	Accept  string `header:"Accept" doc:"Use text/event-stream to stream progress"`
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
	if !strings.Contains(in.Accept, "text/event-stream") {
		stats, err := s.importClaudeAIFromFile(ctx, file)
		if err != nil {
			return nil, err
		}
		return jsonStreamResponse(stats), nil
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
			return
		}
		stats, err := s.importClaudeAIFromFileWithCallbacks(hctx.Context(), file, &importer.ImportCallbacks{
			OnProgress: func(stats importer.ImportStats) {
				stream.SendJSON("progress", stats)
			},
			OnIndexing: func() {
				stream.SendJSON("indexing", struct{}{})
			},
		})
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
) (importer.ImportStats, error) {
	return s.importClaudeAIFromFileWithCallbacks(ctx, file, nil)
}

func (s *Server) importClaudeAIFromFileWithCallbacks(
	ctx context.Context,
	file huma.FormFile,
	cb *importer.ImportCallbacks,
) (importer.ImportStats, error) {
	reader, cleanup, err := claudeImportReader(file)
	if err != nil {
		return importer.ImportStats{}, err
	}
	defer cleanup()
	var stats importer.ImportStats
	err = s.serializeArchiveWrite(func() error {
		var importErr error
		stats, importErr = importer.ImportClaudeAI(ctx, s.db, reader, cb)
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
		stats, err := s.importChatGPTFromFile(ctx, file, nil)
		if err != nil {
			return nil, err
		}
		return jsonStreamResponse(stats), nil
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
			return
		}
		stats, err := s.importChatGPTFromFile(hctx.Context(), file, &importer.ImportCallbacks{
			OnProgress: func(stats importer.ImportStats) {
				stream.SendJSON("progress", stats)
			},
			OnIndexing: func() {
				stream.SendJSON("indexing", struct{}{})
			},
		})
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
	err = s.serializeArchiveWrite(func() error {
		var importErr error
		stats, importErr = importer.ImportChatGPT(ctx, s.db, dir,
			filepath.Join(s.cfg.DataDir, "assets"), cb)
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
		_ = json.NewEncoder(hctx.BodyWriter()).Encode(value)
	}}
}
