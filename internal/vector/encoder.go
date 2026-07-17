// Package vector wires agentsview into kit's vector package for semantic
// search: SQLite-backed vector storage and OpenAI-compatible embeddings.
package vector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
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
	// RequestDimensions, when true, sends Dimension as the OpenAI-compatible
	// "dimensions" request field, asking the endpoint for Matryoshka-reduced
	// vectors of exactly that length. When false (the default) the field is
	// omitted — for models served at their native dimension and endpoints
	// that do not support dimension selection — and Dimension only validates
	// response length.
	RequestDimensions bool
	// Timeout bounds each individual HTTP request.
	Timeout time.Duration
	// MaxRetries is the maximum total attempts on 429/5xx/network errors
	// (4xx fails fast); values <= 0 mean one attempt.
	MaxRetries int
	// InputPrefix is prepended verbatim to every input text before it is
	// sent. Callers use distinct encoder instances when query and document
	// inputs require different task instructions. Empty means no prefix.
	InputPrefix string
	// InputSuffix is appended verbatim to every input text before it is
	// sent, for models that expect a terminator the serving layer does not
	// add (e.g. "<|endoftext|>" for Qwen3-Embedding under llama.cpp).
	// Empty means inputs are sent unmodified.
	InputSuffix string
}

const (
	backoffBase = 250 * time.Millisecond
	backoffMax  = 5 * time.Second
	// retryAfterCap bounds how long a Retry-After header can push a single
	// wait, so a misbehaving or hostile endpoint cannot stall a build for
	// arbitrarily long.
	retryAfterCap = 60 * time.Second
)

// HTTPStatusError reports a non-200 response from the embeddings endpoint.
// It carries the status code so callers can distinguish retryable failures
// (429, 5xx) from non-retryable ones, and, for known input-specific 4xx
// rejections, skip-stamp one poison document without aborting a whole build.
// Route/model/schema/config failures are not input-specific and must abort so
// later builds can retry the corpus after the configuration is fixed. When the
// response is a 429 and carried a parseable Retry-After header, RetryAfter
// holds the delay the server asked for (clamped to retryAfterCap) so retry
// backoff can honor it instead of guessing.
type HTTPStatusError struct {
	// Status is the HTTP status code the embeddings endpoint returned.
	Status int
	// Body is a trimmed snippet (up to 512 bytes) of the response body.
	Body string
	// RetryAfter is the parsed Retry-After delay from a 429 response, or
	// nil when the response carried none or it could not be parsed.
	RetryAfter *time.Duration
}

// InvalidEmbeddingError reports endpoint output that has the expected shape
// but cannot participate in cosine distance. Index identifies the embedding
// within the response batch; Component is -1 for a zero-norm vector.
type InvalidEmbeddingError struct {
	Index     int
	Component int
	Reason    string
}

func (e *InvalidEmbeddingError) Error() string {
	if e.Component >= 0 {
		return fmt.Sprintf(
			"[vector.embeddings] invalid embedding at index %d component %d: %s",
			e.Index, e.Component, e.Reason)
	}
	return fmt.Sprintf(
		"[vector.embeddings] invalid embedding at index %d: %s", e.Index, e.Reason)
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("[vector.embeddings] status %d: %s", e.Status, e.Body)
}

// Permanent reports whether the embeddings endpoint's response indicates a
// rejection of this specific input that will never succeed on retry. It is
// intentionally conservative: generic 4xx statuses often mean a bad route,
// model, media type, or credentials, and skip-stamping those would silently
// mark an entire corpus embedded-with-no-vectors.
func (e *HTTPStatusError) Permanent() bool {
	switch e.Status {
	case http.StatusBadRequest,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity:
		return hasDocumentSpecificEmbeddingError(e.Body)
	}
	return false
}

