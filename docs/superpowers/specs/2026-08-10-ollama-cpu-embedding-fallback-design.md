# Automatic Ollama CPU Embedding Fallback

## Context

Ollama's bundled llama.cpp Metal runner can return non-finite embedding vectors
after repeated requests. Ollama's OpenAI-compatible endpoint exposes those
failures as zero-norm vectors. AgentsView already rejects non-finite and
zero-norm vectors, so the affected document remains pending and later embedding
work cannot progress past it.

Disabling llama.cpp prompt caching, context checkpoints, and slot prompt reuse
does not prevent the failure. Changing the KV cache from `q8_0` to `f16` also
does not prevent it, and disabling Flash Attention makes the repeated-request
failure broader. CPU inference is too slow to use as the normal backend, but a
one-off CPU retry for only the invalid inputs is acceptable.

## Goals

- Keep Metal as the normal embedding backend.
- Retry only invalid vectors on CPU, automatically and at most once.
- Make the Ollama-specific behavior an explicit per-server opt-in.
- Preserve valid Metal vectors from the same response.
- Leave generic OpenAI-compatible servers unchanged.
- Avoid changing the embedding-space fingerprint or rebuilding the index.

## Non-goals

- Do not run the full embedding build on CPU.
- Do not add Ollama-specific behavior to `go.kenn.io/kit`.
- Do not hide authentication, routing, model, response-count, or dimension
  errors behind a CPU retry.
- Do not operate a second permanent Ollama or llama.cpp service.
- Do not retry CPU recursively or add a second general retry policy.

## Configuration

Add an optional transport setting to each named embeddings server:

```toml
[vector.embeddings.servers.local]
endpoint = "http://127.0.0.1:11434/v1"
ollama_cpu_fallback = true
```

The setting defaults to `false`. When enabled, the endpoint path must end in a
`/v1` path component. The encoder derives the native Ollama endpoint by
replacing that final component with `/api/embed`, preserving any preceding proxy
path, escaped path components, and query values. The primary `/embeddings` URL
is derived through the same parsed-URL path handling. The existing API key, when
configured, is sent to both endpoints.

This flag is server transport behavior. It does not join the vector generation
parameters or fingerprint, because both backends use the same model, dimensions,
input affixes, and output space.

## Request Flow

The existing OpenAI-compatible request and retry behavior remains the primary
path:

1. Apply the configured document or query prefix and common suffix.
1. Send the batch to `<endpoint>/embeddings` through Metal.
1. Reorder and validate the response as today.
1. Retry retryable Metal failures up to `max_retries` as today.
1. Coordinate every opted-in encoder whose derived native Ollama URL is exactly
   equal through one process-wide gate. Primary requests hold shared access; a
   CPU fallback waits for active primary requests and holds exclusive access
   until its response and requested unload complete, blocking new Metal
   traffic meanwhile. A caller canceled while waiting leaves the gate
   promptly.
1. If the final Metal response contains zero-norm or non-finite vectors and
   `ollama_cpu_fallback` is enabled, retain the valid vectors and collect
   every invalid response position.
1. Send only the corresponding inputs to the native Ollama endpoint once.
1. Validate the CPU response and splice its vectors into those positions.
1. Return the complete batch only after every vector is valid.

The native fallback request is:

```json
{
  "model": "<configured model>",
  "input": ["<only invalid inputs>"],
  "truncate": false,
  "dimensions": 2560,
  "options": { "num_gpu": 0 },
  "keep_alive": "0s"
}
```

`dimensions` is included only when `request_dimensions` is enabled. The fallback
input strings are the exact already-affixed strings sent through the primary
path.

The request asks Ollama to use a CPU-only runner through `num_gpu: 0` and to
unload it through `keep_alive: "0s"`. With the diagnosed Ollama version,
changing runner options was observed to swap out the Metal runner, load CPU on
demand, unload CPU after the response, and load fresh Metal on the next primary
request. The gate prevents matching, fallback-enabled encoders in this
AgentsView process from racing that observed lifecycle. It cannot coordinate
fallback-disabled server entries, differently spelled aliases or query strings,
or external Ollama clients. Operators should use the Ollama endpoint exclusively
during fallback or configure every AgentsView entry for that instance with the
same endpoint and opt-in. Tests cover AgentsView's request ordering, not
Ollama's internal scheduler.

## Error Handling

Fallback is eligible only when the primary response has the expected count and
dimension but contains a zero-norm or non-finite vector. Other failures keep
their existing behavior.

The CPU request gets one attempt bounded by the configured server timeout. Its
response must contain exactly one correctly sized, finite, non-zero vector per
fallback input, in request order. Native vectors use the same strict decoder as
the OpenAI-compatible response, so JSON `null` components are rejected instead
of becoming zero. If the CPU request fails, returns the wrong shape, or returns
another invalid vector, the encoder returns an error that retains both the
original Metal failure and the CPU failure. It returns no partial batch, so
invalid data cannot reach vector persistence.

The encoder may log that it is retrying a count of invalid vectors through the
configured Ollama CPU fallback, but it must not log input text.

## Code Boundaries

- `internal/config` owns the new per-server boolean and validation.
- `cmd/agentsview` copies the resolved setting into `vector.EncoderConfig`.
- `internal/vector/encoder.go` owns endpoint derivation, the native Ollama
  request and response types, per-endpoint coordination, selective fallback,
  validation, and merge.
- `go.kenn.io/kit` remains provider-agnostic and unchanged.

Because query and document encoders share `NewEncoder`, the fallback applies to
both builds and semantic-search query encoding when explicitly enabled.

## Testing

Focused HTTP tests will use controlled in-process handlers and assert observable
encoder behavior:

- An opted-in encoder retains valid Metal vectors, sends only invalid inputs to
  `/api/embed`, and returns the correctly merged batch.
- The native request carries the configured model, exact affixed inputs,
  `truncate: false`, `options.num_gpu: 0`, `keep_alive: "0s"`, and conditional
  dimensions.
- An encoder without the opt-in returns the original invalid-vector error and
  never calls the native endpoint.
- HTTP, shape, dimension, and invalid-vector failures from the CPU attempt leave
  the whole encode call failed.
- Native JSON `null` components are rejected.
- Non-vector primary failures never trigger the CPU endpoint.
- Concurrent opted-in encoders with the same derived native URL cannot send
  primary traffic while a CPU fallback is in flight, and canceled gate waiters
  return promptly.
- Config parsing and validation accept the explicit flag and reject an enabled
  fallback whose endpoint does not end in `/v1`.

The tests protect AgentsView's routing, request construction, selective merge,
and failure contract. They do not test Ollama's scheduler or process lifecycle.

## Documentation

The semantic-search configuration documentation will describe the opt-in, its
Ollama-only scope, and the runner reload cost. It will state explicitly that the
setting is transport-only and does not start a new embedding generation.
