package servicehttp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

var _ service.SessionAnnotator = (*httpBackend)(nil)

func (b *httpBackend) UpdateSessionLabels(
	ctx context.Context, id string, add, remove []string, replace bool,
) (*db.SessionLabels, error) {
	api, err := b.annotationClient(ctx, "label")
	if err != nil {
		return nil, err
	}
	response, err := api.PatchAPIV1SessionsIDLabelsWithResponse(ctx,
		&apiclient.PatchAPIV1SessionsIDLabelsRequestOptions{
			PathParams: &apiclient.PatchAPIV1SessionsIDLabelsPath{ID: url.PathEscape(id)},
			Body: &apiclient.PatchAPIV1SessionsIDLabelsBody{
				Add: add, Remove: remove, Clear: &replace,
			},
		})
	if response == nil {
		return nil, err
	}
	return annotationResult(response.HTTPResponse, response.Body, err, response.JSON200)
}

func (b *httpBackend) SetSessionParent(
	ctx context.Context, id, parentID string,
) (*db.SessionExternalParent, error) {
	api, err := b.annotationClient(ctx, "parent")
	if err != nil {
		return nil, err
	}
	parentID = strings.TrimSpace(parentID)
	response, err := api.PutAPIV1SessionsIDParentWithResponse(ctx,
		&apiclient.PutAPIV1SessionsIDParentRequestOptions{
			PathParams: &apiclient.PutAPIV1SessionsIDParentPath{ID: url.PathEscape(id)},
			Body:       &apiclient.PutAPIV1SessionsIDParentBody{ParentSessionID: &parentID},
		})
	if response == nil {
		return nil, err
	}
	link, err := annotationResult(response.HTTPResponse, response.Body, err, response.JSON200)
	if parentID == "" && errors.Is(err, errHTTPNotFound) {
		return nil, service.ErrNoSessionParent
	}
	return link, err
}

// annotationClient refuses read-only daemons and servers that predate the
// annotation endpoints before returning the generated client.
func (b *httpBackend) annotationClient(
	ctx context.Context, op string,
) (*apiclient.Client, error) {
	if b.readOnly {
		return nil, fmt.Errorf("%s: daemon at %s is read-only: %w",
			op, b.baseURL, db.ErrReadOnly)
	}
	if err := b.requireSessionAnnotations(ctx); err != nil {
		return nil, err
	}
	return b.apiClient(b.client)
}

func annotationResult[T any](
	response *http.Response, body []byte, err error, out *T,
) (*T, error) {
	if response != nil && response.StatusCode == http.StatusNotImplemented {
		return nil, fmt.Errorf("%s: %w; stop the read-only serve process and use the local DB, or start a local daemon", response.Request.URL.Path, db.ErrReadOnly)
	}
	if err := serviceResponseError(response, body, err); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("unexpected empty response from %s", response.Request.URL.Path)
	}
	return out, nil
}
