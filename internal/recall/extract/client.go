package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// ErrContextOverflow reports a prompt the server rejected as too large for
// the model context. The unit must be split before retrying; retrying the
// same text can never succeed.
var ErrContextOverflow = errors.New("unit text overflows the model context")

// ErrPersistentTruncation reports output the client cannot recover: the
// token budget truncated a unit large enough to split (splitting preserves
// every entry, so no compact retry is attempted), or a unit at the compact
// floor stayed truncated even after the compact retry. Like an overflow,
// the recovery is splitting the unit into smaller pieces.
var ErrPersistentTruncation = errors.New(
	"model output truncated at the token budget",
)

// errTruncated is the per-request truncation signal that
// DistillWithRecovery converts into a split signal or a compact retry.
var errTruncated = errors.New("model output truncated")

// errPermanentRequest marks a server rejection that retrying the same
// request can never fix (wrong model name, malformed field).
var errPermanentRequest = errors.New("model server rejected the request")

// compactSuffix is appended on the truncation retry of a unit too small to
// split. Sampling at temperature zero makes a same-input retry
// deterministic, so the retry must change the input instead of hoping for
// different output; capping the entry count is acceptable only here, where
// splitting cannot recover the lost entries anyway.
const compactSuffix = "\n\nIMPORTANT: the output budget is tight for this " +
	"window. Return at most 4 entries, each with a body of at most 2 " +
	"sentences."

// extractionProtocolVersion feeds the generation fingerprint. Bump it when
// the response schema or the recovery behavior changes in a way that alters
// extraction output for an identical configuration.
const extractionProtocolVersion = 1

