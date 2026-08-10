# Automatic Ollama CPU Embedding Fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep Ollama Metal inference as the default while automatically
recomputing only invalid embedding outputs with a short-lived CPU runner.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-10-ollama-cpu-embedding-fallback-design.md`

**Architecture:** A per-server `ollama_cpu_fallback` opt-in flows from config
into the existing OpenAI-compatible encoder. After ordinary Metal retries are
exhausted, the encoder retains valid vectors, sends only invalid inputs to
Ollama's native `/api/embed` endpoint with `num_gpu: 0` and `keep_alive: "0s"`,
validates the response, and merges it into the batch. Opted-in encoders sharing
the same native endpoint use a process-wide read/write gate: primary traffic is
concurrent under shared access, while CPU fallback quiesces it under exclusive
access.

**Tech Stack:** Go, BurntSushi TOML, `net/http`, `httptest`, testify, Ollama's
native embedding HTTP API.

## Global Constraints

- The fallback is disabled by default and explicit per named embeddings server.
- Generic OpenAI-compatible endpoints retain their current behavior.
- Only zero-norm or non-finite primary vectors are eligible for CPU fallback.
- Metal keeps its existing `max_retries` behavior before the one CPU attempt.
- The CPU request contains only invalid inputs and uses `truncate: false`,
  `options.num_gpu: 0`, and `keep_alive: "0s"`.
- Model, dimensions, prefixes, suffix, and API credentials stay identical.
- The fallback is transport-only and must not join the generation fingerprint.
- Query-bearing endpoints must preserve their query on both primary and native
  requests.
- Do not change `go.kenn.io/kit`.
- Do not install a branch binary over the live AgentsView binary or restart the
  live daemon without separate explicit permission.

## Plan Readiness

Before Task 1, incorporate the single RoboRev plan review, format this file with
the repository's pinned Markdown environment, and commit it. The committed plan
is the execution record; track progress outside this file so later formatting is
idempotent and the final worktree can be clean.

______________________________________________________________________

### Task 1: Add the explicit server setting and wire it into the encoder

**Files:**

- Modify: `internal/config/config.go:186-207,369-390`
- Test: `internal/config/config_vector_test.go:280-510`
- Modify: `internal/vector/encoder.go:27-59`
- Modify: `cmd/agentsview/embeddings.go:292-318`
- Test: `cmd/agentsview/embeddings_test.go:115-190` (completed with Task 2,
  after fallback behavior is observable)

**Interfaces:**

- Produces: `config.VectorEmbeddingsServerConfig.OllamaCPUFallback bool` and
  `vector.EncoderConfig.OllamaCPUFallback bool`.

- Validation contract: when enabled, `Endpoint` must be an absolute HTTP(S) URL
  whose path's final non-empty component is `v1`.

- Later tasks consume `EncoderConfig.OllamaCPUFallback`; this task adds no
  fallback behavior yet.

- Task 2 adds an HTTP-boundary test through `newVectorEncoder` so omitting the
  CLI wiring cannot pass the completed test suite.

- [ ] **Step 1: Write failing config load and validation tests**

Add a focused load case to `TestVectorConfigTOMLLoad`:

```go
t.Run("Ollama CPU fallback loads explicitly", func(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{
		"vector": map[string]any{
			"enabled": true,
			"embeddings": map[string]any{
				"model": "qwen3-embedding:4b-8k", "dimension": 2560,
				"servers": map[string]any{
					"local": map[string]any{
						"endpoint": "http://localhost:11434/proxy/v1/",
						"ollama_cpu_fallback": true,
					},
				},
			},
		},
	})
	assert.True(t, cfg.Vector.Embeddings.Servers["local"].OllamaCPUFallback)
})
```

Add endpoint validation cases to the existing invalid-override table:

```go
{
	name: "Ollama CPU fallback without v1 endpoint",
	server: map[string]any{
		"endpoint": "http://localhost:11434/openai",
		"ollama_cpu_fallback": true,
	},
	wantErr: "ollama_cpu_fallback requires an endpoint ending in /v1",
},
{
	name: "Ollama CPU fallback with relative endpoint",
	server: map[string]any{
		"endpoint": "/v1",
		"ollama_cpu_fallback": true,
	},
	wantErr: "ollama_cpu_fallback requires an absolute HTTP(S) endpoint",
},
```

When merging a test row into the base server map, allow the row's `endpoint` to
overwrite the default endpoint.

- [ ] **Step 2: Run the config test and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/config -run TestVectorConfigTOMLLoad -count=1
```

