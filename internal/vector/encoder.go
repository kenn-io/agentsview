// Package vector wires agentsview into kit's vector package for semantic
// search: SQLite-backed vector storage and OpenAI-compatible embeddings.
package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	kitvec "go.kenn.io/kit/vector"
)

// EncoderConfig configures an OpenAI-compatible embeddings HTTP client.
type EncoderConfig struct {
	// Endpoint is the base URL including "/v1"; "/embeddings" is appended.
	Endpoint string
	// APIKey is sent as a Bearer token when non-empty. Empty means
	// anonymous, unauthenticated requests.
	APIKey string
	// Model is the embeddings model name sent in the request body.
	Model string
	// Dimension is the length every returned vector must have.
	Dimension int
	// Timeout bounds each individual HTTP request.
	Timeout time.Duration
	// MaxRetries is the maximum total attempts on 429/5xx/network errors
	// (4xx fails fast); values <= 0 mean one attempt.
	MaxRetries int
}

const (
	backoffBase = 250 * time.Millisecond
	backoffMax  = 5 * time.Second
)

// embeddingsRequestBody is the OpenAI-compatible embeddings request.
type embeddingsRequestBody struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embeddingsResponseBody is the OpenAI-compatible embeddings response.
type embeddingsResponseBody struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// NewEncoder returns a kitvec.EncodeFunc that POSTs to an OpenAI-compatible
// embeddings endpoint. Each invocation of the returned func makes exactly
// one HTTP call; batching and concurrency are the caller's responsibility
// via kitvec.EncodeBatched.
func NewEncoder(cfg EncoderConfig) kitvec.EncodeFunc {
	url := strings.TrimRight(cfg.Endpoint, "/") + "/embeddings"
	client := &http.Client{Timeout: cfg.Timeout}

	return func(ctx context.Context, texts []string) ([][]float32, error) {
		return encode(ctx, client, url, cfg, texts)
	}
}

// encode performs the retrying HTTP call and validates the response shape.
func encode(
	ctx context.Context, client *http.Client, url string, cfg EncoderConfig, texts []string,
) ([][]float32, error) {
	reqBody, err := json.Marshal(embeddingsRequestBody{Model: cfg.Model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("[vector.embeddings] marshal request: %w", err)
	}

	attempts := cfg.MaxRetries
	if attempts <= 0 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		vectors, retryable, err := attemptEncode(ctx, client, url, cfg, reqBody, texts)
		if err == nil {
			return vectors, nil
		}
		lastErr = err
		if !retryable || attempt == attempts {
			return nil, lastErr
		}
		if err := sleepBackoff(ctx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// attemptEncode makes a single HTTP request and decodes the response. The
// retryable return value indicates whether the error is worth retrying
// (429, 5xx, or a transport-level failure).
func attemptEncode(
	ctx context.Context, client *http.Client, url string, cfg EncoderConfig,
	reqBody []byte, texts []string,
) ([][]float32, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, false, fmt.Errorf("[vector.embeddings] build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("[vector.embeddings] request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retryable, fmt.Errorf(
			"[vector.embeddings] status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded embeddingsResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, false, fmt.Errorf("[vector.embeddings] decode response: %w", err)
	}

	vectors, err := reorderAndValidate(decoded, texts, cfg.Dimension)
	if err != nil {
		return nil, false, err
	}
	return vectors, false, nil
}

// reorderAndValidate reorders the decoded embeddings by their reported
// index and validates counts and dimensions against the request.
func reorderAndValidate(
	decoded embeddingsResponseBody, texts []string, dimension int,
) ([][]float32, error) {
	if len(decoded.Data) != len(texts) {
		return nil, fmt.Errorf(
			"[vector.embeddings] count mismatch: got %d embeddings, want %d",
			len(decoded.Data), len(texts))
	}

	out := make([][]float32, len(texts))
	seen := make([]bool, len(texts))
	for _, d := range decoded.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf(
				"[vector.embeddings] index %d out of range for %d texts", d.Index, len(texts))
		}
		if len(d.Embedding) != dimension {
			return nil, fmt.Errorf(
				"[vector.embeddings] dimension mismatch at index %d: got %d, want %d",
				d.Index, len(d.Embedding), dimension)
		}
		out[d.Index] = d.Embedding
		seen[d.Index] = true
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("[vector.embeddings] missing embedding for index %d", i)
		}
	}
	return out, nil
}

// sleepBackoff waits with capped exponential backoff before the next
// attempt, returning ctx.Err() promptly if ctx is cancelled during the
// wait.
func sleepBackoff(ctx context.Context, attempt int) error {
	delay := backoffBase << (attempt - 1)
	if delay > backoffMax || delay <= 0 {
		delay = backoffMax
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
