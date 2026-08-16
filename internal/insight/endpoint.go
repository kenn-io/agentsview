package insight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

const maxEndpointResponseBytes int64 = 2_000_000

// EndpointConfig is the immutable, validated endpoint choice for one call.
type EndpointConfig struct {
	Endpoint  string
	Model     string
	APIKey    string
	AllowHTTP bool
}

type endpointMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type endpointRequest struct {
	Model    string            `json:"model"`
	Messages []endpointMessage `json:"messages"`
	Stream   bool              `json:"stream"`
}

type endpointResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func generateEndpoint(ctx context.Context, cfg EndpointConfig, prompt string) (Result, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || u.User != nil {
		return Result{}, errorsEndpoint("invalid endpoint")
	}
	if err := config.ValidateExtractTransport(u, cfg.AllowHTTP); err != nil {
		return Result{}, errorsEndpoint("invalid endpoint")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	u.RawPath = ""

	requestBody := endpointRequest{Model: cfg.Model, Stream: false}
	requestBody.Messages = append(requestBody.Messages, endpointMessage{Role: "user", Content: prompt})
	body, err := json.Marshal(requestBody)
	if err != nil {
		return Result{}, errorsEndpoint("encode request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, errorsEndpoint("create request")
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, errorsEndpoint("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Result{}, fmt.Errorf("openai endpoint returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEndpointResponseBytes+1))
	if err != nil {
		return Result{}, errorsEndpoint("read response")
	}
	if int64(len(data)) > maxEndpointResponseBytes {
		return Result{}, errorsEndpoint("response too large")
	}
	var decoded endpointResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return Result{}, errorsEndpoint("invalid response")
	}
	if len(decoded.Choices) == 0 {
		return Result{}, errorsEndpoint("response has no choices")
	}
	if decoded.Choices[0].Message.Role != "assistant" {
		return Result{}, errorsEndpoint("response message role is invalid")
	}
	content, ok := decoded.Choices[0].Message.Content.(string)
	if !ok || strings.TrimSpace(content) == "" {
		return Result{}, errorsEndpoint("response content is empty or invalid")
	}
	model := decoded.Model
	if model == "" {
		model = cfg.Model
	}
	return Result{Content: content, Agent: "openai", Model: model}, nil
}

func errorsEndpoint(reason string) error {
	return fmt.Errorf("openai endpoint error: %s", reason)
}
