package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type agentRemapRuleRequest struct {
	SourceAgent *string `json:"source_agent,omitempty"`
	ModelGlob   *string `json:"model_glob,omitempty"`
	IDPrefix    *string `json:"id_prefix,omitempty"`
	TargetAgent *string `json:"target_agent,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

type agentRemapRuleUpdateInput struct {
	ID   string                `path:"id" required:"true" doc:"Rule ID"`
	Body agentRemapRuleRequest `json:"body"`
}

type agentRemapRuleCreateInput struct {
	Body agentRemapRuleRequest `json:"body"`
}

type agentRemapRulePathInput struct {
	ID string `path:"id" required:"true" doc:"Rule ID"`
}

type agentRemapPreviewInput struct {
	Body struct{} `json:"body"`
}

type agentRemapApplyInput struct {
	Body struct {
		Token string `json:"token" doc:"Consistency token from the preview"`
	} `json:"body"`
}

// agentRemapRegistryAgents returns the registered agent ids for validation.
func agentRemapRegistryAgents() map[string]bool {
	out := make(map[string]bool, len(parser.Registry))
	for _, def := range parser.Registry {
		out[string(def.Type)] = true
	}
	return out
}

func (s *Server) agentRemapWritableDB() (*db.DB, error) {
	localDB, ok := s.db.(*db.DB)
	if !ok || localDB == nil || localDB.ReadOnly() || s.engine == nil {
		return nil, apiError(
			http.StatusNotImplemented, "not available in remote mode",
		)
	}
	return localDB, nil
}

// validateAgentRemapRuleBody enforces registry membership for agents. The db
// layer validates the shape; the server layer knows the parser registry.
func validateAgentRemapRuleBody(
	body agentRemapRuleRequest,
) (source, target string, err error) {
	known := agentRemapRegistryAgents()
	if body.SourceAgent == nil || strings.TrimSpace(*body.SourceAgent) == "" {
		return "", "", apiError(http.StatusBadRequest, "source_agent is required")
	}
	source = strings.TrimSpace(*body.SourceAgent)
	if body.TargetAgent == nil || strings.TrimSpace(*body.TargetAgent) == "" {
		return "", "", apiError(http.StatusBadRequest, "target_agent is required")
	}
	target = strings.TrimSpace(*body.TargetAgent)
	if !known[source] {
		return "", "", apiError(
			http.StatusBadRequest, "unknown source_agent "+source,
		)
	}
	if !known[target] {
		return "", "", apiError(
			http.StatusBadRequest, "unknown target_agent "+target,
		)
	}
	if source == target {
		return "", "", apiError(
			http.StatusBadRequest, "source_agent and target_agent must differ",
		)
	}
	return source, target, nil
}

func agentRemapRuleError(err error) error {
	switch {
	case errors.Is(err, db.ErrAgentRemapRulesChanged):
		return apiError(http.StatusConflict, err.Error())
	case errors.Is(err, sql.ErrNoRows):
		return apiError(http.StatusNotFound, "agent remap rule not found")
	case strings.Contains(err.Error(), "required"),
		strings.Contains(err.Error(), "must differ"),
		strings.Contains(err.Error(), "unknown"):
		return apiError(http.StatusBadRequest, err.Error())
	default:
		return internalError("agent remap rules", err)
	}
}

func (s *Server) humaListAgentRemapRules(
	ctx context.Context, _ *emptyInput,
) (*jsonOutput[[]db.AgentRemapRule], error) {
	localDB, err := s.agentRemapWritableDB()
	if err != nil {
		return nil, err
	}
	rules, err := localDB.ListAgentRemapRules(ctx)
	if err != nil {
		return nil, agentRemapRuleError(err)
	}
	if rules == nil {
		rules = []db.AgentRemapRule{}
	}
	return &jsonOutput[[]db.AgentRemapRule]{Body: rules}, nil
}

func (s *Server) humaCreateAgentRemapRule(
	ctx context.Context, in *agentRemapRuleCreateInput,
) (*createdOutput[db.AgentRemapRule], error) {
	localDB, err := s.agentRemapWritableDB()
	if err != nil {
		return nil, err
	}
	source, target, err := validateAgentRemapRuleBody(in.Body)
	if err != nil {
		return nil, err
	}
	enabled := true
	if in.Body.Enabled != nil {
		enabled = *in.Body.Enabled
	}
	rule, err := localDB.CreateAgentRemapRule(ctx, db.AgentRemapRule{
		SourceAgent: source,
		ModelGlob:   stringValueOrEmpty(in.Body.ModelGlob),
		IDPrefix:    stringValueOrEmpty(in.Body.IDPrefix),
		TargetAgent: target,
		Enabled:     enabled,
	})
	if err != nil {
		return nil, agentRemapRuleError(err)
	}
	return &createdOutput[db.AgentRemapRule]{
		Status: http.StatusCreated, Body: rule,
	}, nil
}

func (s *Server) humaUpdateAgentRemapRule(
	ctx context.Context, in *agentRemapRuleUpdateInput,
) (*jsonOutput[db.AgentRemapRule], error) {
	localDB, err := s.agentRemapWritableDB()
	if err != nil {
		return nil, err
	}
	source, target, err := validateAgentRemapRuleBody(in.Body)
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(in.ID, 10, 64)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "invalid rule id")
	}
	enabled := true
	if in.Body.Enabled != nil {
		enabled = *in.Body.Enabled
	}
	rule, err := localDB.UpdateAgentRemapRule(ctx, db.AgentRemapRule{
		ID:          id,
		SourceAgent: source,
		ModelGlob:   stringValueOrEmpty(in.Body.ModelGlob),
		IDPrefix:    stringValueOrEmpty(in.Body.IDPrefix),
		TargetAgent: target,
		Enabled:     enabled,
	})
	if err != nil {
		return nil, agentRemapRuleError(err)
	}
	return &jsonOutput[db.AgentRemapRule]{Body: rule}, nil
}

func (s *Server) humaDeleteAgentRemapRule(
	ctx context.Context, in *agentRemapRulePathInput,
) (*noContentOutput, error) {
	localDB, err := s.agentRemapWritableDB()
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(in.ID, 10, 64)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "invalid rule id")
	}
	if err := localDB.DeleteAgentRemapRule(ctx, id); err != nil {
		return nil, agentRemapRuleError(err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaPreviewAgentRemapRules(
	ctx context.Context, _ *agentRemapPreviewInput,
) (*jsonOutput[db.AgentRemapPreview], error) {
	localDB, err := s.agentRemapWritableDB()
	if err != nil {
		return nil, err
	}
	preview, err := localDB.PreviewAgentRemapRules(ctx)
	if err != nil {
		return nil, agentRemapRuleError(err)
	}
	return &jsonOutput[db.AgentRemapPreview]{Body: preview}, nil
}

func (s *Server) humaApplyAgentRemapRules(
	ctx context.Context, in *agentRemapApplyInput,
) (*jsonOutput[db.AgentRemapPreview], error) {
	if _, err := s.agentRemapWritableDB(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Body.Token) == "" {
		return nil, apiError(http.StatusBadRequest, "token is required")
	}
	preview, err := s.engine.ApplyAgentRemapRules(ctx, in.Body.Token)
	if err != nil {
		return nil, agentRemapRuleError(err)
	}
	return &jsonOutput[db.AgentRemapPreview]{Body: preview}, nil
}