// hasDocumentSpecificEmbeddingError reports whether an embeddings error body
// describes a rejection of the input document itself: an input/token/context
// length overflow, or a content-policy refusal. Bare keywords are not enough
// — "invalid token" is an auth failure and "unsupported content type" is a
// media-type failure, and skip-stamping those would silently mark the whole
// corpus embedded-with-no-vectors — so a size word must pair with an input
// word, and "content" must pair with "policy".
func hasDocumentSpecificEmbeddingError(body string) bool {
	body = strings.ToLower(body)
	if strings.Contains(body, "content") && strings.Contains(body, "policy") {
		return true
	}
	overLimit := strings.Contains(body, "too long") ||
		strings.Contains(body, "too large") ||
		strings.Contains(body, "too many") ||
		strings.Contains(body, "length") ||
		strings.Contains(body, "limit") ||
		strings.Contains(body, "maximum") ||
		strings.Contains(body, "exceed") ||
		strings.Contains(body, "overflow")
	if !overLimit {
		return false
	}
	return strings.Contains(body, "token") ||
		strings.Contains(body, "context") ||
		strings.Contains(body, "input") ||
		strings.Contains(body, "text")
}

// embeddingsRequestBody is the OpenAI-compatible embeddings request.
type embeddingsRequestBody struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
	// EncodingFormat asks for "base64" responses (raw little-endian float32
	// bytes), roughly 4x smaller than the default JSON float arrays — the
	// difference dominates round-trip time on slow links. Empty omits the
	// field for servers that reject it.
	EncodingFormat string `json:"encoding_format,omitempty"`
	// Dimensions asks the endpoint to reduce every embedding to exactly this
	// length (Matryoshka truncation plus renormalization, server-side). Zero
	// omits the field so native-dimension configurations and endpoints
	// without dimension selection keep working.
	Dimensions int `json:"dimensions,omitempty"`
}

// embeddingsResponseBody is the OpenAI-compatible embeddings response.
type embeddingsResponseBody struct {
	Data []struct {
		Index     int             `json:"index"`
		Embedding embeddingVector `json:"embedding"`
	} `json:"data"`
}

// embeddingVector decodes an OpenAI-compatible embedding that arrives either
// as a JSON float array (the default) or as a base64 string of little-endian
// float32 bytes (encoding_format "base64"). Accepting both means the encoder
// keeps working against servers that silently ignore encoding_format.
type embeddingVector []float32

func (v *embeddingVector) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return fmt.Errorf("decode base64 embedding: %w", err)
		}
		if len(raw)%4 != 0 {
			return fmt.Errorf("base64 embedding is %d bytes, not a multiple of 4", len(raw))
		}
		out := make([]float32, len(raw)/4)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
		}
		*v = out
		return nil
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(b, &elements); err != nil {
		return err
	}
	floats := make([]float32, len(elements))
	for i, element := range elements {
		if bytes.Equal(bytes.TrimSpace(element), []byte("null")) {
			return fmt.Errorf("embedding component %d is null", i)
		}
		if err := json.Unmarshal(element, &floats[i]); err != nil {
			return fmt.Errorf("decode embedding component %d: %w", i, err)
		}
	}
	*v = floats
	return nil
}

func validateEmbedding(vector []float32, index int) error {
	var squaredNorm float64
	for component, value := range vector {
		f := float64(value)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return &InvalidEmbeddingError{
				Index: index, Component: component, Reason: "non-finite component",
			}
		}
		squaredNorm += f * f
	}
	if squaredNorm == 0 {
		return &InvalidEmbeddingError{Index: index, Component: -1, Reason: "zero norm"}
	}
	return nil
}

func validateEmbeddings(vectors [][]float32) error {
	for index, vector := range vectors {
		if err := validateEmbedding(vector, index); err != nil {
			return err
		}
	}
	return nil
}

// encoderClient carries the HTTP client, resolved URL, and the runtime
// float-fallback state shared by every call the EncodeFunc makes.
type encoderClient struct {
	client *http.Client
	url    string
	cfg    EncoderConfig
	// floatMode flips to true (for the encoder's lifetime) when the server
	// rejects the encoding_format field, so every later request goes back
	// to plain JSON float arrays instead of failing the same way again.
	floatMode atomic.Bool
}

