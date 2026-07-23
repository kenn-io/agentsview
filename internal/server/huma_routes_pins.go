package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
)

func (s *Server) registerPinRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Pins")

	get(s, group, "/pins", "List pins", s.humaListPins)
	get(s, group, "/sessions/{id}/pins", "List session pins", s.humaListSessionPins)
	post(s, group, "/sessions/{id}/messages/{messageId}/pin", "Pin message", s.humaPinMessage)
	deleteRoute(s, group, "/sessions/{id}/messages/{messageId}/pin", "Unpin message", s.humaUnpinMessage)
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

func (s *Server) humaPinMessage(
	ctx context.Context,
	in *pinMessageInput,
) (*createdOutput[pinMessageResponse], error) {
	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()

	var prior *db.PinnedMessage
	var pin *artifact.MetadataPin
	if s.metadata != nil {
		var err error
		prior, err = s.findPinnedMessage(ctx, in.ID, in.MessageID)
		if err != nil {
			return nil, internalError("pin message prior state", err)
		}
		pin, err = s.metadataPinForMessage(ctx, in.ID, in.MessageID, in.Body.Note)
		if err != nil {
			return nil, internalError("pin message metadata lookup", err)
		}
	}
	id, err := s.db.PinMessage(in.ID, in.MessageID, in.Body.Note)
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
	if pin != nil {
		if err := s.appendMetadataEvent(ctx, artifact.MetadataEventInput{
			SessionID: in.ID,
			Op:        artifact.MetadataOpPin,
			Pin:       pin,
		}); err != nil {
			var publishedErr *artifact.MetadataPublishedError
			if errors.As(err, &publishedErr) {
				return nil, internalError("pin message metadata event", err)
			}
			return nil, internalError("pin message metadata event",
				s.restorePinState(in.ID, in.MessageID, prior, err))
		}
	}
	return &createdOutput[pinMessageResponse]{
		Status: http.StatusCreated,
		Body:   pinMessageResponse{ID: id},
	}, nil
}

func (s *Server) humaUnpinMessage(
	ctx context.Context,
	in *messagePathInput,
) (*noContentOutput, error) {
	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()

	var prior *db.PinnedMessage
	var pin *artifact.MetadataPin
	if s.metadata != nil {
		var err error
		prior, err = s.findPinnedMessage(ctx, in.ID, in.MessageID)
		if err != nil {
			return nil, internalError("unpin message prior state", err)
		}
		pin, err = s.metadataPinForMessage(ctx, in.ID, in.MessageID, nil)
		if err != nil {
			return nil, internalError("unpin message metadata lookup", err)
		}
	}
	if err := s.db.UnpinMessage(in.ID, in.MessageID); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("unpin message", err)
	}
	if pin != nil {
		if err := s.appendMetadataEvent(ctx, artifact.MetadataEventInput{
			SessionID: in.ID,
			Op:        artifact.MetadataOpUnpin,
			Pin:       pin,
		}); err != nil {
			var publishedErr *artifact.MetadataPublishedError
			if errors.As(err, &publishedErr) {
				return nil, internalError("unpin message metadata event", err)
			}
			return nil, internalError("unpin message metadata event",
				s.restorePinState(in.ID, in.MessageID, prior, err))
		}
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

// findPinnedMessage returns the current pinned_messages row for the
// message, or nil when the message is not pinned.
func (s *Server) findPinnedMessage(
	ctx context.Context, sessionID string, messageID int64,
) (*db.PinnedMessage, error) {
	pins, err := s.db.ListPinnedMessages(ctx, sessionID, "")
	if err != nil {
		return nil, err
	}
	for i := range pins {
		if pins[i].MessageID == messageID {
			return &pins[i], nil
		}
	}
	return nil, nil
}

// restorePinState puts the pinned_messages row for the message back to
// prior after a pre-publish metadata failure, so local pin state never
// diverges from the durable ledger. It returns baseErr joined with any
// restore failure.
func (s *Server) restorePinState(
	sessionID string, messageID int64, prior *db.PinnedMessage, baseErr error,
) error {
	if prior != nil {
		if _, err := s.db.PinMessage(sessionID, messageID, prior.Note); err != nil {
			return errors.Join(baseErr,
				fmt.Errorf("restore pin after metadata failure: %w", err))
		}
		return baseErr
	}
	if err := s.db.UnpinMessage(sessionID, messageID); err != nil {
		return errors.Join(baseErr,
			fmt.Errorf("remove pin after metadata failure: %w", err))
	}
	return baseErr
}
