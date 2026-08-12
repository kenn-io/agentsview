package server

import (
	"context"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/cloudsync/claudeai"
	"go.kenn.io/agentsview/internal/cloudsync/transport"
)

func (s *Server) registerCloudRoutes() {
	group := newRouteGroup(s.api, "/api/v1/cloud", "Cloud sources")
	post(s, group, "/claude-ai/sync", "Start Claude.ai sync", s.humaStartClaudeAICloud)
	get(s, group, "/claude-ai/status", "Get Claude.ai sync status", s.humaClaudeAICloudStatus)
	post(s, group, "/claude-ai/cancel", "Cancel Claude.ai sync", s.humaCancelClaudeAICloud)
	get(s, group, "/claude-ai/schedule", "Get Claude.ai sync schedule", s.humaClaudeAICloudSchedule)
	post(s, group, "/claude-ai/schedule", "Configure Claude.ai sync schedule", s.humaConfigureClaudeAICloudSchedule)
	post(s, group, "/transport/claim", "Claim authenticated cloud request", s.humaClaimCloudTransport)
	post(s, group, "/transport/result", "Complete authenticated cloud request", s.humaCompleteCloudTransport)
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
