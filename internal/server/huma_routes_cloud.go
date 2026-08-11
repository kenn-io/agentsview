package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/cloudsync/claudeai"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
)

func (s *Server) registerCloudRoutes() {
	group := newRouteGroup(s.api, "/api/v1/cloud", "Cloud sources")
	registerRoute(group, http.MethodPost, "/claude-ai/plan",
		"Plan Claude.ai browser import", s.humaPlanClaudeAICloud, s.humaTimeout(),
		func(op *huma.Operation) { op.MaxBodyBytes = 8 << 20 },
	)
	post(s, group, "/claude-ai/complete", "Complete Claude.ai browser import", s.humaCompleteClaudeAICloud)
	post(s, group, "/claude-ai/fail", "Record failed Claude.ai browser import", s.humaFailClaudeAICloud)
	stream(s, group, http.MethodPost, "/claude-ai/import",
		"Import Claude.ai conversations", s.humaImportClaudeAICloud,
		streamJSONResponse(),
		func(op *huma.Operation) { op.MaxBodyBytes = 32 << 20 },
	)
}

type cloudPlanInput struct {
	Body struct {
		Summaries []json.RawMessage `json:"summaries" minItems:"1"`
		Repair    bool              `json:"repair,omitempty"`
	} `contentType:"application/json"`
}

type cloudPlanResponse struct {
	Body claudeai.BrowserImportPlan
}

func (s *Server) humaPlanClaudeAICloud(
	_ context.Context,
	in *cloudPlanInput,
) (*cloudPlanResponse, error) {
	plan, err := claudeai.PlanBrowserImportWithForce(
		filepath.Join(s.cfg.DataDir, "cloud-cache", "claude-ai"), in.Body.Summaries, in.Body.Repair)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "plan Claude import: "+err.Error())
	}
	return &cloudPlanResponse{Body: plan}, nil
}

type cloudCompleteInput struct{}

type cloudCompleteResponse struct{ Body claudeai.SyncState }

func (s *Server) humaCompleteClaudeAICloud(_ context.Context, _ *cloudCompleteInput) (*cloudCompleteResponse, error) {
	state, err := claudeai.CompleteBrowserImport(filepath.Join(s.cfg.DataDir, "cloud-cache", "claude-ai"))
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, "complete Claude import: "+err.Error())
	}
	return &cloudCompleteResponse{Body: state}, nil
}

func (s *Server) humaFailClaudeAICloud(_ context.Context, _ *cloudCompleteInput) (*cloudCompleteResponse, error) {
	state, err := claudeai.FailBrowserImport(filepath.Join(s.cfg.DataDir, "cloud-cache", "claude-ai"))
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, "save failed Claude import state: "+err.Error())
	}
	return &cloudCompleteResponse{Body: state}, nil
}

type cloudImportInput struct {
	Accept string `header:"Accept" doc:"Use text/event-stream to stream progress"`
	Body   struct {
		Conversations []claudeai.BrowserConversation `json:"conversations" minItems:"1" doc:"Authenticated Claude browser responses"`
		Repair        bool                           `json:"repair,omitempty" doc:"Restore explicitly deleted Claude.ai conversations"`
	} `contentType:"application/json"`
}

type claudeCloudImportStats struct {
	importer.ImportStats
	Downloaded int `json:"downloaded"`
	Unchanged  int `json:"unchanged"`
}

func (s *Server) humaImportClaudeAICloud(
	ctx context.Context,
	in *cloudImportInput,
) (*huma.StreamResponse, error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"cloud import not available in read-only mode")
	}
	if err := s.rejectWriterClosedWrite(); err != nil {
		return nil, err
	}
	if !containsSSE(in.Accept) {
		stats, err := s.importClaudeAICloud(ctx, nil, in.Body.Conversations, in.Body.Repair)
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
		stats, err := s.importClaudeAICloud(hctx.Context(), &importer.ImportCallbacks{
			OnProgress: func(stats importer.ImportStats) {
				stream.SendJSON("progress", stats)
			},
			OnIndexing: func() { stream.SendJSON("indexing", struct{}{}) },
		}, in.Body.Conversations, in.Body.Repair)
		if err != nil {
			stream.SendJSON("error", map[string]string{"error": err.Error()})
			return
		}
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) importClaudeAICloud(
	ctx context.Context,
	cb *importer.ImportCallbacks,
	conversations []claudeai.BrowserConversation,
	repair bool,
) (claudeCloudImportStats, error) {
	if len(conversations) == 0 {
		return claudeCloudImportStats{}, apiError(http.StatusBadRequest,
			"no Claude browser conversations supplied")
	}
	cacheRoot := filepath.Join(s.cfg.DataDir, "cloud-cache", "claude-ai")
	prepared, err := claudeai.PrepareBrowserImport(cacheRoot, conversations)
	if err != nil {
		return claudeCloudImportStats{}, apiError(http.StatusBadRequest,
			"process Claude browser conversations: "+err.Error())
	}

	stats := claudeCloudImportStats{
		Downloaded: prepared.Downloaded,
		Unchanged:  prepared.Unchanged,
	}
	err = s.serializeArchiveWrite(func() error {
		if repair {
			ids, restoreErr := claudeRepairSessionIDs(conversations)
			if restoreErr != nil {
				return restoreErr
			}
			if _, restoreErr := s.db.RestoreExcludedSessions(ids); restoreErr != nil {
				return fmt.Errorf("restore Claude deletion markers: %w", restoreErr)
			}
		}
		var importErr error
		stats.ImportStats, importErr = importer.ImportClaudeAI(ctx, s.db,
			bytes.NewReader(prepared.ExportJSON), cb, s.cfg.LocalMachineName)
		return importErr
	})
	if err != nil {
		if errors.Is(err, db.ErrWriterClosed) {
			return claudeCloudImportStats{}, writerClosedError()
		}
		return claudeCloudImportStats{}, apiError(http.StatusInternalServerError,
			"import Claude conversations: "+err.Error())
	}
	if err := prepared.Commit(); err != nil {
		return claudeCloudImportStats{}, apiError(http.StatusInternalServerError,
			"save Claude import checkpoint: "+err.Error())
	}
	if _, err := claudeai.RecordBrowserImportProgress(cacheRoot, prepared.Downloaded,
		stats.Imported+stats.Updated, stats.Errors, false); err != nil {
		return claudeCloudImportStats{}, apiError(http.StatusInternalServerError,
			"save Claude sync state: "+err.Error())
	}
	return stats, nil
}

func claudeRepairSessionIDs(conversations []claudeai.BrowserConversation) ([]string, error) {
	ids := make([]string, 0, len(conversations))
	for _, conversation := range conversations {
		var summary struct {
			UUID string `json:"uuid"`
		}
		if err := json.Unmarshal(conversation.Summary, &summary); err != nil || summary.UUID == "" {
			return nil, fmt.Errorf("decode Claude repair conversation summary")
		}
		ids = append(ids, "claude-ai:"+summary.UUID)
	}
	return ids, nil
}

func containsSSE(accept string) bool {
	return len(accept) >= len("text/event-stream") &&
		bytes.Contains([]byte(accept), []byte("text/event-stream"))
}