Expected: FAIL because `OllamaCPUFallback` does not exist and the setting is not
validated.

- [ ] **Step 3: Implement the config field and validation**

Add the server field:

```go
// OllamaCPUFallback retries invalid Metal vectors once through Ollama's
// native CPU embedding path. It is transport-only and defaults to false.
OllamaCPUFallback bool `toml:"ollama_cpu_fallback" json:"ollama_cpu_fallback,omitempty"`
```

In `VectorEmbeddingsServerConfig.validate`, after the endpoint-empty check,
validate only opted-in servers:

```go
if c.OllamaCPUFallback {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf(
			"%s ollama_cpu_fallback requires an absolute HTTP(S) endpoint", section)
	}
	endpointPath := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(endpointPath, "/v1") {
		return fmt.Errorf(
			"%s ollama_cpu_fallback requires an endpoint ending in /v1", section)
	}
}
```

`net/url` and `strings` are already imported by `internal/config/config.go`.

- [ ] **Step 4: Add the encoder interface field and CLI wiring**

Add this transport option to `vector.EncoderConfig`:

```go
// OllamaCPUFallback enables one native Ollama CPU retry for invalid vectors.
// Callers must opt in only for an Ollama endpoint ending in /v1.
OllamaCPUFallback bool
```

Pass it in `newVectorEncoder`:

```go
OllamaCPUFallback: server.OllamaCPUFallback,
```

Do not add it to `vectorGeneration`; the server choice and transport settings
remain outside the embedding generation fingerprint.

- [ ] **Step 5: Run focused tests and static checks**

Run:

```bash
go fmt ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/config ./cmd/agentsview -run 'TestVectorConfigTOMLLoad|TestVectorGeneration' -count=1
go vet ./...
go vet -tags fts5 ./...
```

Expected: all commands exit 0.

- [ ] **Step 6: Commit the configuration contract**

```bash
git add internal/config/config.go internal/config/config_vector_test.go \
  internal/vector/encoder.go cmd/agentsview/embeddings.go
git commit -m "feat(vector): configure Ollama CPU fallback" \
  -m "Make the provider-specific recovery path an explicit server transport option so generic OpenAI-compatible endpoints and vector generation identity remain unchanged."
```

### Task 2: Selectively retry invalid vectors through Ollama CPU

**Files:**

- Modify: `internal/vector/encoder.go:168-490`
- Test: `internal/vector/encoder_test.go:1-760`
- Test: `cmd/agentsview/embeddings_test.go`
- Modify:
  `docs/superpowers/specs/2026-08-10-ollama-cpu-embedding-fallback-design.md`
- Modify:
  `docs/superpowers/plans/2026-08-10-automatic-ollama-cpu-embedding-fallback.md`

**Interfaces:**

- Consumes: `EncoderConfig.OllamaCPUFallback bool` from Task 1.

- Produces:

    - `ollamaEmbedURL(endpoint string) (string, error)`
    - `openAIEmbeddingsURL(endpoint string) (string, error)`
    - `(*encoderClient).requestInputs(texts []string) []string`
    - `(*encoderClient).ollamaCPUFallback(ctx context.Context, texts []string, primaryVectors [][]float32) ([][]float32, error)`
    - Primary attempts that retain ordered vectors when norm validation fails.

- The public interface remains `NewEncoder(EncoderConfig) kitvec.EncodeFunc`.

- [ ] **Step 1: Write the failing selective-success test**

Add test-only request types mirroring the native boundary:

```go
type testOllamaEmbedRequest struct {
	Model      string         `json:"model"`
	Input      []string       `json:"input"`
	Truncate   *bool          `json:"truncate"`
	Dimensions *int           `json:"dimensions,omitempty"`
	Options    map[string]any `json:"options"`
	KeepAlive  string         `json:"keep_alive"`
}
```

Add `TestEncoderOllamaCPUFallbackReplacesOnlyInvalidVectors`. Its
`httptest.Server` must expose both `/proxy/v1/embeddings` and
`/proxy/api/embed`:

