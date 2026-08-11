package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/cloudsync/claudeai"
	"go.kenn.io/agentsview/internal/cloudsync/transport"
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
	post(s, group, "/claude-ai/sync", "Start Claude.ai sync", s.humaStartClaudeAICloud)
	get(s, group, "/claude-ai/status", "Get Claude.ai sync status", s.humaClaudeAICloudStatus)
	post(s, group, "/claude-ai/cancel", "Cancel Claude.ai sync", s.humaCancelClaudeAICloud)
	get(s, group, "/claude-ai/schedule", "Get Claude.ai sync schedule", s.humaClaudeAICloudSchedule)
	post(s, group, "/claude-ai/schedule", "Configure Claude.ai sync schedule", s.humaConfigureClaudeAICloudSchedule)
	post(s, group, "/transport/claim", "Claim authenticated cloud request", s.humaClaimCloudTransport)
	post(s, group, "/transport/result", "Complete authenticated cloud request", s.humaCompleteCloudTransport)
	stream(s, group, http.MethodPost, "/claude-ai/import",
		"Import Claude.ai conversations", s.humaImportClaudeAICloud,
		streamJSONResponse(),
		func(op *huma.Operation) { op.MaxBodyBytes = 32 << 20 },
	)
}

type cloudSyncInput struct {
	Body struct {
		Mode claudeai.SyncMode `json:"mode"`
	} `contentType:"application/json"`
}

type cloudSyncResponse struct{ Body claudeai.JobStatus }
type cloudScheduleInput struct {
	Body claudeai.ScheduleConfig `contentType:"application/json"`
}
type cloudScheduleResponse struct{ Body claudeai.ScheduleConfig }
type cloudTransportClaimResponse struct{ Body transport.Request }
type cloudTransportResultInput struct {
	Body transport.Response `contentType:"application/json"`
}
type cloudTransportResultResponse struct{ Body struct{} }

func (s *Server) humaStartClaudeAICloud(ctx context.Context, in *cloudSyncInput) (*cloudSyncResponse, error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented, "cloud import not available in read-only mode")
	}
	status, err := s.claudeSync.Start(ctx, in.Body.Mode)
	if err != nil {
		return nil, apiError(http.StatusConflict, err.Error())
	}
	return &cloudSyncResponse{Body: status}, nil
}

func (s *Server) humaClaudeAICloudStatus(_ context.Context, _ *struct{}) (*cloudSyncResponse, error) {
	return &cloudSyncResponse{Body: s.claudeSync.Status()}, nil
}

func (s *Server) humaCancelClaudeAICloud(_ context.Context, _ *struct{}) (*cloudSyncResponse, error) {
	s.claudeSync.Cancel()
	return &cloudSyncResponse{Body: s.claudeSync.Status()}, nil
}

func (s *Server) humaClaudeAICloudSchedule(_ context.Context, _ *struct{}) (*cloudScheduleResponse, error) {
	return &cloudScheduleResponse{Body: s.claudeSync.Schedule()}, nil
}

func (s *Server) humaConfigureClaudeAICloudSchedule(_ context.Context, in *cloudScheduleInput) (*cloudScheduleResponse, error) {
	if err := s.claudeSync.ConfigureSchedule(in.Body); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	return &cloudScheduleResponse{Body: s.claudeSync.Schedule()}, nil
}

func (s *Server) humaClaimCloudTransport(ctx context.Context, _ *struct{}) (*cloudTransportClaimResponse, error) {
	claimCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := s.claudeTransport.Claim(claimCtx)
	if err != nil {
		return nil, apiError(http.StatusNoContent, "no authenticated cloud request is pending")
	}
	return &cloudTransportClaimResponse{Body: request}, nil
}

func (s *Server) humaCompleteCloudTransport(_ context.Context, in *cloudTransportResultInput) (*cloudTransportResultResponse, error) {
	if err := s.claudeTransport.Complete(in.Body); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	return &cloudTransportResultResponse{}, nil
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
	prepared, err := claudeai.PrepareBrowserImportWithForce(cacheRoot, conversations, repair)
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
	if stats.Errors > 0 {
		return claudeCloudImportStats{}, apiError(http.StatusInternalServerError,
			"import Claude conversations: one or more conversations were not imported")
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
