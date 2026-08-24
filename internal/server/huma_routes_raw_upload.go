package server

import (
	"context"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	rawSyncUploadOffsetHeader   = "Upload-Offset"
	rawSyncUploadLengthHeader   = "Upload-Length"
	rawSyncUploadCompleteHeader = "Upload-Complete"
	rawSyncUploadStartMaxBytes  = 4 << 10
	// Upload PATCH authentication runs before this route-specific extension.
	rawSyncUploadReadTimeout = 15 * time.Minute
	// Huma treats a body exactly equal to MaxBodyBytes as oversized, so the
	// transport read ceiling is one byte above the service's accepted chunk.
	rawSyncUploadReadMaxBytes = rawsync.DefaultUploadChunkBytes + 1
)

func (s *Server) registerRawUploadRoutes(group routeGroup) {
	group.register(
		http.MethodPost, "/uploads", "Create or resume a raw object upload",
		s.humaRawSyncUploadStart, s.humaTimeout(), maxBodyBytes(rawSyncUploadStartMaxBytes),
	)
	group.register(
		http.MethodHead, "/uploads/{upload_id}", "Read a raw upload offset",
		s.humaRawSyncUploadStatus, s.humaTimeout(),
	)
	group.register(
		http.MethodPatch, "/uploads/{upload_id}", "Append a raw upload chunk",
		s.humaRawSyncUploadAppend, s.humaReadDeadline(rawSyncUploadReadTimeout),
		maxBodyBytes(rawSyncUploadReadMaxBytes),
	)
}

type rawSyncUploadStartInput struct {
	Authorization string `header:"Authorization"`
	Body          struct {
		Provider parser.AgentType  `json:"provider"`
		Object   rawsync.ObjectRef `json:"object"`
	}
}

type rawSyncUploadResponse struct {
	UploadID  string            `json:"upload_id,omitempty"`
	Object    rawsync.ObjectRef `json:"object"`
	Offset    int64             `json:"offset"`
	Complete  bool              `json:"complete"`
	Created   bool              `json:"created"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
}

type rawSyncUploadStartOutput struct {
	Location string `header:"Location"`
	Body     rawSyncUploadResponse
}

func (s *Server) humaRawSyncUploadStart(
	ctx context.Context,
	in *rawSyncUploadStartInput,
) (*rawSyncUploadStartOutput, error) {
	identity, err := rawSyncIdentityFromContext(ctx)
	if err != nil {
		return nil, err
	}
	session, created, err := s.rawSyncUploads.Start(
		ctx, identity, in.Body.Provider, in.Body.Object,
	)
	if err != nil {
		return nil, rawSyncHTTPError(err)
	}
	location := ""
	if session.ID != "" {
		location = s.basePath + "/api/v1/raw-sync/uploads/" + session.ID
	}
	return &rawSyncUploadStartOutput{
		Location: location,
		Body:     rawSyncUploadResponseFromSession(session, created),
	}, nil
}

type rawSyncUploadStatusInput struct {
	Authorization string `header:"Authorization"`
	UploadID      string `path:"upload_id"`
}

type rawSyncUploadHeaders struct {
	UploadOffset   int64 `header:"Upload-Offset"`
	UploadLength   int64 `header:"Upload-Length"`
	UploadComplete bool  `header:"Upload-Complete"`
}

func (s *Server) humaRawSyncUploadStatus(
	ctx context.Context,
	in *rawSyncUploadStatusInput,
) (*rawSyncUploadHeaders, error) {
	identity, err := rawSyncIdentityFromContext(ctx)
	if err != nil {
		return nil, err
	}
	session, err := s.rawSyncUploads.Status(ctx, identity, in.UploadID)
	if err != nil {
		return nil, rawSyncHTTPError(err)
	}
	headers := rawSyncUploadHeadersFromSession(session)
	return &headers, nil
}

type rawSyncUploadAppendInput struct {
	Authorization string `header:"Authorization"`
	UploadID      string `path:"upload_id"`
	UploadOffset  int64  `header:"Upload-Offset" required:"true"`
	RawBody       []byte `contentType:"application/octet-stream"`
}

type rawSyncUploadAppendOutput struct {
	UploadOffset   int64 `header:"Upload-Offset"`
	UploadLength   int64 `header:"Upload-Length"`
	UploadComplete bool  `header:"Upload-Complete"`
	Body           rawSyncUploadResponse
}

func (s *Server) humaRawSyncUploadAppend(
	ctx context.Context,
	in *rawSyncUploadAppendInput,
) (*rawSyncUploadAppendOutput, error) {
	identity, err := rawSyncIdentityFromContext(ctx)
	if err != nil {
		return nil, err
	}
	session, err := s.rawSyncUploads.Append(
		ctx, identity, in.UploadID, in.UploadOffset, in.RawBody,
	)
	if err != nil {
		return nil, rawSyncHTTPError(err)
	}
	return &rawSyncUploadAppendOutput{
		UploadOffset: session.Offset, UploadLength: session.Object.Length,
		UploadComplete: session.Complete,
		Body:           rawSyncUploadResponseFromSession(session, false),
	}, nil
}

func rawSyncUploadHeadersFromSession(session rawsync.UploadSession) rawSyncUploadHeaders {
	return rawSyncUploadHeaders{
		UploadOffset:   session.Offset,
		UploadLength:   session.Object.Length,
		UploadComplete: session.Complete,
	}
}

func rawSyncUploadResponseFromSession(
	session rawsync.UploadSession,
	created bool,
) rawSyncUploadResponse {
	var expiresAt *time.Time
	if !session.ExpiresAt.IsZero() {
		value := session.ExpiresAt
		expiresAt = &value
	}
	return rawSyncUploadResponse{
		UploadID: session.ID, Object: session.Object, Offset: session.Offset,
		Complete: session.Complete, Created: created, ExpiresAt: expiresAt,
	}
}