```go
func TestEncoderOllamaCPUFallbackReplacesOnlyInvalidVectors(t *testing.T) {
	var metalCalls, cpuCalls atomic.Int32
	var gotCPU testOllamaEmbedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/v1/embeddings":
			metalCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{1, 2, 3})},
				{"index": 1, "embedding": base64Embedding([]float32{0, 0, 0})},
				{"index": 2, "embedding": base64Embedding(
					[]float32{1, float32(math.NaN()), 0})},
			}})
		case "/proxy/api/embed":
			cpuCalls.Add(1)
			require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotCPU))
			writeJSON(t, w, http.StatusOK, map[string]any{
				"model": "test-model",
				"embeddings": [][]float32{{4, 5, 6}, {7, 8, 9}},
			})
		default:
			require.FailNow(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/proxy/v1", APIKey: "secret",
		Model: "test-model", Dimension: 3, RequestDimensions: true,
		Timeout: time.Second, MaxRetries: 2,
		InputPrefix: "pre:", InputSuffix: ":suf",
		OllamaCPUFallback: true,
	})
	out, err := enc(context.Background(), []string{"alpha", "beta", "gamma"})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}}, out)
	assert.Equal(t, int32(2), metalCalls.Load(), "normal retries run first")
	assert.Equal(t, int32(1), cpuCalls.Load(), "CPU is attempted exactly once")
	assert.Equal(t, "test-model", gotCPU.Model)
	assert.Equal(t, []string{"pre:beta:suf", "pre:gamma:suf"}, gotCPU.Input)
	require.NotNil(t, gotCPU.Truncate, "truncate must be present explicitly")
	assert.False(t, *gotCPU.Truncate)
	require.NotNil(t, gotCPU.Dimensions)
	assert.Equal(t, 3, *gotCPU.Dimensions)
	assert.EqualValues(t, 0, gotCPU.Options["num_gpu"])
	assert.Equal(t, "0s", gotCPU.KeepAlive)
}
```

This test catches recomputing valid vectors, losing input affixes, using the
wrong native path/options, skipping Metal retries, or retaining the CPU runner.

Also add `TestEncoderOllamaCPUFallbackOmitsDimensionsWhenNotRequested`, with
`RequestDimensions: false`, one invalid primary vector, and one valid CPU
vector. Decode the native request into `testOllamaEmbedRequest` and assert
`Dimensions` is nil while `Truncate` is non-nil and false. This protects both
sides of the native wire contract.

- [ ] **Step 2: Run the selective-success test and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/vector \
  -run TestEncoderOllamaCPUFallbackReplacesOnlyInvalidVectors -count=1
```

Expected: FAIL because the encoder returns the Metal invalid-vector error and
never calls `/proxy/api/embed`.

- [ ] **Step 3: Refactor primary response handling without changing default
  behavior**

Split shape ordering from norm validation:

```go
func reorderEmbeddings(
	decoded embeddingsResponseBody, texts []string, cfg EncoderConfig,
) ([][]float32, error)
```

This function keeps the existing count, index, duplicate, missing, and dimension
checks but does not call `validateEmbeddings`. Change `attemptEncode` so it
returns ordered vectors alongside a retryable `*InvalidEmbeddingError`:

```go
vectors, err := reorderEmbeddings(decoded, texts, cfg)
if err != nil {
	return nil, false, err
}
if err := validateEmbeddings(vectors); err != nil {
	var invalidErr *InvalidEmbeddingError
	return vectors, errors.As(err, &invalidErr), err
}
return vectors, false, nil
```

In `encode`, retain the last vectors but continue returning `nil, err` after
exhaustion when fallback is disabled. Existing invalid-output tests must remain
green.

- [ ] **Step 4: Implement endpoint derivation and the one-shot selective CPU
  request**

Add native request/response types:

```go
type ollamaEmbedRequest struct {
	Model      string              `json:"model"`
	Input      []string            `json:"input"`
	Truncate   bool                `json:"truncate"`
	Dimensions int                 `json:"dimensions,omitempty"`
	Options    ollamaEmbedOptions  `json:"options"`
	KeepAlive  string              `json:"keep_alive"`
}

type ollamaEmbedOptions struct {
	NumGPU int `json:"num_gpu"`
}