// Entry is one distilled memory entry as the model produces it.
type Entry struct {
	Type     string   `json:"type"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Entities []string `json:"entities"`
}

// Usage reports token consumption for one model call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// entrySchema constrains decoding so the model can only produce parseable
// entries; validation failures become server-side sampling constraints
// instead of client-side parse errors.
var entrySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"entries": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{
						"type": "string",
						"enum": []string{
							"fact", "decision", "procedure",
							"warning", "preference", "open_question",
						},
					},
					"title": map[string]any{"type": "string"},
					"body":  map[string]any{"type": "string"},
					"entities": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
				},
				"required":             []string{"type", "title", "body", "entities"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"entries"},
	"additionalProperties": false,
}

// Client distills unit text into entries through an OpenAI-compatible chat
// completion endpoint with constrained decoding.
type Client struct {
	BaseURL string
	Model   string
	// CompactFloorChars bounds the compact retry: a truncated unit longer
	// than this returns ErrPersistentTruncation immediately so the caller
	// splits it without losing entries; a unit at or below it (too small
	// for splitting to help) gets one compact retry first. Callers set it
	// to SplitFloorChars of their window budget; zero disables the compact
	// retry entirely.
	CompactFloorChars int
	HTTPClient        *http.Client
	Request           RequestShape
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// DistillWithRecovery runs one unit through the model with the recovery
// ladder: transient failures are retried up to maxAttempts per phase;
// truncated output on a unit at or below CompactFloorChars triggers exactly
// one compact retry (temperature-zero sampling makes same-input retries
// useless); truncation on a splittable unit, or truncation that survives
// the compact retry, surfaces as ErrPersistentTruncation and a context
// overflow as ErrContextOverflow — both mean "split this unit", which the
// caller owns because it also owns unit identity.
func (c *Client) DistillWithRecovery(
	ctx context.Context, systemPrompt, text string, maxAttempts int,
) ([]Entry, Usage, error) {
	if c.Request.MaxTokens <= 0 {
		return nil, Usage{}, fmt.Errorf(
			"extraction request max_tokens must be positive; the profile or "+
				"configuration must set it (got %d)", c.Request.MaxTokens,
		)
	}
	compactAllowed := len(text) <= c.CompactFloorChars
	for _, compact := range []bool{false, true} {
		var lastErr error
		truncated := false
		for range maxAttempts {
			entries, usage, err := c.distill(ctx, systemPrompt, text, compact)
			if err == nil {
				return entries, usage, nil
			}
			if errors.Is(err, ErrContextOverflow) ||
				errors.Is(err, errPermanentRequest) {
				return nil, Usage{}, err
			}
			if errors.Is(err, errTruncated) {
				truncated = true
				break
			}
			if ctx.Err() != nil {
				return nil, Usage{}, ctx.Err()
			}
			lastErr = err
		}
		if !truncated {
			return nil, Usage{}, fmt.Errorf(
				"distilling unit after %d attempts: %w", maxAttempts, lastErr,
			)
		}
		if !compactAllowed {
			break
		}
	}
	return nil, Usage{}, fmt.Errorf(
		"unit of %d chars: %w", len(text), ErrPersistentTruncation,
	)
}

func (c *Client) distill(
	ctx context.Context, systemPrompt, text string, compact bool,
) ([]Entry, Usage, error) {
	userText := text
	if compact {
		userText += compactSuffix
	}
	payload := map[string]any{
		"model":       c.Model,
		"max_tokens":  c.Request.MaxTokens,
		"temperature": c.Request.Temperature,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userText},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "session_readout",
				"strict": true,
				"schema": entrySchema,
			},
		},
	}
	maps.Copy(payload, c.Request.ExtraBody)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("encoding distill request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body),
	)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("building distill request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("posting distill request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("reading distill response: %w", err)
	}
	if response.StatusCode == http.StatusBadRequest {
		detail := string(raw)
		if len(detail) > 200 {
			detail = detail[:200]
		}
		// Chat servers answer 400 both for prompts that exceed the context
		// window (character budgets overshoot because token density varies
		// across content) and for genuinely bad requests; only the former
		// is recoverable by splitting the unit.
		if isContextOverflowDetail(string(raw)) {
			return nil, Usage{}, fmt.Errorf(
				"%d-char unit: %w: %s", len(userText), ErrContextOverflow,
				detail,
			)
		}
		return nil, Usage{}, fmt.Errorf(
			"%w (HTTP 400): %s", errPermanentRequest, detail,
		)
	}
	if response.StatusCode != http.StatusOK {
		return nil, Usage{}, fmt.Errorf(
			"distill request failed with HTTP %d", response.StatusCode,
		)
	}

	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, Usage{}, fmt.Errorf("parsing distill response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, Usage{}, fmt.Errorf("distill response has no choices")
	}
	choice := parsed.Choices[0]
	if choice.FinishReason == "length" {
		return nil, Usage{}, fmt.Errorf(
			"at max_tokens=%d: %w", c.Request.MaxTokens, errTruncated,
		)
	}
	if choice.Message.Content == "" {
		// Empty content with a normal finish reason means the token budget
		// went somewhere invisible (typically hidden reasoning the request
		// shape should have disabled).
		return nil, Usage{}, fmt.Errorf(
			"distill response content is empty; check the model profile's " +
				"request shape",
		)
	}
	var out struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(choice.Message.Content), &out); err != nil {
		return nil, Usage{}, fmt.Errorf("parsing distilled entries: %w", err)
	}
	return out.Entries, parsed.Usage, nil
}

// isContextOverflowDetail reports whether a 400 body identifies an
// input-length error. Chat servers phrase it differently ("maximum context
// length", "context_length_exceeded", "prompt is too long", "input length
// exceeds"), but all name the context or the input being too long.
func isContextOverflowDetail(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{"context", "too long", "input length"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// SplitFloorChars is the smallest unit size worth splitting further. A unit
// below this floor that still overflows or truncates is not a size problem,
// so the error should surface instead of recursing forever. The floor
// scales down with the window budget so small-window configurations can
// still split.
func SplitFloorChars(maxWindowChars int) int {
	floor := maxWindowChars / 8
	if floor > 2000 {
		return 2000
	}
	if floor < 1 {
		return 1
	}
	return floor
}
