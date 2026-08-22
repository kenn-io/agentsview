package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	rawSyncDeviceIDHeader        = "X-AgentsView-Device-ID"
	rawSyncAuthorizationMaxBytes = 128
	rawSyncTokenMaxBodyBytes     = 4 << 10
	rawSyncControlMaxBodyBytes   = 1 << 20
)

func (s *Server) registerRawSyncRoutes() {
	if s.rawSyncDeviceAuth == nil && !s.rawSyncSchemaOnly {
		return
	}
	group := newRouteGroup(s.api, "/api/v1/raw-sync", "RawSync")
	group.register(
		http.MethodPost, "/tokens", "Exchange a device credential",
		s.humaRawSyncToken, s.humaTimeout(), maxBodyBytes(rawSyncTokenMaxBodyBytes),
	)
	if s.rawSyncCustody == nil && !s.rawSyncSchemaOnly {
		return
	}
	group.register(
		http.MethodPost, "/objects/missing", "Negotiate missing raw objects",
		s.humaRawSyncMissingObjects, s.humaTimeout(),
		maxBodyBytes(rawSyncControlMaxBodyBytes),
	)
	group.register(
		http.MethodPost, "/manifests", "Commit a raw manifest",
		s.humaRawSyncManifest, s.humaTimeout(), maxBodyBytes(rawSyncControlMaxBodyBytes),
	)
}

type rawSyncTokenInput struct {
	Authorization string `header:"Authorization"`
	DeviceID      string `header:"X-AgentsView-Device-ID"`
	Body          struct {
		Scopes []string `json:"scopes"`
	}
}

type rawSyncTokenResponse struct {
	Token     string    `json:"token"`
	DeviceID  string    `json:"device_id"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) humaRawSyncToken(
	ctx context.Context,
	in *rawSyncTokenInput,
) (*jsonOutput[rawSyncTokenResponse], error) {
	scopes, err := rawsync.ParseDeviceTokenScopeNames(in.Body.Scopes)
	if err != nil {
		return nil, rawSyncHTTPError(err)
	}
	credential, err := rawSyncBearer(in.Authorization)
	if err != nil {
		return nil, rawSyncHTTPError(err)
	}
	identity, err := rawSyncIdentityFromContext(ctx)
	if err != nil {
		return nil, err
	}
	issued, err := s.rawSyncDeviceAuth.IssueToken(ctx, identity.DeviceID, credential, scopes)
	if err != nil {
		if errors.Is(err, rawsync.ErrUnauthorized) || errors.Is(err, rawsync.ErrInvalid) {
			return nil, rawSyncHTTPError(rawsync.ErrUnauthorized)
		}
		return nil, rawSyncHTTPError(err)
	}
	if issued.Identity != identity {
		return nil, rawSyncHTTPError(errors.New("raw sync token identity changed"))
	}
	return &jsonOutput[rawSyncTokenResponse]{Body: rawSyncTokenResponse{
		Token:     issued.Token,
		DeviceID:  issued.Identity.DeviceID,
		Scopes:    issued.Scopes.Names(),
		ExpiresAt: issued.ExpiresAt,
	}}, nil
}

type rawSyncMissingObjectsInput struct {
	Authorization string `header:"Authorization"`
	Body          struct {
		Provider parser.AgentType    `json:"provider"`
		Objects  []rawsync.ObjectRef `json:"objects"`
	}
}

type rawSyncMissingObjectsResponse struct {
	Missing []rawsync.ObjectRef `json:"missing"`
}

func (s *Server) humaRawSyncMissingObjects(
	ctx context.Context,
	in *rawSyncMissingObjectsInput,
) (*jsonOutput[rawSyncMissingObjectsResponse], error) {
	identity, err := rawSyncIdentityFromContext(ctx)
	if err != nil {
		return nil, err
	}
	missing, err := s.rawSyncCustody.MissingObjects(
		ctx, identity, in.Body.Provider, in.Body.Objects,
	)
	if err != nil {
		return nil, rawSyncHTTPError(err)
	}
	return &jsonOutput[rawSyncMissingObjectsResponse]{
		Body: rawSyncMissingObjectsResponse{Missing: missing},
	}, nil
}

type rawSyncManifestInput struct {
	Authorization string `header:"Authorization"`
	Body          rawsync.Manifest
}

type rawSyncManifestResponse struct {
	ManifestID string `json:"manifest_id"`
	Receipt    string `json:"receipt"`
	Generation int64  `json:"generation"`
	Created    bool   `json:"created"`
}

func (s *Server) humaRawSyncManifest(
	ctx context.Context,
	in *rawSyncManifestInput,
) (*jsonOutput[rawSyncManifestResponse], error) {
	identity, err := rawSyncIdentityFromContext(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.rawSyncCustody.CommitManifest(ctx, identity, in.Body)
	if err != nil {
		return nil, rawSyncHTTPError(err)
	}
	return &jsonOutput[rawSyncManifestResponse]{Body: rawSyncManifestResponse{
		ManifestID: result.ManifestID,
		Receipt:    result.Receipt,
		Generation: result.Generation,
		Created:    result.Created,
	}}, nil
}

func rawSyncIdentityFromContext(ctx context.Context) (rawsync.AuthIdentity, error) {
	identity, ok := ctx.Value(ctxKeyRawSyncIdentity).(rawsync.AuthIdentity)
	if !ok {
		return rawsync.AuthIdentity{}, rawSyncHTTPError(rawsync.ErrUnauthorized)
	}
	return identity, nil
}

func rawSyncBearer(value string) (string, error) {
	if len(value) > rawSyncAuthorizationMaxBytes {
		return "", rawsync.ErrUnauthorized
	}
	secret, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || secret == "" || strings.ContainsAny(secret, " \t\r\n") {
		return "", rawsync.ErrUnauthorized
	}
	return secret, nil
}

func rawSyncHTTPError(err error) error {
	if err == nil {
		return nil
	}
	var headConflict *rawsync.HeadConflictError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return apiError(http.StatusGatewayTimeout, "gateway timeout")
	case errors.Is(err, rawsync.ErrUnauthorized):
		return apiErrorWithCode(http.StatusUnauthorized, "unauthorized", "Unauthorized")
	case errors.As(err, &headConflict) && headConflict != nil:
		return &apiErrorResponse{
			Status:            http.StatusConflict,
			Code:              "head_conflict",
			Message:           "raw source head changed",
			CurrentManifestID: headConflict.CurrentManifestID,
			CurrentReceipt:    headConflict.CurrentReceipt,
			CurrentGeneration: headConflict.CurrentGeneration,
		}
	case errors.Is(err, rawsync.ErrMissingObject):
		return apiErrorWithCode(
			http.StatusConflict, "missing_object", "raw manifest references a missing object",
		)
	case errors.Is(err, rawsync.ErrConflict):
		return apiErrorWithCode(http.StatusConflict, "conflict", "raw sync conflict")
	case errors.Is(err, rawsync.ErrNotFound):
		return apiErrorWithCode(http.StatusNotFound, "not_found", "raw sync object not found")
	case errors.Is(err, rawsync.ErrInvalid):
		return apiErrorWithCode(http.StatusBadRequest, "invalid_request", "invalid raw sync request")
	default:
		return apiErrorWithCode(
			http.StatusInternalServerError, "internal_error", "raw sync request failed",
		)
	}
}
