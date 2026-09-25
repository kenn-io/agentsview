package server

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Server) registerConversationExportRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Export")

	s.postLong(group, "/export/conversations/initialize",
		"Initialize conversation export", s.humaInitializeConversationExport)
}

type conversationExportInitializeResponse struct {
	DatabaseID  string `json:"database_id" doc:"Archive generation that now serves conversation exports"`
	Initialized bool   `json:"initialized" doc:"Whether this request built the projection"`
}

// The export CLI reads the archive read-only, so a cold archive asks the
// daemon that owns the writer to build the projection before its first walk.
func (s *Server) humaInitializeConversationExport(
	ctx context.Context, _ *emptyInput,
) (*jsonOutput[conversationExportInitializeResponse], error) {
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	initialized, err := local.EnsureConversationExportInitialized(ctx)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("initialize conversation export", err)
	}
	databaseID, err := local.GetOrCreateDatabaseID(ctx)
	if err != nil {
		return nil, internalError("initialize conversation export", err)
	}
	return &jsonOutput[conversationExportInitializeResponse]{
		Body: conversationExportInitializeResponse{DatabaseID: databaseID, Initialized: initialized},
	}, nil
}
