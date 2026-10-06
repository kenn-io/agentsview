package servicehttp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

var _ service.SessionAnnotator = (*httpBackend)(nil)

func (b *httpBackend) SessionLabels(
	ctx context.Context, id string,
) (*db.SessionLabels, error) {
	if err := b.requireSessionAnnotations(ctx); err != nil {
		return nil, err
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionsIDLabelsWithResponse(ctx,
		&apiclient.GetAPIV1SessionsIDLabelsRequestOptions{
			PathParams: &apiclient.GetAPIV1SessionsIDLabelsPath{ID: url.PathEscape(id)},
		})
	if response == nil {
		return nil, err
	}
	return annotationResult(response.HTTPResponse, response.Body, err, response.JSON200)
}

func (b *httpBackend) SetSessionLabels(
	ctx context.Context, id string, labels []string,
) (*db.SessionLabels, error) {
	if err := b.requireWritable("label"); err != nil {
		return nil, err
	}
	if err := b.requireSessionAnnotations(ctx); err != nil {
		return nil, err
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	if labels == nil {
		labels = []string{}
	}
	response, err := api.PutAPIV1SessionsIDLabelsWithResponse(ctx,
		&apiclient.PutAPIV1SessionsIDLabelsRequestOptions{
			PathParams: &apiclient.PutAPIV1SessionsIDLabelsPath{ID: url.PathEscape(id)},
			Body:       &apiclient.PutAPIV1SessionsIDLabelsBody{Labels: labels},
		})
	if response == nil {
		return nil, err
	}
	return annotationResult(response.HTTPResponse, response.Body, err, response.JSON200)
}

func (b *httpBackend) UpdateSessionLabels(
	ctx context.Context, id string, add, remove []string,
) (*db.SessionLabels, error) {
	if err := b.requireWritable("label"); err != nil {
		return nil, err
	}
	if err := b.requireSessionAnnotations(ctx); err != nil {
		return nil, err
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.PatchAPIV1SessionsIDLabelsWithResponse(ctx,
		&apiclient.PatchAPIV1SessionsIDLabelsRequestOptions{
			PathParams: &apiclient.PatchAPIV1SessionsIDLabelsPath{ID: url.PathEscape(id)},
			Body:       &apiclient.PatchAPIV1SessionsIDLabelsBody{Add: add, Remove: remove},
		})
	if response == nil {
		return nil, err
	}
	return annotationResult(response.HTTPResponse, response.Body, err, response.JSON200)
}

func (b *httpBackend) SessionParent(
	ctx context.Context, id string,
) (*db.SessionExternalParent, error) {
	if err := b.requireSessionAnnotations(ctx); err != nil {
		return nil, err
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1SessionsIDParentWithResponse(ctx,
		&apiclient.GetAPIV1SessionsIDParentRequestOptions{
			PathParams: &apiclient.GetAPIV1SessionsIDParentPath{ID: url.PathEscape(id)},
		})
	if response == nil {
		return nil, err
	}
	link, err := annotationResult(response.HTTPResponse, response.Body, err, response.JSON200)
	if errors.Is(err, errHTTPNotFound) {
		return nil, nil
	}
	return link, err
}

func (b *httpBackend) SetSessionParent(
	ctx context.Context, id, parentID string,
) (*db.SessionExternalParent, error) {
	if err := b.requireWritable("parent"); err != nil {
		return nil, err
	}
	if err := b.requireSessionAnnotations(ctx); err != nil {
		return nil, err
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	body := &apiclient.PutAPIV1SessionsIDParentBody{ParentSessionID: parentID}
	response, err := api.PutAPIV1SessionsIDParentWithResponse(ctx,
		&apiclient.PutAPIV1SessionsIDParentRequestOptions{
			PathParams: &apiclient.PutAPIV1SessionsIDParentPath{ID: url.PathEscape(id)},
			Body:       body,
		})
	if response == nil {
		return nil, err
	}
	return annotationResult(response.HTTPResponse, response.Body, err, response.JSON200)
}

func (b *httpBackend) ClearSessionParent(
	ctx context.Context, id string,
) (*db.SessionExternalParent, error) {
	if err := b.requireWritable("parent"); err != nil {
		return nil, err
	}
	if err := b.requireSessionAnnotations(ctx); err != nil {
		return nil, err
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	response, err := api.DeleteAPIV1SessionsIDParentWithResponse(ctx,
		&apiclient.DeleteAPIV1SessionsIDParentRequestOptions{
			PathParams: &apiclient.DeleteAPIV1SessionsIDParentPath{ID: url.PathEscape(id)},
		})
	if response == nil {
		return nil, err
	}
	link, err := annotationResult(response.HTTPResponse, response.Body, err, response.JSON200)
	if errors.Is(err, errHTTPNotFound) {
		return nil, service.ErrNoSessionParent
	}
	return link, err
}

func (b *httpBackend) requireWritable(op string) error {
	if b.readOnly {
		return fmt.Errorf("%s: daemon at %s is read-only: %w",
			op, b.baseURL, db.ErrReadOnly)
	}
	return nil
}

func annotationResult[T any](
	response *http.Response, body []byte, err error, out *T,
) (*T, error) {
	if err := serviceResponseError(response, body, err); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("unexpected empty response from %s", response.Request.URL.Path)
	}
	return out, nil
}