type ollamaEmbedResponse struct {
	Embeddings []embeddingVector `json:"embeddings"`
}
```

Derive both request URLs by parsing `cfg.Endpoint` and clearing `RawPath`.
Append `/embeddings` to the primary path without moving it into the query
string. For the native URL, trim one trailing slash and replace the final `/v1`
with `/api/embed`. Preserve scheme, host, proxy prefix, and query values for
both.

Add `TestEmbeddingURLsPreserveEndpointComponents` as a table test for a plain
`/v1` endpoint and a `/proxy/v1/?tenant=local` endpoint. Assert the exact native
URL, including the proxy prefix and query string, so dropping queries or
mishandling a trailing slash cannot pass.

Add an HTTP-boundary test that drives fallback through the query-bearing proxy
endpoint and asserts both `/proxy/v1/embeddings?tenant=local` and
`/proxy/api/embed?tenant=local` are reached.

Factor input affixing into:

```go
func (ec *encoderClient) requestInputs(texts []string) []string
```

Use it from both `marshalRequest` and `ollamaCPUFallback` so CPU sees the exact
primary strings.

`ollamaCPUFallback` must:

1. Scan `primaryVectors` with `validateEmbedding` and collect every invalid
   original index.
1. Build the native request with only those affixed inputs.
1. Include `Dimensions` only through the existing `omitempty` field when
   `RequestDimensions` is true.
1. Send the same bearer credential as the primary request.
1. Make exactly one HTTP call using the existing timeout-bound client.
1. Copy the outer primary slice and replace only invalid positions.

All opted-in encoders whose derived native URL is identical share a process-wide
`sync.RWMutex`. Each primary request holds `RLock` for its HTTP attempt. The CPU
fallback takes `Lock` after the invalid primary attempt returns, waiting for
active primary requests and preventing new ones until the native response and
requested unload complete. Add a concurrent HTTP test using two independently
constructed encoders to prove no primary request reaches the server during the
exclusive fallback.

At this step, implement the successful native-request path and only the minimum
response-count guard needed to merge without indexing outside the response.
Leave complete HTTP, decode, dimension, norm, and combined-error handling for
Steps 6-8, after their failing tests exist.

At the final retry boundary in `encode`:

```go
if attempt == attempts && ec.cfg.OllamaCPUFallback {
	var invalidErr *InvalidEmbeddingError
	if errors.As(err, &invalidErr) {
		merged, fallbackErr := ec.ollamaCPUFallback(ctx, texts, vectors)
		if fallbackErr == nil {
			return merged, nil
		}
		return nil, errors.Join(
			lastErr,
			fmt.Errorf("[vector.embeddings] Ollama CPU fallback: %w", fallbackErr),
		)
	}
}
```

- [ ] **Step 5: Run the selective-success test and verify GREEN**

Run the command from Step 2 again.

Expected: PASS with two primary calls, one CPU call, and the literal merged
vectors.

- [ ] **Step 6: Write failing eligibility and failure-contract tests**

Add these focused tests, each with its own `httptest.Server` handler:

- `TestEncoderOllamaCPUFallbackDisabledLeavesInvalidResponseFailed`: primary
  returns a zero vector, the flag is false, `/api/embed` calls
  `require.FailNow`, output is nil, and `errors.As` finds
  `*InvalidEmbeddingError`.
- `TestEncoderOllamaCPUFallbackDoesNotMaskDimensionMismatch`: primary returns a
  wrong-length vector with the flag true, `/api/embed` calls
  `require.FailNow`, and the error contains `dimension mismatch`.
- `TestEncoderOllamaCPUFallbackRejectsIneligiblePrimaryFailures`: table subtests
  for HTTP 401, HTTP 404, a malformed JSON response, and a response count
  mismatch. Enable fallback in every case, make `/api/embed` call
  `require.FailNow`, and assert the original error remains visible. These
  cases cover the authentication, routing/model, decode, and response-shape
  boundaries that must never switch runners.
- `TestEncoderOllamaCPUFallbackFailureLeavesBatchFailed`: table subtests for
  native status 500, wrong response count, wrong vector dimension, and a
  zero-norm CPU vector. Add a raw native response containing a JSON `null`
  component and assert it is rejected by the strict decoder. Each asserts nil
  output, one CPU call, an error containing both `invalid embedding` and
  `Ollama CPU fallback`, and no second CPU attempt.
- `TestNewVectorEncoderWiresOllamaCPUFallback` in
  `cmd/agentsview/embeddings_test.go`: build a named server config with the
  flag enabled, call `newVectorEncoder`, return an invalid OpenAI-compatible
  vector, and serve a valid native CPU vector. Assert the encode call succeeds
  and the native endpoint is called once. This observes the CLI-to-vector
  wiring at the HTTP boundary rather than merely comparing struct fields.

Use `MaxRetries: 1` in failure-contract tests so call counts are literal and the
tests remain fast.

- [ ] **Step 7: Run the new tests and verify RED where behavior is missing**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/vector \
  -run 'TestEncoderOllamaCPUFallback' -count=1
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
  -run TestNewVectorEncoderWiresOllamaCPUFallback -count=1
```

