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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
// request can never fix (wrong model name, bad credentials, malformed
// field).
var errPermanentRequest = errors.New("model server rejected the request")

// transientError marks a failure worth retrying: network errors, timeouts,
// rate limits, and server errors. RetryAfter carries the server's requested
// delay, zero when it gave none.
type transientError struct {
	err        error
	retryAfter time.Duration
}

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// reservedRequestKeys are payload fields the client owns. ExtraBody must
// not shadow them: a profile smuggling max_tokens or response_format
// through the extra body would bypass validation and desynchronize the
// generation fingerprint from the request actually sent.
var reservedRequestKeys = []string{
	"model", "messages", "max_tokens", "temperature", "response_format",
}

// maxRetryDelay caps the wait between transient retries, whether from
// backoff growth or an excessive Retry-After header.
const maxRetryDelay = 30 * time.Second

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
// v2: minLength constraints on entry title and body.
const extractionProtocolVersion = 2

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

// entryTypes is the closed set of entry kinds, shared by the request
// schema and the client-side validation of what actually came back.
var entryTypes = []string{
	"fact", "decision", "procedure", "warning", "preference", "open_question",
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
						"enum": entryTypes,
					},
					"title": map[string]any{"type": "string", "minLength": 1},
					"body":  map[string]any{"type": "string", "minLength": 1},
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
// completion endpoint with constrained decoding. Every output-affecting
// parameter lives in Request so it is covered by the generation
// fingerprint.
type Client struct {
	BaseURL string
	Model   string
	// RetryBackoff seeds the exponential wait between transient retries;
	// zero means 500ms. It shapes latency, not output, so it stays outside
	// RequestShape and the fingerprint.
	RetryBackoff time.Duration
	HTTPClient   *http.Client
	Request      RequestShape
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (c *Client) retryBackoff() time.Duration {
	if c.RetryBackoff > 0 {
		return c.RetryBackoff
	}
	return 500 * time.Millisecond
}

// DistillWithRecovery runs one unit through the model with the recovery
// ladder: transient failures (network errors, timeouts, rate limits,
// server errors) are retried up to maxAttempts per phase with exponential
// backoff honoring Retry-After, while permanent rejections fail fast;
// truncated output on a unit at or below the compact floor triggers exactly
// one compact retry (temperature-zero sampling makes same-input retries
// useless); truncation on a splittable unit, or truncation that survives
// the compact retry, surfaces as ErrPersistentTruncation and a context
// overflow as ErrContextOverflow — both mean "split this unit", which the
// caller owns because it also owns unit identity. The returned Usage sums
// every attempt, successful or not, so recovery costs are accounted.
func (c *Client) DistillWithRecovery(
	ctx context.Context, systemPrompt, text string, maxAttempts int,
) ([]Entry, Usage, error) {
	var total Usage
	if c.Request.MaxTokens <= 0 {
		return nil, total, fmt.Errorf(
			"extraction request max_tokens must be positive; the profile or "+
				"configuration must set it (got %d)", c.Request.MaxTokens,
		)
	}
	for _, key := range reservedRequestKeys {
		if _, ok := c.Request.ExtraBody[key]; ok {
			return nil, total, fmt.Errorf(
				"extra body must not set reserved request field %q; use the "+
					"dedicated configuration for it", key,
			)
		}
	}
	// Rune count, not byte length: segmentation budgets count code points,
	// so the floor must measure units the same way.
	unitChars := utf8.RuneCountInString(text)
	compactAllowed := unitChars <= c.Request.CompactFloorChars
	for _, compact := range []bool{false, true} {
		var lastErr error
		truncated := false
		for attempt := range maxAttempts {
			entries, usage, err := c.distill(ctx, systemPrompt, text, compact)
			total.PromptTokens += usage.PromptTokens
			total.CompletionTokens += usage.CompletionTokens
			if err == nil {
				return entries, total, nil
			}
			if errors.Is(err, errTruncated) {
				truncated = true
				break
			}
			var transient *transientError
			if !errors.As(err, &transient) {
				return nil, total, err
			}
			lastErr = err
			if attempt+1 >= maxAttempts {
				break
			}
			delay := transient.retryAfter
			if delay <= 0 {
				delay = c.retryBackoff() << attempt
			}
			if delay > maxRetryDelay {
				delay = maxRetryDelay
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, total, ctx.Err()
			case <-timer.C:
			}
		}
		if !truncated {
			return nil, total, fmt.Errorf(
				"distilling unit after %d attempts: %w", maxAttempts, lastErr,
			)
		}
		if !compactAllowed {
			break
		}
	}
	return nil, total, fmt.Errorf(
		"unit of %d chars: %w", unitChars, ErrPersistentTruncation,
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
	// Extras first, client-owned fields last: even if validation of
	// reserved keys were bypassed, the extra body could not shadow them.
	merged := make(map[string]any, len(c.Request.ExtraBody)+len(payload))
	maps.Copy(merged, c.Request.ExtraBody)
	maps.Copy(merged, payload)
	body, err := json.Marshal(merged)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("encoding distill request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("building distill request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, Usage{}, &transientError{
			err: fmt.Errorf("posting distill request: %w", err),
		}
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, Usage{}, &transientError{
			err: fmt.Errorf("reading distill response: %w", err),
		}
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
				"%d-char unit: %w: %s",
				utf8.RuneCountInString(userText), ErrContextOverflow, detail,
			)
		}
		return nil, Usage{}, fmt.Errorf(
			"%w (HTTP 400): %s", errPermanentRequest, detail,
		)
	}
	if response.StatusCode != http.StatusOK {
		statusErr := fmt.Errorf(
			"distill request failed with HTTP %d", response.StatusCode,
		)
		if isTransientStatus(response.StatusCode) {
			return nil, Usage{}, &transientError{
				err: statusErr,
				retryAfter: parseRetryAfter(
					response.Header.Get("Retry-After"),
				),
			}
		}
		detail := string(raw)
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return nil, Usage{}, fmt.Errorf(
			"%w (HTTP %d): %s", errPermanentRequest,
			response.StatusCode, detail,
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
	// A 200 with an unreadable or choiceless body is a glitch in transit or
	// in the serving layer, not a property of the input, so it retries.
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, Usage{}, &transientError{
			err: fmt.Errorf("parsing distill response: %w", err),
		}
	}
	if len(parsed.Choices) == 0 {
		return nil, Usage{}, &transientError{
			err: fmt.Errorf("distill response has no choices"),
		}
	}
	// From here the server reports token usage even when the attempt fails,
	// so error returns carry parsed.Usage for the caller's accounting.
	choice := parsed.Choices[0]
	if choice.FinishReason == "length" {
		return nil, parsed.Usage, fmt.Errorf(
			"at max_tokens=%d: %w", c.Request.MaxTokens, errTruncated,
		)
	}
	if choice.Message.Content == "" {
		// Empty content with a normal finish reason means the token budget
		// went somewhere invisible (typically hidden reasoning the request
		// shape should have disabled).
		return nil, parsed.Usage, fmt.Errorf(
			"distill response content is empty; check the model profile's " +
				"request shape",
		)
	}
	entries, err := parseEntries(choice.Message.Content)
	if err != nil {
		// The server was asked for constrained decoding, so a violation
		// means it did not enforce the schema; at temperature zero that is
		// deterministic and not worth retrying.
		return nil, parsed.Usage, fmt.Errorf(
			"distilled content violates the response schema (does the "+
				"server enforce json_schema?): %w", err,
		)
	}
	return entries, parsed.Usage, nil
}

// parseEntries decodes and validates distilled content against the same
// constraints entrySchema requests: an entries array must be present,
// keys are matched exactly (Go's struct decoding is case-insensitive, so
// this walks raw messages instead), unknown keys, nulls, and trailing data
// are rejected, and every entry needs a known type, a non-blank title and
// body, and an entities array of strings.
func parseEntries(content string) ([]Entry, error) {
	top, err := strictObject(json.RawMessage(content), []string{"entries"})
	if err != nil {
		return nil, err
	}
	rawEntries, err := strictArray(top["entries"], "entries")
	if err != nil {
		return nil, err
	}
	entryKeys := []string{"type", "title", "body", "entities"}
	entries := make([]Entry, 0, len(rawEntries))
	for i, rawEntry := range rawEntries {
		fields, err := strictObject(rawEntry, entryKeys)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		var entry Entry
		if entry.Type, err = strictString(fields["type"], "type"); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if !slices.Contains(entryTypes, entry.Type) {
			return nil, fmt.Errorf(
				"entry %d: type %q is not one of %s",
				i, entry.Type, strings.Join(entryTypes, ", "),
			)
		}
		if entry.Title, err = strictString(fields["title"], "title"); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if strings.TrimSpace(entry.Title) == "" {
			return nil, fmt.Errorf("entry %d: title is blank", i)
		}
		if entry.Body, err = strictString(fields["body"], "body"); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if strings.TrimSpace(entry.Body) == "" {
			return nil, fmt.Errorf("entry %d: body is blank", i)
		}
		rawEntities, err := strictArray(fields["entities"], "entities")
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		entry.Entities = make([]string, 0, len(rawEntities))
		for j, rawEntity := range rawEntities {
			entity, err := strictString(rawEntity, "entity")
			if err != nil {
				return nil, fmt.Errorf("entry %d, entity %d: %w", i, j, err)
			}
			entry.Entities = append(entry.Entities, entity)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// strictObject unmarshals data as a JSON object holding exactly the given
// keys, matched case-sensitively. json.Unmarshal already rejects trailing
// data after the value.
func strictObject(
	data json.RawMessage, keys []string,
) (map[string]json.RawMessage, error) {
	if isJSONNull(data) {
		return nil, fmt.Errorf("expected an object, got null")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			return nil, fmt.Errorf("required key %q is missing", key)
		}
	}
	for key := range object {
		if !slices.Contains(keys, key) {
			return nil, fmt.Errorf("unknown key %q", key)
		}
	}
	return object, nil
}

func strictArray(
	data json.RawMessage, name string,
) ([]json.RawMessage, error) {
	if isJSONNull(data) {
		return nil, fmt.Errorf("%s must be an array, got null", name)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s must be an array: %w", name, err)
	}
	return list, nil
}

func strictString(data json.RawMessage, name string) (string, error) {
	if isJSONNull(data) {
		return "", fmt.Errorf("%s must be a string, got null", name)
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return "", fmt.Errorf("%s must be a string: %w", name, err)
	}
	return value, nil
}

// isJSONNull matters because json.Unmarshal treats null as a no-op for
// maps, slices, and strings instead of reporting a type mismatch.
func isJSONNull(data json.RawMessage) bool {
	return string(bytes.TrimSpace(data)) == "null"
}

// isTransientStatus reports whether an HTTP status is worth retrying:
// timeouts, rate limits, and server-side errors. Other non-200 statuses
// (auth failures, missing routes, validation rejections) are deterministic
// for the same request and fail fast.
func isTransientStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= 500
}

// parseRetryAfter reads a Retry-After header in either the delay-seconds or
// HTTP-date form, returning zero for absent or unparseable values.
func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

// isContextOverflowDetail reports whether a 400 body identifies an
// input-length error. A structured error code is unambiguous; otherwise
// the message must pair an input-side subject (context, prompt, input)
// with an overflow term (exceed, too long/large, maximum). Bare "token" is
// deliberately not a subject: output-budget rejections like "max_tokens
// exceeds the maximum allowed value" would match it, and splitting the
// input cannot fix an invalid output limit — while every genuine overflow
// phrasing also names the prompt, input, or context. A phrasing this
// misses fails the unit with the server's message intact, which is
// recoverable by configuration; the reverse mistake would send the caller
// splitting units in a useless loop.
func isContextOverflowDetail(body string) bool {
	lower := strings.ToLower(body)
	codes := []string{
		"context_length_exceeded",
		"exceed_context_size_error",
	}
	for _, code := range codes {
		if strings.Contains(lower, code) {
			return true
		}
	}
	subjects := []string{"context", "prompt", "input"}
	overflowTerms := []string{"exceed", "too long", "too large", "maximum"}
	hasSubject := slices.ContainsFunc(subjects, func(subject string) bool {
		return strings.Contains(lower, subject)
	})
	hasOverflowTerm := slices.ContainsFunc(overflowTerms, func(term string) bool {
		return strings.Contains(lower, term)
	})
	return hasSubject && hasOverflowTerm
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
