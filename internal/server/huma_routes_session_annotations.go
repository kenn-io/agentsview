package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/db"
)

// registerSessionAnnotationRoutes owns caller-supplied session metadata:
// labels and launcher-supplied parent links. Both are stored apart from
// parsed transcript data and accept session IDs that have not synced yet,
// so a launcher can record them as soon as it starts a session.
func (s *Server) registerSessionAnnotationRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Sessions")

	s.get(group, "/sessions/{id}/labels", "Get session labels", s.humaGetSessionLabels)
	s.put(group, "/sessions/{id}/labels", "Replace session labels", s.humaSetSessionLabels)
	s.patch(group, "/sessions/{id}/labels", "Add or remove session labels", s.humaUpdateSessionLabels)
	s.get(group, "/sessions/{id}/parent", "Get session parent link", s.humaGetSessionParent)
	s.put(group, "/sessions/{id}/parent", "Set session parent link", s.humaSetSessionParent)
	s.deleteRoute(group, "/sessions/{id}/parent", "Remove session parent link", s.humaClearSessionParent)
}

type setSessionLabelsInput struct {
	ID   string `path:"id" required:"true" doc:"Session ID"`
	Body struct {
		Labels []string `json:"labels" required:"true" doc:"The complete label set; an empty list removes every label. Labels are free text, often key=value."`
	}
}

type updateSessionLabelsInput struct {
	ID   string `path:"id" required:"true" doc:"Session ID"`
	Body struct {
		Add    []string `json:"add,omitempty" doc:"Labels to add"`
		Remove []string `json:"remove,omitempty" doc:"Labels to remove; absent labels are ignored"`
	}
}

type setSessionParentInput struct {
	ID   string `path:"id" required:"true" doc:"Session ID"`
	Body struct {
		ParentSessionID  string `json:"parent_session_id" required:"true" doc:"ID of the session that launched this one"`
		RelationshipType string `json:"relationship_type,omitempty" enum:"subagent,fork,continuation" doc:"Relationship to the parent; defaults to subagent"`
	}
}

func (s *Server) humaGetSessionLabels(
	ctx context.Context, in *idPathInput,
) (*jsonOutput[db.SessionLabels], error) {
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	labels, err := localDB.GetSessionLabels(ctx, in.ID)
	if err != nil {
		return nil, sessionAnnotationError("get session labels", err)
	}
	return &jsonOutput[db.SessionLabels]{Body: labelsResponse(labels)}, nil
}

func (s *Server) humaSetSessionLabels(
	ctx context.Context, in *setSessionLabelsInput,
) (*jsonOutput[db.SessionLabels], error) {
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	labels, err := s.syncEngineForLocal(ctx, localDB).SetSessionLabels(
		ctx, in.ID, in.Body.Labels,
	)
	if err != nil {
		return nil, sessionAnnotationError("set session labels", err)
	}
	return &jsonOutput[db.SessionLabels]{Body: labelsResponse(labels)}, nil
}

func (s *Server) humaUpdateSessionLabels(
	ctx context.Context, in *updateSessionLabelsInput,
) (*jsonOutput[db.SessionLabels], error) {
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	labels, err := s.syncEngineForLocal(ctx, localDB).UpdateSessionLabels(
		ctx, in.ID, in.Body.Add, in.Body.Remove,
	)
	if err != nil {
		return nil, sessionAnnotationError("update session labels", err)
	}
	return &jsonOutput[db.SessionLabels]{Body: labelsResponse(labels)}, nil
}

func (s *Server) humaGetSessionParent(
	ctx context.Context, in *idPathInput,
) (*jsonOutput[db.SessionExternalParent], error) {
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	link, err := localDB.GetSessionExternalParent(ctx, in.ID)
	if err != nil {
		return nil, sessionAnnotationError("get session parent", err)
	}
	return &jsonOutput[db.SessionExternalParent]{Body: link}, nil
}

func (s *Server) humaSetSessionParent(
	ctx context.Context, in *setSessionParentInput,
) (*jsonOutput[db.SessionExternalParent], error) {
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	link, err := s.syncEngineForLocal(ctx, localDB).SetSessionExternalParent(
		ctx, in.ID, in.Body.ParentSessionID, in.Body.RelationshipType,
	)
	if err != nil {
		return nil, sessionAnnotationError("set session parent", err)
	}
	return &jsonOutput[db.SessionExternalParent]{Body: link}, nil
}

func (s *Server) humaClearSessionParent(
	ctx context.Context, in *idPathInput,
) (*jsonOutput[db.SessionExternalParent], error) {
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	link, err := s.syncEngineForLocal(ctx, localDB).ClearSessionExternalParent(
		ctx, in.ID,
	)
	if err != nil {
		return nil, sessionAnnotationError("clear session parent", err)
	}
	return &jsonOutput[db.SessionExternalParent]{Body: link}, nil
}

func labelsResponse(labels db.SessionLabels) db.SessionLabels {
	if labels.Labels == nil {
		labels.Labels = []string{}
	}
	return labels
}

func sessionAnnotationError(op string, err error) error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return apiError(http.StatusNotFound, "session parent link not found")
	case errors.Is(err, db.ErrSessionLabelsInvalid),
		errors.Is(err, db.ErrSessionExternalParentInvalid):
		return apiError(http.StatusBadRequest, err.Error())
	}
	if handled := handleHumaReadOnly(err); handled != nil {
		return handled
	}
	return internalError(op, err)
}
