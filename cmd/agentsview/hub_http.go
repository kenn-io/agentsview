package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/remotecred"
	"go.kenn.io/agentsview/internal/vector"
)

// httpHub uses the generated API with the remote's credential and redirect policy.
type httpHub struct {
	URL, Token string
	Client     *http.Client
}

func (h *httpHub) HubVectorGeneration(ctx context.Context) (out vector.HubGeneration, err error) {
	client, err := h.apiClient()
	if err != nil {
		return out, err
	}
	resp, err := client.GetAPIV1VectorsGenerationWithResponse(ctx, acceptHubEncoding)
	if resp == nil {
		return out, err
	}
	if err != nil {
		return out, errors.New("invalid hub vector response")
	}
	err = decodeHubResponse(resp.Body, &out)
	return
}

func (h *httpHub) LookupHubVectors(ctx context.Context, in vector.HubLookupRequest) (out vector.HubLookupReply, err error) {
	if err = in.Validate(); err != nil {
		return
	}
	client, err := h.apiClient()
	if err != nil {
		return out, err
	}
	body := apiclient.VectorHubLookupRequest{Fingerprint: in.Fingerprint, Cursor: &in.Cursor, Identities: make([]apiclient.VectorHubIdentity, len(in.Identities))}
	for i, id := range in.Identities {
		occurrence, ordinal := int64(id.Occurrence), int64(id.Ordinal)
		body.Identities[i] = apiclient.VectorHubIdentity{SessionID: id.SessionID, UnitKind: id.UnitKind, SourceUUID: &id.SourceUUID, Occurrence: &occurrence, Ordinal: &ordinal, ContentHash: id.ContentHash}
	}
	resp, err := client.PostAPIV1VectorsLookupWithResponse(ctx, &apiclient.PostAPIV1VectorsLookupRequestOptions{Body: &body}, acceptHubEncoding)
	if resp == nil {
		return out, err
	}
	if err != nil {
		return out, errors.New("invalid hub vector response")
	}
	err = decodeHubResponse(resp.Body, &out)
	return
}

func acceptHubEncoding(_ context.Context, req *http.Request) error {
	req.Header.Set("Accept-Encoding", "zstd")
	return nil
}

func decodeHubResponse(raw []byte, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("invalid hub vector response")
	}
	return nil
}

func (h *httpHub) apiClient() (*apiclient.Client, error) {
	if h.Client == nil {
		return nil, errors.New("remote vector HTTP client unavailable")
	}
	client, err := apiclient.NewHTTPClientWithResponsePolicy(h.URL, h.Token, h.Client, h.responsePolicy)
	if err != nil {
		return nil, errors.New("invalid remote vector client configuration")
	}
	return client, nil
}

// responsePolicy bounds decompression before the generated runtime buffers or
// decodes the response. Generated decode errors are replaced at the call site
// because they can retain untrusted response text.
func (h *httpHub) responsePolicy(resp *http.Response, err error) (*http.Response, error) {
	if err == nil && resp.StatusCode == http.StatusConflict {
		resp.Body.Close()
		return nil, vector.ErrHubGenerationChanged
	}
	resp, err = remotecred.ResponsePolicy(h.URL)(resp, err)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote %s vector endpoint unavailable (HTTP %d)", remotecred.Redact(h.URL), resp.StatusCode)
	}
	var reader io.Reader = resp.Body
	switch resp.Header.Get("Content-Encoding") {
	case "zstd":
		decoder, err := zstd.NewReader(resp.Body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			return nil, errors.New("invalid compressed hub vectors")
		}
		defer decoder.Close()
		reader = decoder
	case "", "identity":
	default:
		return nil, errors.New("unsupported hub vector encoding")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, vector.MaxHubLookupBytes+1))
	if err != nil {
		return nil, errors.New("reading hub vectors failed")
	}
	if len(raw) > vector.MaxHubLookupBytes {
		return nil, errors.New("hub vector response exceeds 32 MiB")
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	resp.Header.Del("Content-Encoding")
	return resp, nil
}
