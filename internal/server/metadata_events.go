package server

import (
	"context"
	"encoding/json"
	"fmt"

	"go.kenn.io/agentsview/internal/artifact"
)

type metadataArtifactLeaseKey struct{}

type metadataArtifactLease struct {
	server *Server
}

func (s *Server) acquireMetadataArtifactLease(
	ctx context.Context,
) (context.Context, func(), error) {
	if s.metadata == nil {
		return ctx, func() {}, nil
	}
	if lease, ok := ctx.Value(metadataArtifactLeaseKey{}).(*metadataArtifactLease); ok &&
		lease.server == s {
		return ctx, func() {}, nil
	}
	_, release, err := s.artifactOps.acquire()
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, metadataArtifactLeaseKey{}, &metadataArtifactLease{
		server: s,
	}), release, nil
}

func (s *Server) appendMetadataEvent(
	ctx context.Context,
	input artifact.MetadataEventInput,
) error {
	if artifact.MetadataEventsSuppressed(ctx) {
		return nil
	}
	if s.metadataAppend != nil {
		return s.metadataAppend(ctx, input)
	}
	if s.metadata == nil {
		return nil
	}
	ctx, release, err := s.acquireMetadataArtifactLease(ctx)
	if err != nil {
		return fmt.Errorf("acquiring artifact store for metadata append: %w", err)
	}
	defer release()
	_, err = s.metadata.Append(ctx, input)
	return err
}

func (s *Server) repairLocalMetadataEvent(
	ctx context.Context,
	input artifact.MetadataEventInput,
) (int, error) {
	if s.metadata == nil {
		return 0, nil
	}
	ctx, release, err := s.acquireMetadataArtifactLease(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquiring artifact store for metadata repair: %w", err)
	}
	defer release()
	return s.metadata.RepairLocalSessionMetadata(ctx, input.SessionID, input.Op)
}

func (s *Server) ensureLocalMetadataEvent(
	ctx context.Context,
	input artifact.MetadataEventInput,
	field string,
	wantOp string,
) error {
	if s.metadata == nil {
		if s.metadataAppend != nil {
			return s.appendMetadataEvent(ctx, input)
		}
		return nil
	}
	ctx, release, err := s.acquireMetadataArtifactLease(ctx)
	if err != nil {
		return fmt.Errorf("acquiring artifact store for metadata ensure: %w", err)
	}
	defer release()
	if _, err := s.repairLocalMetadataEvent(ctx, input); err != nil {
		return err
	}
	op, ok, err := s.metadataReplayStateOp(ctx, input.SessionID, field)
	if err != nil {
		return err
	}
	if ok && op == wantOp {
		return nil
	}
	return s.appendMetadataEvent(ctx, input)
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
	msg, err := s.db.GetMessageForMetadataPin(ctx, sessionID, messageID)
	if err != nil {
		return nil, fmt.Errorf("loading message for metadata pin: %w", err)
	}
	if msg == nil {
		return nil, nil
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
