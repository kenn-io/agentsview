// Package vector wires agentsview into kit's vector package for semantic
// search: SQLite-backed vector storage and OpenAI-compatible embeddings.
package vector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"

	kitvec "go.kenn.io/kit/vector"
)

// EncoderConfig configures an OpenAI-compatible embeddings HTTP client.
type EncoderConfig struct {
	// Endpoint is the base URL including "/v1"; "/embeddings" is appended.
	Endpoint string
	// APIKey is sent as a Bearer token when non-empty. Empty means
	// anonymous, unauthenticated requests.
	APIKey string
	// OllamaCPUFallback reloads an invalid Ollama Metal runner before one native
	// CPU retry. Callers must opt in only for an Ollama endpoint ending in /v1.
	OllamaCPUFallback bool
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
	// MaxRetries is the maximum total attempts on retryable errors; values
	// <= 0 mean one attempt. A 429 does not consume this budget when
	// RetryRateLimits is enabled.
	MaxRetries int
	// RetryRateLimits keeps retrying HTTP 429 responses until the request
	// succeeds or ctx is canceled. Long-running document builds enable this;
	// latency-sensitive query encoders leave it disabled and use MaxRetries.
	RetryRateLimits bool
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
//
// Blank inputs are deliberately absent: kitvec.Split drops blank windows and
// kitvec.EncodeBatched refuses blank chunks outright — counting whitespace,
// invisible formatting runes, and control characters as blank — so a blank
// document is stamped without vectors and never becomes a request. That
// removes the need to recognize each provider's phrasing for the rejection
// (see isPermanentEncodeError for the structured signal kit raises instead).
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
	Dimensions int `json:"dimensions,omitzero"`
}

// embeddingsResponseBody is the OpenAI-compatible embeddings response.
type embeddingsResponseBody struct {
	Data []struct {
		Index     int             `json:"index"`
		Embedding embeddingVector `json:"embedding"`
	} `json:"data"`
}

type ollamaEmbedRequest struct {
	Model      string              `json:"model"`
	Input      []string            `json:"input"`
	Truncate   bool                `json:"truncate"`
	Dimensions int                 `json:"dimensions,omitzero"`
	Options    *ollamaEmbedOptions `json:"options,omitempty"`
	KeepAlive  string              `json:"keep_alive,omitempty"`
}

type ollamaEmbedOptions struct {
	NumGPU int `json:"num_gpu"`
}

type ollamaEmbedResponse struct {
	Embeddings []embeddingVector `json:"embeddings"`
}

type ollamaProcessResponse struct {
	Models []struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	} `json:"models"`
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
	var elements []jsontext.Value
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
	urlErr error
	cfg    EncoderConfig
	gate   *ollamaEndpointGate
	// floatMode flips to true (for the encoder's lifetime) when the server
	// rejects the encoding_format field, so every later request goes back
	// to plain JSON float arrays instead of failing the same way again.
	floatMode atomic.Bool
}

var ollamaEndpointGates sync.Map

// ollamaEndpointGate is a context-aware, writer-preferring read/write gate.
// Primary requests share read access. A CPU fallback takes write access so it
// waits for active primaries and prevents new ones from entering, while a
// canceled caller can leave the queue immediately.
type ollamaEndpointGate struct {
	weighted *semaphore.Weighted
}

const ollamaEndpointGateCapacity int64 = 1<<63 - 1

func newOllamaEndpointGate() *ollamaEndpointGate {
	return &ollamaEndpointGate{weighted: semaphore.NewWeighted(ollamaEndpointGateCapacity)}
}

func (g *ollamaEndpointGate) acquireRead(ctx context.Context) error {
	return g.weighted.Acquire(ctx, 1)
}

func (g *ollamaEndpointGate) releaseRead() {
	g.weighted.Release(1)
}

func (g *ollamaEndpointGate) acquireWrite(ctx context.Context) error {
	return g.weighted.Acquire(ctx, ollamaEndpointGateCapacity)
}

func (g *ollamaEndpointGate) releaseWrite() {
	g.weighted.Release(ollamaEndpointGateCapacity)
}

