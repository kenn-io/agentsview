package servicehttp

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/service"
)

func (b *httpBackend) FileReferences(ctx context.Context, in service.FileReferencesInput) (*service.FileReferencesResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	in, err := service.NormalizeFileReferencesInput(in)
	if err != nil {
		return nil, err
	}
	query := &apiclient.GetAPIV1SessionsIDFileReferencesQuery{Binding: new(in.Binding), OccurrenceID: new(in.OccurrenceID), Offset: new(int64(in.Offset)), Limit: new(int64(in.Limit)), ReferenceOffset: new(int64(in.ReferenceOffset)), Refresh: new(in.Refresh)}
	if in.FirstOrdinal != nil {
		query.FirstOrdinal = new(int64(*in.FirstOrdinal))
		query.LastOrdinal = new(int64(*in.LastOrdinal))
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionsIDFileReferencesWithResponse(ctx, &apiclient.GetAPIV1SessionsIDFileReferencesRequestOptions{PathParams: &apiclient.GetAPIV1SessionsIDFileReferencesPath{ID: url.PathEscape(in.SessionID)}, Query: query, Header: &apiclient.GetAPIV1SessionsIDFileReferencesHeaders{XAgentsViewLookupIntent: new(service.FileReferencesIntentValue)}})
	if response == nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusConflict:
		return nil, service.ErrSourceChanged
	case http.StatusNotFound:
		return nil, service.ErrFileReferencesNotFound
	case http.StatusBadRequest:
		return nil, service.ErrFileReferencesInput
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}
	return response.JSON200, nil
}
