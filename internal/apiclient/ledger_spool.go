package apiclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
)

// Keep this operation separate from client.gen.go so concurrent API additions
// do not compete for the same insertion points in the generated client.
type LedgerSpoolIngestInputBody struct {
	Zone *string `json:"zone,omitempty"`
}

type PostAPIV1LedgerSpoolIngestBody = LedgerSpoolIngestInputBody

type PostAPIV1LedgerSpoolIngestRequestOptions struct {
	Body *PostAPIV1LedgerSpoolIngestBody
}

func (o *PostAPIV1LedgerSpoolIngestRequestOptions) Validate() error {
	return nil
}

func (o *PostAPIV1LedgerSpoolIngestRequestOptions) GetPathParams() (map[string]any, error) {
	return nil, nil
}

func (o *PostAPIV1LedgerSpoolIngestRequestOptions) GetQuery() (map[string]any, error) {
	return nil, nil
}

func (o *PostAPIV1LedgerSpoolIngestRequestOptions) GetBody() any {
	return o.Body
}

func (o *PostAPIV1LedgerSpoolIngestRequestOptions) GetHeader() (map[string]string, error) {
	return nil, nil
}

type LedgerspoolrunFailure struct {
	ErrorData string `json:"error" validate:"required"`
	File      string `json:"file" validate:"required"`
}

func (l LedgerspoolrunFailure) Validate() error {
	return runtime.ConvertValidatorError(typesValidator.Struct(l))
}

type LedgerspoolrunLedgerSpoolIngestRun struct {
	Zones []LedgerspoolrunZoneIngest `json:"zones" validate:"required"`
}

func (l LedgerspoolrunLedgerSpoolIngestRun) Validate() error {
	var errors runtime.ValidationErrors
	for i, item := range l.Zones {
		if v, ok := any(item).(runtime.Validator); ok {
			if err := v.Validate(); err != nil {
				errors = errors.Append(fmt.Sprintf("Zones[%d]", i), err)
			}
		}
	}
	if len(errors) == 0 {
		return nil
	}
	return errors
}

type LedgerspoolrunZoneIngest struct {
	Committed []string                `json:"committed" validate:"required"`
	Failed    []LedgerspoolrunFailure `json:"failed" validate:"required"`
	Skipped   []string                `json:"skipped" validate:"required"`
	State     string                  `json:"state" validate:"required"`
	Zone      string                  `json:"zone" validate:"required"`
}

func (l LedgerspoolrunZoneIngest) Validate() error {
	var errors runtime.ValidationErrors
	if err := typesValidator.Var(l.Committed, "required"); err != nil {
		errors = errors.Append("Committed", err)
	}
	for i, item := range l.Failed {
		if v, ok := any(item).(runtime.Validator); ok {
			if err := v.Validate(); err != nil {
				errors = errors.Append(fmt.Sprintf("Failed[%d]", i), err)
			}
		}
	}
	if err := typesValidator.Var(l.Skipped, "required"); err != nil {
		errors = errors.Append("Skipped", err)
	}
	if err := typesValidator.Var(l.State, "required"); err != nil {
		errors = errors.Append("State", err)
	}
	if err := typesValidator.Var(l.Zone, "required"); err != nil {
		errors = errors.Append("Zone", err)
	}
	if len(errors) == 0 {
		return nil
	}
	return errors
}

type PostAPIV1LedgerSpoolIngestResponse = LedgerspoolrunLedgerSpoolIngestRun

type PostAPIV1LedgerSpoolIngestResp struct {
	HTTPResponse *http.Response
	Body         []byte
	StatusCode   int
	JSON200      *PostAPIV1LedgerSpoolIngestResponse
	JSON400      *APIErrorResponse
	JSON401      *APIErrorResponse
	JSON403      *APIErrorResponse
	JSON404      *APIErrorResponse
	JSON409      *APIErrorResponse
	JSON422      *APIErrorResponse
	JSON500      *APIErrorResponse
	JSON501      *APIErrorResponse
	JSON502      *APIErrorResponse
	JSON503      *APIErrorResponse
	JSON504      *APIErrorResponse
}