// NewEncoder returns a kitvec.EncodeFunc that POSTs to an OpenAI-compatible
// embeddings endpoint. Each invocation makes primary calls according to the
// retry policy and, when explicitly enabled, recovers invalid Ollama responses
// by unloading and retrying the Metal runner before falling back to CPU.
// Batching and concurrency are the caller's responsibility via kitvec.EncodeBatched.
//
// Requests ask for base64-encoded embeddings (encoding_format "base64",
// ~4x smaller than JSON float arrays); responses in either format are
// accepted, and a server that rejects the field outright downgrades this
// encoder to plain float requests for its lifetime.
func NewEncoder(cfg EncoderConfig) kitvec.EncodeFunc {
	primaryURL, err := openAIEmbeddingsURL(cfg.Endpoint)
	ec := &encoderClient{
		client: &http.Client{Timeout: cfg.Timeout},
		url:    primaryURL,
		urlErr: err,
		cfg:    cfg,
	}
	if cfg.OllamaCPUFallback {
		if nativeURL, nativeErr := ollamaEmbedURL(cfg.Endpoint); nativeErr == nil {
			gate, _ := ollamaEndpointGates.LoadOrStore(nativeURL, newOllamaEndpointGate())
			ec.gate = gate.(*ollamaEndpointGate)
		}
	}
	return ec.encode
}

func openAIEmbeddingsURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse embeddings endpoint: %w", err)
	}
	basePath := strings.TrimRight(u.Path, "/")
	if u.RawPath != "" {
		rawBase, err := rawPathPrefix(u.RawPath, basePath, u.Path[len(basePath):])
		if err != nil {
			return "", fmt.Errorf("parse embeddings endpoint path: %w", err)
		}
		u.RawPath = rawBase + "/embeddings"
	}
	u.Path = basePath + "/embeddings"
	return u.String(), nil
}

func ollamaEmbedURL(endpoint string) (string, error) {
	return ollamaAPIURL(endpoint, "/api/embed")
}

func ollamaPSURL(endpoint string) (string, error) {
	return ollamaAPIURL(endpoint, "/api/ps")
}

func ollamaAPIURL(endpoint, apiPath string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse Ollama endpoint: %w", err)
	}
	endpointPath := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(endpointPath, "/v1") {
		return "", fmt.Errorf("ollama endpoint path %q does not end in /v1", u.Path)
	}
	basePath := strings.TrimSuffix(endpointPath, "/v1")
	if u.RawPath != "" {
		rawBase, err := rawPathPrefix(u.RawPath, basePath, u.Path[len(basePath):])
		if err != nil {
			return "", fmt.Errorf("parse Ollama endpoint path: %w", err)
		}
		u.RawPath = rawBase + apiPath
	}
	u.Path = basePath + apiPath
	return u.String(), nil
}

// rawPathPrefix finds the byte prefix of an escaped URL path that decodes to
// decodedPrefix while the remainder decodes to decodedSuffix. Keeping the
// caller's original escaped prefix preserves meaningful encodings such as
// %2F instead of silently rewriting the endpoint route.
func rawPathPrefix(rawPath, decodedPrefix, decodedSuffix string) (string, error) {
	for split := 0; split <= len(rawPath); split++ {
		prefix, prefixErr := url.PathUnescape(rawPath[:split])
		if prefixErr != nil || prefix != decodedPrefix {
			continue
		}
		suffix, suffixErr := url.PathUnescape(rawPath[split:])
		if suffixErr == nil && suffix == decodedSuffix {
			return rawPath[:split], nil
		}
	}
	return "", fmt.Errorf(
		"escaped path %q does not match decoded path %q%s",
		rawPath, decodedPrefix, decodedSuffix,
	)
}

func (ec *encoderClient) requestInputs(texts []string) []string {
	inputs := texts
	if ec.cfg.InputPrefix != "" || ec.cfg.InputSuffix != "" {
		inputs = make([]string, len(texts))
		for i, t := range texts {
			inputs[i] = ec.cfg.InputPrefix + t + ec.cfg.InputSuffix
		}
	}
	return inputs
}

// marshalRequest builds the request body, applying the configured input
// affixes and, unless the encoder has downgraded to float mode, asking for
// base64 embeddings.
func (ec *encoderClient) marshalRequest(texts []string) ([]byte, error) {
	body := embeddingsRequestBody{Model: ec.cfg.Model, Input: ec.requestInputs(texts)}
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
	if ec.urlErr != nil {
		return nil, fmt.Errorf("[vector.embeddings] endpoint: %w", ec.urlErr)
	}
	usedBase64 := !ec.floatMode.Load()
	reqBody, err := ec.marshalRequest(texts)
	if err != nil {
		return nil, err
	}

	maxAttempts := ec.cfg.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	var lastErr error
	nonRateLimitAttempts := 0
	rateLimitAttempts := 0
	for {
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
		if ec.cfg.RetryRateLimits && isRateLimitError(err) {
			// A 429 is the one error class the provider tells us exactly how
			// to handle: it clears with time and doesn't indicate the request
			// itself is broken. Retry it forever (until ctx is canceled)
			// rather than spending the limited MaxRetries budget on it, so a
			// large build doesn't abort permanently on a transient quota.
			rateLimitAttempts++
			if err := sleepBackoff(ctx, rateLimitAttempts, err); err != nil {
				return nil, err
			}
			continue
		}
		nonRateLimitAttempts++
		if nonRateLimitAttempts == maxAttempts && ec.cfg.OllamaCPUFallback {
			if _, ok := errors.AsType[*InvalidEmbeddingError](err); ok {
				merged, fallbackErr := ec.ollamaFallback(ctx, texts, vectors)
				if fallbackErr == nil {
					return merged, nil
				}
				return nil, errors.Join(
					lastErr,
					fmt.Errorf("[vector.embeddings] Ollama CPU fallback: %w", fallbackErr),
				)
			}
		}
		if !retryable || nonRateLimitAttempts == maxAttempts {
			return nil, lastErr
		}
		if err := sleepBackoff(ctx, nonRateLimitAttempts, err); err != nil {
			return nil, err
		}
	}
}

