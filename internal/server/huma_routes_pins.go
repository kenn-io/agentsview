package server

import (
	"context"
	"net/http"

	"go.kenn.io/agentsview/internal/db"

	"github.com/danielgtaylor/huma/v2"
)

func (s *Server) registerPinRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Pins")

	s.get(group, "/pins", "List pins", s.humaListPins)
	s.get(group, "/sessions/{id}/pins", "List session pins", s.humaListSessionPins)
	s.post(group, "/sessions/{id}/messages/{messageId}/pin", "Pin message", s.humaPinMessage)
	s.deleteRoute(group, "/sessions/{id}/messages/{messageId}/pin", "Unpin message", s.humaUnpinMessage)
}

type pinsInput struct {
	Project string `query:"project" doc:"Filter by project"`
}

type pinsResponse struct {
	Pins []db.PinnedMessage `json:"pins"`
}

type pinMessageInput struct {
	ID        string `path:"id" required:"true" doc:"Session ID"`
	MessageID int64  `path:"messageId" required:"true" doc:"Message ordinal"`
	Body      pinRequest
}

type pinMessageResponse struct {
	ID int64 `json:"id"`
}

func (s *Server) humaListPins(
	ctx context.Context,
	in *pinsInput,
) (*jsonOutput[pinsResponse], error) {
	pins, err := s.db.ListPinnedMessages(ctx, "", in.Project)
	if err != nil {
		return nil, internalError("list pins", err)
	}
	if pins == nil {
		pins = []db.PinnedMessage{}
	}
	return &jsonOutput[pinsResponse]{Body: pinsResponse{Pins: pins}}, nil
}

func (s *Server) humaListSessionPins(
	ctx context.Context,
	in *idPathInput,
) (*jsonOutput[pinsResponse], error) {
	pins, err := s.db.ListPinnedMessages(ctx, in.ID, "")
	if err != nil {
		return nil, internalError("list session pins", err)
	}
	if pins == nil {
		pins = []db.PinnedMessage{}
	}
	return &jsonOutput[pinsResponse]{Body: pinsResponse{Pins: pins}}, nil
}

func (s *Server) humaPinMessage(ctx context.Context,
	in *pinMessageInput,
) (*createdOutput[pinMessageResponse], error) {
	id, err := s.db.PinMessage(ctx, in.ID, in.MessageID, in.Body.Note)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("pin message", err)
	}
	if id == 0 {
		return nil, apiError(http.StatusBadRequest,
			"message does not belong to this session")
	}
	return &createdOutput[pinMessageResponse]{
		Status: http.StatusCreated,
		Body:   pinMessageResponse{ID: id},
	}, nil
}

func (s *Server) humaUnpinMessage(ctx context.Context,
	in *messagePathInput,
) (*noContentOutput, error) {
	if err := s.db.UnpinMessage(ctx, in.ID, in.MessageID); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("unpin message", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}