func (c *Client) PostAPIV1LedgerSpoolIngestWithResponse(
	ctx context.Context,
	options *PostAPIV1LedgerSpoolIngestRequestOptions,
	reqEditors ...runtime.RequestEditorFn,
) (*PostAPIV1LedgerSpoolIngestResp, error) {
	const path = "/api/v1/ledger/spool/ingest"
	reqParams := runtime.RequestOptionsParameters{
		RequestURL:  c.apiClient.GetBaseURL() + path,
		Method:      http.MethodPost,
		Options:     options,
		ContentType: "application/json",
	}
	req, err := c.apiClient.CreateRequest(ctx, reqParams, reqEditors...)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}
	resp, err := c.apiClient.ExecuteRequest(ctx, req, path)
	if err != nil {
		return nil, fmt.Errorf("error executing request: %w", err)
	}
	if resp == nil {
		return nil, nil
	}

	out := &PostAPIV1LedgerSpoolIngestResp{
		HTTPResponse: resp.Raw,
		Body:         resp.Content,
		StatusCode:   resp.StatusCode,
	}
	switch resp.StatusCode {
	case http.StatusOK:
		out.JSON200 = new(PostAPIV1LedgerSpoolIngestResponse)
		if len(resp.Content) > 0 {
			if err := json.Unmarshal(resp.Content, out.JSON200); err != nil {
				return out, ledgerSpoolIngestDecodeError(resp, "PostAPIV1LedgerSpoolIngestResponse", err)
			}
		}
		return out, nil
	case http.StatusBadRequest:
		out.JSON400 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponse", out.JSON400)
	case http.StatusUnauthorized:
		out.JSON401 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON", out.JSON401)
	case http.StatusForbidden:
		out.JSON403 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON403", out.JSON403)
	case http.StatusNotFound:
		out.JSON404 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON404", out.JSON404)
	case http.StatusConflict:
		out.JSON409 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON409", out.JSON409)
	case http.StatusUnprocessableEntity:
		out.JSON422 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON422", out.JSON422)
	case http.StatusInternalServerError:
		out.JSON500 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON500", out.JSON500)
	case http.StatusNotImplemented:
		out.JSON501 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON501", out.JSON501)
	case http.StatusBadGateway:
		out.JSON502 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON502", out.JSON502)
	case http.StatusServiceUnavailable:
		out.JSON503 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON503", out.JSON503)
	case http.StatusGatewayTimeout:
		out.JSON504 = new(APIErrorResponse)
		return out, decodeLedgerSpoolIngestAPIError(resp, "PostAPIV1LedgerSpoolIngestErrorResponseJSON504", out.JSON504)
	default:
		return out, runtime.NewClientAPIError(
			fmt.Errorf("unexpected status code: %d", resp.StatusCode),
			runtime.WithStatusCode(resp.StatusCode),
		)
	}
}

func decodeLedgerSpoolIngestAPIError(resp *runtime.Response, targetType string, target *APIErrorResponse) error {
	if len(resp.Content) > 0 {
		if err := json.Unmarshal(resp.Content, target); err != nil {
			return ledgerSpoolIngestDecodeError(resp, targetType, err)
		}
	}
	return runtime.NewClientAPIError(
		fmt.Errorf("API error (status %d)", resp.StatusCode),
		runtime.WithStatusCode(resp.StatusCode),
	)
}

func ledgerSpoolIngestDecodeError(resp *runtime.Response, targetType string, err error) error {
	return &runtime.ResponseDecodeError{
		StatusCode:    resp.StatusCode,
		ContentType:   resp.Headers.Get("Content-Type"),
		ContentLength: len(resp.Content),
		TargetType:    targetType,
		Body:          resp.Content,
		Err:           err,
	}
}