// NewEncoder returns a kitvec.EncodeFunc that POSTs to an OpenAI-compatible
// embeddings endpoint. Each invocation of the returned func makes exactly
// one HTTP call; batching and concurrency are the caller's responsibility
// via kitvec.EncodeBatched.
//
// Requests ask for base64-encoded embeddings (encoding_format "base64",
// ~4x smaller than JSON float arrays); responses in either format are
// accepted, and a server that rejects the field outright downgrades this
// encoder to plain float requests for its lifetime.
func NewEncoder(cfg EncoderConfig) kitvec.EncodeFunc {
	ec := &encoderClient{
		client: &http.Client{Timeout: cfg.Timeout},
		url:    strings.TrimRight(cfg.Endpoint, "/") + "/embeddings",
		cfg:    cfg,
	}
	return ec.encode
}

// marshalRequest builds the request body, applying the configured input
// affixes and, unless the encoder has downgraded to float mode, asking for
// base64 embeddings.
func (ec *encoderClient) marshalRequest(texts []string) ([]byte, error) {
	inputs := texts
	if ec.cfg.InputPrefix != "" || ec.cfg.InputSuffix != "" {
		inputs = make([]string, len(texts))
		for i, t := range texts {
			inputs[i] = ec.cfg.InputPrefix + t + ec.cfg.InputSuffix
		}
	}
	body := embeddingsRequestBody{Model: ec.cfg.Model, Input: inputs}
	if !ec.floatMode.Load() {
		body.EncodingFormat = "base64"
	}
	if ec.cfg.RequestDimensions {
		body.Dimensions = ec.cfg.Dimension
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("[vector.embeddings] marshal request: %w", err)
	}
	return reqBody, nil
}

// encode performs the retrying HTTP call and validates the response shape.
func (ec *encoderClient) encode(ctx context.Context, texts []string) ([][]float32, error) {
	usedBase64 := !ec.floatMode.Load()
	reqBody, err := ec.marshalRequest(texts)
	if err != nil {
		return nil, err
	}

	attempts := ec.cfg.MaxRetries
	if attempts <= 0 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		vectors, retryable, err := ec.attemptEncode(ctx, reqBody, texts)
		if err == nil {
			return vectors, nil
		}
		if usedBase64 && isEncodingFormatRejection(err) {
			// The server rejected the encoding_format field itself (not this
			// input). Downgrade to float mode permanently and redo the call;
			// without this, every request would fail identically and the
			// build would abort on a transport nicety.
			ec.floatMode.Store(true)
			return ec.encode(ctx, texts)
		}
		if ec.cfg.RequestDimensions && isDimensionsRejection(err) {
			// Unlike encoding_format there is no safe downgrade: dropping the
			// dimensions field would change the embedding space, so fail with
			// the fix spelled out instead of retrying an identical request.
			return nil, fmt.Errorf(
				"[vector.embeddings] endpoint rejected the dimensions field "+
					"(model %q or this endpoint may not support reduced output dimensions): "+
					"unset [vector.embeddings] request_dimensions or set dimension to the "+
					"model's native output length: %w",
				ec.cfg.Model, err)
		}
		lastErr = err
		if !retryable || attempt == attempts {
			return nil, lastErr
		}
		if err := sleepBackoff(ctx, attempt, err); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// isEncodingFormatRejection reports whether err is a client-error response
// that names the encoding_format field, i.e. a server refusing the base64
// request format rather than the input. The match is deliberately narrow:
// generic 4xx bodies must keep flowing through the normal retry/permanence
// classification.
func isEncodingFormatRejection(err error) bool {
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	if statusErr.Status < 400 || statusErr.Status >= 500 {
		return false
	}
	return strings.Contains(strings.ToLower(statusErr.Body), "encoding_format")
}

// isDimensionsRejection reports whether err is a client-error response whose
// body names the dimensions field, i.e. a server refusing the reduced-output
// request rather than the input. Callers must only consult it when the
// request actually carried the field; the match is body-text based and a
// request without the field cannot be rejected for it.
func isDimensionsRejection(err error) bool {
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	if statusErr.Status < 400 || statusErr.Status >= 500 {
		return false
	}
	return strings.Contains(strings.ToLower(statusErr.Body), "dimensions")
}

// attemptEncode makes a single HTTP request and decodes the response. The
// retryable return value indicates whether the error is worth retrying
// (429, 5xx, or a transport-level failure).
func (ec *encoderClient) attemptEncode(
	ctx context.Context, reqBody []byte, texts []string,
) ([][]float32, bool, error) {
	client, url, cfg := ec.client, ec.url, ec.cfg
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
		statusErr := &HTTPStatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
		if resp.StatusCode == http.StatusTooManyRequests {
			if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
				statusErr.RetryAfter = &d
			}
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retryable, statusErr
	}

	var decoded embeddingsResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		// A decode failure almost always means the connection died
		// mid-stream (truncated body), not that the endpoint sent a
		// deliberately malformed response; treat it as transient so the
		// caller retries rather than giving up immediately.
		return nil, true, fmt.Errorf("[vector.embeddings] decode response: %w", err)
	}

	vectors, err := reorderAndValidate(decoded, texts, cfg)
	if err != nil {
		var invalidErr *InvalidEmbeddingError
		return nil, errors.As(err, &invalidErr), err
	}
	return vectors, false, nil
}

