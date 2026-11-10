// Package vector wires agentsview into kit's vector package for semantic
// search: SQLite-backed vector storage and OpenAI-compatible embeddings.
package vector

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	kitvec "go.kenn.io/kit/vector"
)

// EncoderConfig configures one role's embeddings encoder on kit's shared
// client. Model, Roles, Deployment, and Transport are the shared embedding
// contract; the rest is agentsview's retry policy.
type EncoderConfig struct {
	Model      embedconfig.Model
	Roles      embedconfig.Roles
	Deployment embedconfig.Deployment
	Transport  embedconfig.Transport
	// APIKey is sent as a Bearer token when non-empty.
	APIKey string
	// OllamaMetalRecovery enables kit's Ollama Metal recovery for an Ollama
	// endpoint ending in /v1.
	OllamaMetalRecovery bool
	// MaxRetries is the maximum total attempts on retryable errors; values
	// <= 1 mean one attempt.
	MaxRetries int
	// RetryRateLimits keeps retrying HTTP 429 responses after MaxRetries is
	// spent, until the request succeeds or ctx is canceled. Long-running
	// document builds enable this; latency-sensitive query encoders do not.
	RetryRateLimits bool
}

const (
	backoffBase = 250 * time.Millisecond
	backoffMax  = 5 * time.Second
	// retryAfterCap bounds how long a Retry-After header can push a single
	// wait, so a misbehaving endpoint cannot stall a build for arbitrarily
	// long.
	retryAfterCap = 60 * time.Second
)

// NewEncoder returns a kitvec.EncodeFunc that embeds texts for role through
// kit's embedclient, applying the role's configured prefix and suffix. Callers
// batch through kitvec, so the client never splits a call further.
func NewEncoder(cfg EncoderConfig, role embedconfig.Role) (kitvec.EncodeFunc, error) {
	retry := embedclient.Retry{
		MaxAttempts:    cfg.MaxRetries,
		InitialBackoff: backoffBase,
		MaxBackoff:     backoffMax,
		MaxRetryAfter:  retryAfterCap,
	}
	if cfg.RetryRateLimits {
		// kit's retry spends one attempt budget on every retryable status,
		// so retryEncode owns retries to keep 429s off the budget.
		retry.MaxAttempts = 1
	}
	client, err := embedclient.New(embedclient.Options{
		Model:      cfg.Model,
		Roles:      cfg.Roles,
		Deployment: cfg.Deployment,
		Batch:      embedconfig.Batch{Items: math.MaxInt32},
		Transport:  cfg.Transport,
		APIKey:     cfg.APIKey,
		// Ollama recovery re-encodes only the invalid inputs.
		OllamaMetalRecovery: cfg.OllamaMetalRecovery,
		Retry:               retry,
	})
	if err != nil {
		return nil, fmt.Errorf("[vector.embeddings] configure client: %w", err)
	}
	timeout := cfg.Transport.Timeout
	if !cfg.RetryRateLimits {
		return func(ctx context.Context, texts []string) ([][]float32, error) {
			vectors, err := client.EmbedTexts(ctx, role, texts)
			return vectors, describeTimeout(ctx, err, timeout)
		}, nil
	}
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		vectors, err := encodeHalvingOnTimeout(ctx, texts, func(batch []string) ([][]float32, error) {
			return retryEncode(ctx, cfg.MaxRetries, len(batch) > 1, func() ([][]float32, error) {
				return client.EmbedTexts(ctx, role, batch)
			})
		})
		return vectors, describeTimeout(ctx, err, timeout)
	}, nil
}

// encodeHalvingOnTimeout encodes texts and, when a request of several inputs
// times out, encodes each half separately instead. A slow server, such as one
// running the model on the CPU, may answer smaller requests in time where a
// full batch cannot; failing the batch would instead fail the whole build on
// every attempt. A single input that times out returns its error.
func encodeHalvingOnTimeout(
	ctx context.Context, texts []string, encode func([]string) ([][]float32, error),
) ([][]float32, error) {
	vectors, err := encode(texts)
	if err == nil || len(texts) < 2 || !isRequestTimeout(ctx, err) {
		return vectors, err
	}
	mid := len(texts) / 2
	log.Printf("vector build: embed request for %d inputs timed out; retrying as requests of %d and %d",
		len(texts), mid, len(texts)-mid)
	first, err := encodeHalvingOnTimeout(ctx, texts[:mid], encode)
	if err != nil {
		return nil, err
	}
	second, err := encodeHalvingOnTimeout(ctx, texts[mid:], encode)
	if err != nil {
		// Report an invalid vector at its position in texts, not in the half.
		if vectorErr, ok := errors.AsType[*embedclient.VectorError](err); ok {
			vectorErr.Index += mid
		}
		return nil, err
	}
	return append(first, second...), nil
}

// isRequestTimeout reports whether err is one request that ran past the
// transport timeout, as opposed to the caller's context ending.
func isRequestTimeout(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if _, ok := errors.AsType[*embedclient.TransportError](err); !ok {
		return false
	}
	netErr, ok := errors.AsType[net.Error](err)
	return ok && netErr.Timeout()
}

// describeTimeout names a request timeout in err's message. Kit reports every
// transport failure as "embed request failed" so its message cannot leak an
// internal host, which leaves a slow server indistinguishable from an
// unreachable one. The wrapped error still matches *embedclient.TransportError.
func describeTimeout(ctx context.Context, err error, timeout time.Duration) error {
	if err == nil || !isRequestTimeout(ctx, err) {
		return err
	}
	if timeout <= 0 {
		return fmt.Errorf("%w: request timed out", err)
	}
	return fmt.Errorf("%w: no response within the %s request timeout", err, timeout)
}

// retryEncode calls encode until it succeeds, with the same jittered, capped
// exponential backoff kit uses. A 429 clears with time and does not mean the
// request is broken, so a durable build waits it out without limit. Other
// retryable failures (408, 5xx, transport errors) share one budget of
// maxAttempts across the whole call, so rate limits interleaved with server
// failures cannot reset it. When splittable is set, a request timeout returns
// at once so the caller can retry with fewer inputs; the same request would
// most likely time out again. Any other error returns at once.
func retryEncode(
	ctx context.Context, maxAttempts int, splittable bool, encode func() ([][]float32, error),
) ([][]float32, error) {
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = backoffBase
	policy.MaxInterval = backoffMax
	failures := 0
	vectors, err := backoff.Retry(ctx, func() ([][]float32, error) {
		vectors, err := encode()
		if err == nil {
			return vectors, nil
		}
		apiErr, isAPIErr := errors.AsType[*embedclient.APIError](err)
		_, isTransportErr := errors.AsType[*embedclient.TransportError](err)
		switch {
		case splittable && isRequestTimeout(ctx, err):
			return nil, backoff.Permanent(err)
		case isAPIErr && apiErr.StatusCode == http.StatusTooManyRequests:
		case (isAPIErr && apiErr.Retryable()) || isTransportErr:
			if failures++; failures >= max(maxAttempts, 1) {
				return nil, backoff.Permanent(err)
			}
		default:
			return nil, backoff.Permanent(err)
		}
		if isAPIErr && apiErr.RetryAfter > 0 {
			return nil, backoff.RetryAfter(min(apiErr.RetryAfter, retryAfterCap), err)
		}
		return nil, err
	}, backoff.WithBackOff(policy), backoff.WithMaxElapsedTime(0))
	if retryErr := backoff.AsRetryError(err); retryErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(retryErr.LastErr, ctxErr)
		}
		return nil, retryErr.LastErr
	}
	return vectors, err
}
