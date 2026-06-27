package server

import (
	"context"
	"encoding/json"
	"fmt"

	"go.kenn.io/agentsview/internal/artifact"
)

func (s *Server) appendMetadataEvent(
	ctx context.Context,
	input artifact.MetadataEventInput,
) error {
	if artifact.MetadataEventsSuppressed(ctx) || s.metadata == nil {
		return nil
	}
	_, err := s.metadata.Append(ctx, input)
	return err
}

func (s *Server) repairLocalMetadataEvent(
	ctx context.Context,
	input artifact.MetadataEventInput,
) (int, error) {
	if s.metadata == nil {
		return 0, nil
	}
	return s.metadata.RepairLocalSessionMetadata(ctx, input.SessionID, input.Op)
}

type metadataReplayStateStore interface {
	MetadataReplayStateOp(ctx context.Context, sessionGID string, field string) (string, bool, error)
}

func (s *Server) metadataReplayStateOp(
	ctx context.Context,
	sessionID string,
	field string,
) (string, bool, error) {
	store, ok := s.db.(metadataReplayStateStore)
	if !ok {
		return "", false, nil
	}
	origin := s.localArtifactOrigin()
	if origin == "" {
		return "", false, nil
	}
	return store.MetadataReplayStateOp(ctx, artifact.MetadataSessionGID(origin, sessionID), field)
}

func renameMetadataValue(displayName *string) (json.RawMessage, error) {
	data, err := json.Marshal(struct {
		DisplayName *string `json:"display_name"`
	}{DisplayName: displayName})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func (s *Server) metadataPinForMessage(
	ctx context.Context,
	sessionID string,
	messageID int64,
	note *string,
) (*artifact.MetadataPin, error) {
	msgs, err := s.db.GetAllMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("loading message for metadata pin: %w", err)
	}
	for _, msg := range msgs {
		if msg.ID != messageID {
			continue
		}
		pin := &artifact.MetadataPin{
			SourceUUID: msg.SourceUUID,
			Ordinal:    msg.Ordinal,
		}
		if note != nil {
			noteCopy := *note
			pin.Note = &noteCopy
		}
		return pin, nil
	}
	return nil, nil
}