// reorderAndValidate reorders the decoded embeddings by their reported
// index and validates counts and dimensions against the request. A
// wrong-length vector is always an error — reduction happens server-side or
// not at all, never by client-side truncation — and when the request carried
// the dimensions field the error says the endpoint ignored it.
func reorderAndValidate(
	decoded embeddingsResponseBody, texts []string, cfg EncoderConfig,
) ([][]float32, error) {
	dimension := cfg.Dimension
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
			if cfg.RequestDimensions {
				return nil, fmt.Errorf(
					"[vector.embeddings] dimension mismatch at index %d: got %d, want %d "+
						"(the endpoint ignored the requested dimensions field; model %q or "+
						"this endpoint may not support reduced output dimensions — unset "+
						"[vector.embeddings] request_dimensions or set dimension to the "+
						"model's native output length)",
					d.Index, len(d.Embedding), dimension, cfg.Model)
			}
			return nil, fmt.Errorf(
				"[vector.embeddings] dimension mismatch at index %d: got %d, want %d",
				d.Index, len(d.Embedding), dimension)
		}
		out[d.Index] = []float32(d.Embedding)
		seen[d.Index] = true
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("[vector.embeddings] missing embedding for index %d", i)
		}
	}
	if err := validateEmbeddings(out); err != nil {
		return nil, err
	}
	return out, nil
}

// sleepBackoff waits before the next attempt — honoring a 429 response's
// Retry-After delay when lastErr carries one, falling back to capped
// exponential backoff otherwise — returning ctx.Err() promptly if ctx is
// cancelled during the wait.
func sleepBackoff(ctx context.Context, attempt int, lastErr error) error {
	delay := backoffDelay(attempt, lastErr)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backoffDelay picks the wait before the next retry: a 429's parsed
// Retry-After delay when present, otherwise capped exponential backoff
// from attempt.
func backoffDelay(attempt int, lastErr error) time.Duration {
	var statusErr *HTTPStatusError
	if errors.As(lastErr, &statusErr) && statusErr.RetryAfter != nil {
		return *statusErr.RetryAfter
	}
	delay := backoffBase << (attempt - 1)
	if delay > backoffMax || delay <= 0 {
		delay = backoffMax
	}
	return delay
}

// parseRetryAfter parses an HTTP Retry-After header value in either the
// delta-seconds or HTTP-date form (RFC 9110 §10.2.3), relative to now,
// clamped to [0, retryAfterCap]. It reports ok=false for an empty or
// unparseable header. A delta-seconds value of 0 (or an HTTP-date already
// in the past) means "retry immediately", reported as a zero duration.
func parseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		return clampRetryAfter(time.Duration(seconds) * time.Second), true
	}
	if when, err := http.ParseTime(header); err == nil {
		return clampRetryAfter(when.Sub(now)), true
	}
	return 0, false
}

// clampRetryAfter bounds d to [0, retryAfterCap].
func clampRetryAfter(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > retryAfterCap {
		return retryAfterCap
	}
	return d
}
