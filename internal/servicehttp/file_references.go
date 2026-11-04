package servicehttp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/service"
)

func (b *httpBackend) FileReferences(ctx context.Context, id string) (service.FileReferencesResult, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return service.FileReferencesResult{}, err
	}
	response, err := api.GetAPIV1SessionsIDFileReferencesWithResponse(ctx, &apiclient.GetAPIV1SessionsIDFileReferencesRequestOptions{PathParams: &apiclient.GetAPIV1SessionsIDFileReferencesPath{ID: url.PathEscape(id)}})
	if response == nil {
		return service.FileReferencesResult{}, err
	}
	if response.HTTPResponse.StatusCode == http.StatusNotFound {
		// Keep the transport marker so callers treat a missing session as a
		// rejected request rather than a failed daemon.
		return service.FileReferencesResult{}, fmt.Errorf("%w: %w", service.ErrSessionNotFound, errHTTPNotFound)
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return service.FileReferencesResult{}, err
	}
	if response.JSON200 == nil {
		return service.FileReferencesResult{}, errors.New("file references: empty response")
	}
	return *response.JSON200, nil
}
