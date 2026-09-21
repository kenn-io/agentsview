package apiclient

import (
	"context"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
)

// RawRequest invokes one generated operation without consuming its response.
// The caller owns status handling, read limits, decoding, and closing Body.
func RawRequest(baseURL string, httpClient *http.Client, operation func(*Client) error, editors ...runtime.RequestEditorFn) (*http.Response, error) {
	options := make([]runtime.APIClientOption, 0, len(editors))
	for _, editor := range editors {
		options = append(options, runtime.WithRequestEditorFn(editor))
	}
	builder, err := runtime.NewAPIClient(baseURL, options...)
	if err != nil {
		return nil, err
	}
	transport := &rawTransport{APIClient: builder, client: httpClient}
	err = operation(NewClient(transport))
	// Generated envelopes may report an HTTP status error. The raw consumer
	// interprets that status and reads the original error body itself.
	if transport.response != nil {
		return transport.response, nil
	}
	return nil, err
}

type rawTransport struct {
	runtime.APIClient
	client   *http.Client
	response *http.Response
}

func (t *rawTransport) ExecuteRequest(ctx context.Context, req *http.Request, _ string) (*runtime.Response, error) {
	response, err := t.client.Do(req.WithContext(ctx)) //nolint:bodyclose // RawRequest transfers the open response to its caller.
	if err != nil {
		return nil, err
	}
	t.response = response
	return &runtime.Response{StatusCode: response.StatusCode, Headers: response.Header, Raw: response}, nil
}