// isRateLimitError reports whether err is an HTTP 429 (Too Many Requests)
// response from the embeddings endpoint.
func isRateLimitError(err error) bool {
	if statusErr, ok := errors.AsType[*HTTPStatusError](err); ok {
		return statusErr.Status == http.StatusTooManyRequests
	}
	return false
}

func (ec *encoderClient) ollamaFallback(
	ctx context.Context, texts []string, primaryVectors [][]float32,
) ([][]float32, error) {
	if ec.gate != nil {
		if err := ec.gate.acquireWrite(ctx); err != nil {
			return nil, err
		}
		defer ec.gate.releaseWrite()
	}

	requestInputs := ec.requestInputs(texts)
	invalidIndices := make([]int, 0, len(primaryVectors))
	invalidInputs := make([]string, 0, len(primaryVectors))
	for index, vector := range primaryVectors {
		if validateEmbedding(vector, index) == nil {
			continue
		}
		invalidIndices = append(invalidIndices, index)
		invalidInputs = append(invalidInputs, requestInputs[index])
	}

	fallbackURL, err := ollamaEmbedURL(ec.cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	psURL, err := ollamaPSURL(ec.cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	recovered, unloadErr := ec.ollamaNativeEmbed(
		ctx, fallbackURL, invalidInputs, invalidIndices, nil, "0s",
	)
	unloadWaitErr := ec.waitForOllamaUnload(ctx, psURL)
	if unloadErr == nil && unloadWaitErr == nil {
		return mergeOllamaVectors(primaryVectors, invalidIndices, recovered), nil
	}

	var reloadErr error
	if unloadWaitErr == nil {
		recovered, reloadErr = ec.ollamaNativeEmbed(
			ctx, fallbackURL, invalidInputs, invalidIndices, nil, "",
		)
		if reloadErr == nil {
			return mergeOllamaVectors(primaryVectors, invalidIndices, recovered), nil
		}
	}

	recovered, cpuErr := ec.ollamaNativeEmbed(
		ctx, fallbackURL, invalidInputs, invalidIndices,
		&ollamaEmbedOptions{NumGPU: 0}, "0s",
	)
	if cpuErr != nil {
		var recoveryErr error
		if unloadErr != nil {
			recoveryErr = errors.Join(
				recoveryErr, fmt.Errorf("ollama Metal unload request: %w", unloadErr))
		}
		if unloadWaitErr != nil {
			recoveryErr = errors.Join(
				recoveryErr, fmt.Errorf("ollama Metal unload wait: %w", unloadWaitErr))
		}
		if reloadErr != nil {
			recoveryErr = errors.Join(
				recoveryErr, fmt.Errorf("ollama Metal retry: %w", reloadErr))
		}
		return nil, errors.Join(recoveryErr, fmt.Errorf("ollama CPU fallback: %w", cpuErr))
	}
	return mergeOllamaVectors(primaryVectors, invalidIndices, recovered), nil
}

const ollamaUnloadTimeout = 10 * time.Second

func (ec *encoderClient) waitForOllamaUnload(ctx context.Context, psURL string) error {
	ctx, cancel := context.WithTimeout(ctx, ollamaUnloadTimeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		loaded, err := ec.ollamaModelLoaded(ctx, psURL)
		if err != nil {
			return err
		}
		if !loaded {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (ec *encoderClient) ollamaModelLoaded(ctx context.Context, psURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, psURL, nil)
	if err != nil {
		return false, fmt.Errorf("build Ollama process request: %w", err)
	}
	if ec.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+ec.cfg.APIKey)
	}
	resp, err := ec.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("ollama process request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, &HTTPStatusError{
			Status: resp.StatusCode,
			Body:   strings.TrimSpace(string(body)),
		}
	}

	var decoded ollamaProcessResponse
	if err := json.UnmarshalRead(resp.Body, &decoded); err != nil {
		return false, fmt.Errorf("decode Ollama process response: %w", err)
	}
	for _, model := range decoded.Models {
		if ollamaModelNamesMatch(ec.cfg.Model, model.Model) ||
			ollamaModelNamesMatch(ec.cfg.Model, model.Name) {
			return true, nil
		}
	}
	return false, nil
}

func ollamaModelNamesMatch(configured, loaded string) bool {
	return ollamaDisplayModelName(configured) == ollamaDisplayModelName(loaded)
}

func ollamaDisplayModelName(name string) string {
	if _, rest, ok := strings.Cut(name, "://"); ok {
		name = rest
	}
	parts := strings.Split(name, "/")
	if len(parts) >= 3 && strings.EqualFold(parts[0], "registry.ollama.ai") {
		parts = parts[1:]
		if len(parts) == 2 && strings.EqualFold(parts[0], "library") {
			parts = parts[1:]
		}
	}
	last := len(parts) - 1
	if !strings.Contains(parts[last], ":") {
		parts[last] += ":latest"
	}
	return strings.Join(parts, "/")
}

func (ec *encoderClient) ollamaNativeEmbed(
	ctx context.Context,
	fallbackURL string,
	inputs []string,
	originalIndices []int,
	options *ollamaEmbedOptions,
	keepAlive string,
) ([][]float32, error) {
	body := ollamaEmbedRequest{
		Model:     ec.cfg.Model,
		Input:     inputs,
		Truncate:  false,
		Options:   options,
		KeepAlive: keepAlive,
	}
	if ec.cfg.RequestDimensions {
		body.Dimensions = ec.cfg.Dimension
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal Ollama embed request: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, fallbackURL, bytes.NewReader(reqBody),
	)
	if err != nil {
		return nil, fmt.Errorf("build Ollama embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if ec.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+ec.cfg.APIKey)
	}
	resp, err := ec.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &HTTPStatusError{
			Status: resp.StatusCode,
			Body:   strings.TrimSpace(string(body)),
		}
	}

	var decoded ollamaEmbedResponse
	if err := json.UnmarshalRead(resp.Body, &decoded); err != nil {
		return nil, fmt.Errorf("decode Ollama embed response: %w", err)
	}
	if len(decoded.Embeddings) != len(originalIndices) {
		return nil, fmt.Errorf(
			"ollama embed response count mismatch: got %d embeddings, want %d",
			len(decoded.Embeddings), len(originalIndices))
	}

	recovered := make([][]float32, len(decoded.Embeddings))
	for fallbackIndex, originalIndex := range originalIndices {
		vector := []float32(decoded.Embeddings[fallbackIndex])
		if len(vector) != ec.cfg.Dimension {
			return nil, fmt.Errorf(
				"ollama embed response dimension mismatch at index %d: got %d, want %d",
				originalIndex, len(vector), ec.cfg.Dimension)
		}
		if err := validateEmbedding(vector, originalIndex); err != nil {
			return nil, err
		}
		recovered[fallbackIndex] = vector
	}
	return recovered, nil
}

func mergeOllamaVectors(
	primaryVectors [][]float32, invalidIndices []int, recovered [][]float32,
) [][]float32 {
	merged := make([][]float32, len(primaryVectors))
	copy(merged, primaryVectors)
	for recoveredIndex, originalIndex := range invalidIndices {
		merged[originalIndex] = recovered[recoveredIndex]
	}
	return merged
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
	if ec.gate != nil {
		if err := ec.gate.acquireRead(ctx); err != nil {
			return nil, false, err
		}
		defer ec.gate.releaseRead()
	}

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
	if err := json.UnmarshalRead(resp.Body, &decoded); err != nil {
		// A decode failure almost always means the connection died
		// mid-stream (truncated body), not that the endpoint sent a
		// deliberately malformed response; treat it as transient so the
		// caller retries rather than giving up immediately.
		return nil, true, fmt.Errorf("[vector.embeddings] decode response: %w", err)
	}

	vectors, err := reorderEmbeddings(decoded, texts, cfg)
	if err != nil {
		return nil, false, err
	}
	if err := validateEmbeddings(vectors); err != nil {
		var invalidErr *InvalidEmbeddingError
		return vectors, errors.As(err, &invalidErr), err
	}
	return vectors, false, nil
}

// reorderEmbeddings reorders the decoded embeddings by their reported index
// and validates counts and dimensions against the request. A
// wrong-length vector is always an error — reduction happens server-side or
// not at all, never by client-side truncation — and when the request carried
// the dimensions field the error says the endpoint ignored it.
func reorderEmbeddings(
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