Expected before completing error handling: the combined-error and native
response-validation cases FAIL for the intended missing behavior. The CLI wiring
test may already pass because Task 1 supplied that field; its purpose is to
prevent a future omission, not to force an artificial second implementation.

- [ ] **Step 8: Complete native error and response validation**

For non-200 native responses, read at most 512 bytes and return an
`HTTPStatusError`. Require the CPU response count and every vector dimension to
match the invalid subset, and validate each CPU vector against its original
batch index. For JSON decode, count, dimension, or norm errors, prefix the error
with enough context to identify the CPU fallback without logging input text.
Join the original Metal error with the wrapped CPU error exactly once.

- [ ] **Step 9: Run encoder and package verification**

Run:

```bash
go fmt ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/vector -count=1
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
  -run TestNewVectorEncoderWiresOllamaCPUFallback -count=1
go vet ./...
go vet -tags fts5 ./...
```

Expected: all commands exit 0, including all pre-existing retry, base64,
dimension, cancellation, and invalid-vector tests.

- [ ] **Step 10: Commit the fallback behavior**

```bash
git add internal/vector/encoder.go internal/vector/encoder_test.go \
  cmd/agentsview/embeddings_test.go \
  docs/superpowers/specs/2026-08-10-ollama-cpu-embedding-fallback-design.md \
  docs/superpowers/plans/2026-08-10-automatic-ollama-cpu-embedding-fallback.md
git commit -m "fix(vector): recover invalid Ollama embeddings on CPU" \
  -m "Keep Metal as the fast path while isolating the expensive CPU runner to invalid vectors after normal retries are exhausted."
```

### Task 3: Document the operational behavior and verify the complete change

**Files:**

- Modify: `docs/semantic-search.md:44-50,75-123,239-300`

**Interfaces:**

- Consumes the config and behavior from Tasks 1-2.

- Produces user-facing configuration guidance; no code interface changes.

- [ ] **Step 1: Update the configuration reference**

Add a commented setting to the main example:

```toml
# ollama_cpu_fallback = true      # Ollama only: retry invalid Metal vectors once on CPU
```

Add `ollama_cpu_fallback` to the named-server transport list. State that it is
excluded from the generation fingerprint and does not trigger a rebuild.

- [ ] **Step 2: Document fallback behavior in Ollama quickstart**

Extend the quickstart example with:

```toml
ollama_cpu_fallback = true
```

Explain that invalid Metal responses first use normal retries, then only bad
inputs are sent to `/api/embed` with `num_gpu: 0`; the request asks Ollama for a
CPU-only runner and immediate unload. State that the diagnosed Ollama version
was observed to swap out Metal, unload CPU after the response, and reload fresh
Metal on the next request; AgentsView gates its own endpoint traffic around this
sequence but does not verify Ollama's scheduler lifecycle. Call out the one-time
latency cost and the requirement that the configured endpoint end in `/v1`.

- [ ] **Step 3: Correct the direct llama-server cache guidance**

Keep the cache-disabling command as an optional way to avoid prompt-state reuse,
but remove the claim that cache or slot-similarity settings prevent this
failure. State the observed boundary accurately: repeated Metal embedding
requests can still become non-finite with prompt cache, context checkpoints, and
similarity routing disabled, so the explicit CPU fallback is the recovery
mechanism for Ollama-managed runners.

- [ ] **Step 4: Format and run full verification**

Run:

```bash
uv run --project docs --frozen mdformat --wrap 80 docs/semantic-search.md \
  docs/superpowers/specs/2026-08-10-ollama-cpu-embedding-fallback-design.md \
  docs/superpowers/plans/2026-08-10-automatic-ollama-cpu-embedding-fallback.md
git diff --check
go fmt ./...
go vet ./...
go vet -tags fts5 ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/config ./internal/vector ./cmd/agentsview -count=1
```

If the focused packages pass, run the repository test target:

```bash
make test
```

Expected: formatting and static checks exit 0; all focused and repository tests
pass.

- [ ] **Step 5: Commit the documentation**

```bash
git add docs/semantic-search.md
git commit -m "docs(vector): explain Ollama CPU fallback" \
  -m "Document the runner swap cost and correct the earlier cache guidance now that repeated Metal failures have been isolated from prompt-state reuse."
```

- [ ] **Step 6: Verify repository state and report deployment boundary**

Run:

```bash
git status --short --branch
git log --oneline -4
```

Expected: a clean worktree with focused design, configuration, fallback, and
documentation commits. Report that activating the flag in the user's live
configuration requires a binary containing these commits; do not overwrite the
live binary or restart its daemon without explicit permission.
