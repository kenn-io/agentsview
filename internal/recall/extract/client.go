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
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/config"
)

// ErrContextOverflow reports a prompt the server rejected as too large for
// the model context. The unit must be split before retrying; retrying the
// same text can never succeed.
var ErrContextOverflow = errors.New("unit text overflows the model context")

// ErrPersistentTruncation reports output the client cannot recover: the
// token budget truncated the unit's entries. Splitting the unit preserves
// every entry, so the caller splits until SplitFloorChars; a unit at the
// floor that still truncates needs a larger max_tokens, and the error says
// so. There is deliberately no retry that caps the entry count: a capped
// response looks complete while silently dropping entries.
var ErrPersistentTruncation = errors.New(
	"model output truncated at the token budget",
)

// ErrSplitBudgetExceeded reports a unit whose overflow recovery ran past the
// per-unit split budget: one oversized transcript message would otherwise
// drive an unbounded fan-out of model calls and accumulate every leaf's
// entries in memory. It is neither transient nor endpoint-scoped, so the
// session fails closed and waits out its backoff rather than aborting the
// pass or retrying the same oversized message every time.
var ErrSplitBudgetExceeded = errors.New(
	"unit overflow recovery exceeded the split budget",
)

// errTruncated is the per-request truncation signal that
// DistillWithRecovery converts into the typed split signal.
var errTruncated = errors.New("model output truncated")

// errPermanentRequest marks a server rejection that retrying the same
// request can never fix (wrong model name, bad credentials, malformed
// field).
var errPermanentRequest = errors.New("model server rejected the request")

// errProtocolViolation marks a 200 whose content violates the
// constrained-decoding contract: the server was asked to enforce
// json_schema and did not. That is an endpoint property, not a fact about
// the submitted transcript, so it classifies as endpoint-scoped.
var errProtocolViolation = errors.New(
	"distill response violates the extraction protocol",
)

// requestStatusError carries the HTTP status of a permanent server
// rejection so callers can tell endpoint-scoped failures from
// input-specific ones.
type requestStatusError struct {
	status int
	err    error
}

func (e *requestStatusError) Error() string { return e.err.Error() }
func (e *requestStatusError) Unwrap() error { return e.err }

// endpointScopedRejection reports whether err is a permanent failure that
// indicts the endpoint or configuration rather than the submitted unit:
// authentication (401), authorization (403), unknown route or model (404),
// wrong method or media type on the route (405, 415), an unimplemented
// route (501), and schema-violating 200s all fail every request equally,
// so visiting further sessions burns one doomed model call and one failure
// backoff apiece. A 400 stays input-scoped — the same endpoint answers
// other units fine when only this request's content is refused.
func endpointScopedRejection(err error) bool {
	if errors.Is(err, errProtocolViolation) ||
		errors.Is(err, errRedirectRefused) {
		return true
	}
	var rejection *requestStatusError
	if !errors.As(err, &rejection) {
		return false
	}
	switch rejection.status {
	case http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusUnsupportedMediaType, http.StatusNotImplemented:
		return true
	}
	return false
}

// errRedirectRefused marks a refused redirect. Deterministic: the same
// endpoint redirects every identical request, so it is endpoint-scoped.
var errRedirectRefused = errors.New("extraction requests do not follow redirects")

// RefuseRedirects is the CheckRedirect policy for extraction HTTP clients:
// redirects are never followed. A 307/308 replays the extraction POST —
// transcript content included — to whatever destination the endpoint
// names, letting a compromised endpoint exfiltrate the request or aim it
// at loopback services that trust local callers. A name-based same-origin
// allowance would not close this: the redirect target is re-resolved, so a
// rebinding hostname passes any string comparison while the connection
// lands elsewhere. Endpoints must be configured with their final URL.
func RefuseRedirects(req *http.Request, _ []*http.Request) error {
	// The target is redacted: a redirect can name a URL carrying
	// credentials or signed tokens, and this message reaches stderr and
	// stored failure rows.
	return fmt.Errorf(
		"%w: refusing redirect to %q; configure the endpoint's final URL",
		errRedirectRefused,
		boundedToken(config.RedactedEndpoint(req.URL.String()), 200))
}

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

// extractionProtocolVersion feeds the generation fingerprint. Bump it when
// the response schema or the recovery behavior changes in a way that alters
// extraction output for an identical configuration.
// v2: minLength constraints on entry title and body.
// v3: truncation always splits; the entry-capped compact retry is gone.
// v4: maxItems/maxLength bounds on entries, fields, and entities.
const extractionProtocolVersion = 4

// Local resource bounds on a single distill response. The transport cap
// only bounds bytes; within it a compromised or misconfigured endpoint
// could return tens of thousands of entries or multi-megabyte fields, and
// accepting them would balloon the archive (and its FTS index) and hold
// the write lock through the inserts. entrySchema declares the same limits
// (maxItems/maxLength), so a compliant constrained-decoding server never
// produces a response the client refuses. Lengths count Unicode code
// points to match JSON Schema maxLength semantics.
const (
	maxResponseEntries = 100
	maxEntryTitleChars = 500
	maxEntryBodyChars  = 5000
	maxEntryEntities   = 50
	maxEntityChars     = 200
)

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
			"type":     "array",
			"maxItems": maxResponseEntries,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{
						"type": "string",
						"enum": entryTypes,
					},
					"title": map[string]any{
						"type": "string", "minLength": 1,
						"maxLength": maxEntryTitleChars,
					},
					"body": map[string]any{
						"type": "string", "minLength": 1,
						"maxLength": maxEntryBodyChars,
					},
					"entities": map[string]any{
						"type":     "array",
						"maxItems": maxEntryEntities,
						"items": map[string]any{
							"type":      "string",
							"maxLength": maxEntityChars,
						},
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
		if c.HTTPClient.CheckRedirect == nil {
			// The no-redirect policy is the extraction client's own
			// boundary, not the caller's wiring: a redirect replays the
			// extraction POST — transcript included — wherever the
			// endpoint points. A caller-provided client keeps its other
			// settings; only a missing policy is filled in, on a copy.
			enforced := *c.HTTPClient
			enforced.CheckRedirect = RefuseRedirects
			return &enforced
		}
		return c.HTTPClient
	}
	return &http.Client{
		Timeout:       10 * time.Minute,
		CheckRedirect: RefuseRedirects,
	}
}

func (c *Client) retryBackoff() time.Duration {
	if c.RetryBackoff > 0 {
		return c.RetryBackoff
	}
	return 500 * time.Millisecond
}

// ValidateRequestShape rejects a request configuration that would fail
// every model call identically. Manager construction calls it so a bad
// profile fails daemon or CLI setup outright, before any progress rows are
// created; DistillWithRecovery re-checks as a backstop for direct callers.
func (c *Client) ValidateRequestShape() error {
	if c.Request.MaxTokens <= 0 {
		return fmt.Errorf(
			"extraction request max_tokens must be positive; the profile or "+
				"configuration must set it (got %d)", c.Request.MaxTokens,
		)
	}
	for _, key := range reservedRequestKeys {
		if _, ok := c.Request.ExtraBody[key]; ok {
			return fmt.Errorf(
				"extra body must not set reserved request field %q; use the "+
					"dedicated configuration for it", key,
			)
		}
	}
	return nil
}

// DistillWithRecovery runs one unit through the model with the recovery
// ladder: transient failures (network errors, timeouts, rate limits,
// server errors) are retried up to maxAttempts with exponential backoff
// honoring Retry-After, while permanent rejections fail fast; truncated
// output surfaces as ErrPersistentTruncation and a context overflow as
// ErrContextOverflow — both mean "split this unit", which the caller owns
// because it also owns unit identity. The returned Usage sums every
// attempt, successful or not, so recovery costs are accounted.
func (c *Client) DistillWithRecovery(
	ctx context.Context, systemPrompt, text string, maxAttempts int,
) ([]Entry, Usage, error) {
	var total Usage
	if err := c.ValidateRequestShape(); err != nil {
		return nil, total, err
	}
	var lastErr error
	for attempt := range maxAttempts {
		entries, usage, err := c.distill(ctx, systemPrompt, text)
		total.PromptTokens += usage.PromptTokens
		total.CompletionTokens += usage.CompletionTokens
		if err == nil {
			return entries, total, nil
		}
		if errors.Is(err, errTruncated) {
			// Rune count, not byte length: split budgets count code points.
			return nil, total, fmt.Errorf(
				"unit of %d chars at max_tokens=%d (split the unit, or raise "+
					"max_tokens if it is already at the split floor): %w",
				utf8.RuneCountInString(text), c.Request.MaxTokens,
				ErrPersistentTruncation,
			)
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
	return nil, total, fmt.Errorf(
		"distilling unit after %d attempts: %w", maxAttempts, lastErr,
	)
}

func (c *Client) distill(
	ctx context.Context, systemPrompt, text string,
) ([]Entry, Usage, error) {
	payload := map[string]any{
		"model":       c.Model,
		"max_tokens":  c.Request.MaxTokens,
		"temperature": c.Request.Temperature,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": text},
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
	// The route joins the parsed endpoint path so query parameters
	// (Azure-style ?api-version=...) survive; string concatenation would
	// land the route inside the query value.
	endpoint, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, Usage{}, fmt.Errorf(
			"invalid endpoint %s", config.RedactedEndpoint(c.BaseURL))
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/chat/completions"
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body),
	)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("building distill request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient().Do(request)
	if err != nil {
		// url.Error echoes the request URL with only the password masked:
		// the username (which can itself be an API key) and the query
		// string still leak, and this message reaches doctor output and
		// stored failure rows. Report the redacted endpoint instead.
		cause := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			cause = urlErr.Err
		}
		if errors.Is(cause, errRedirectRefused) {
			// Deterministic for every request against the same
			// configuration: fail fast, endpoint-scoped, no retries.
			if c.credentialedEndpoint() {
				// The refusal names its redirect target, and a redirect
				// can put the endpoint credential in the target hostname,
				// which RedactedEndpoint preserves. Keep the sentinel
				// alone.
				return nil, Usage{}, fmt.Errorf(
					"posting distill request to %s: %w",
					config.RedactedEndpoint(endpoint.String()),
					errRedirectRefused)
			}
			// The cause is RefuseRedirects' own message with the target
			// redacted and bounded, so it stays on the chain verbatim.
			return nil, Usage{}, fmt.Errorf(
				"posting distill request to %s: %w",
				config.RedactedEndpoint(endpoint.String()), cause)
		}
		// Beyond the URL, the cause text itself can quote
		// server-controlled bytes — a malformed redirect Location is
		// parsed before CheckRedirect runs and echoed raw by its parse
		// error, and a malformed HTTP response is quoted verbatim — so
		// the detail is withheld for credentialed endpoints and
		// control-stripped and bounded otherwise. Classification needs
		// nothing from the chain here: everything but a redirect
		// refusal is transient.
		return nil, Usage{}, &transientError{err: fmt.Errorf(
			"posting distill request to %s: %s",
			config.RedactedEndpoint(endpoint.String()),
			c.transportErrorDetail(cause))}
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		// Read errors quote raw wire bytes — a malformed chunked trailer
		// line is echoed verbatim — so the detail follows the same policy
		// as transport errors.
		return nil, Usage{}, &transientError{
			err: fmt.Errorf("reading distill response: %s",
				c.transportErrorDetail(err)),
		}
	}
	if response.StatusCode == http.StatusBadRequest {
		detail := c.responseDetail(raw)
		// Chat servers answer 400 both for prompts that exceed the context
		// window (character budgets overshoot because token density varies
		// across content) and for genuinely bad requests; only the former
		// is recoverable by splitting the unit.
		if isContextOverflowDetail(string(raw)) {
			return nil, Usage{}, fmt.Errorf(
				"%d-char unit: %w: %s",
				utf8.RuneCountInString(text), ErrContextOverflow, detail,
			)
		}
		return nil, Usage{}, &requestStatusError{
			status: http.StatusBadRequest,
			err: fmt.Errorf(
				"%w (HTTP 400): %s", errPermanentRequest, detail,
			),
		}
	}
	if response.StatusCode == http.StatusRequestEntityTooLarge {
		// The transport-level twin of an in-band context-overflow 400:
		// the request body is too big, and only splitting the unit can
		// shrink it. A permanent failure here would retry the session
		// unchanged after every backoff, forever.
		return nil, Usage{}, fmt.Errorf(
			"%d-char unit: %w (HTTP 413): %s",
			utf8.RuneCountInString(text), ErrContextOverflow,
			c.responseDetail(raw),
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
		return nil, Usage{}, &requestStatusError{
			status: response.StatusCode,
			err: fmt.Errorf(
				"%w (HTTP %d): %s", errPermanentRequest,
				response.StatusCode, c.responseDetail(raw),
			),
		}
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
		cause := fmt.Errorf("parsing distill response: %w", err)
		if c.credentialedEndpoint() {
			cause = fmt.Errorf("parsing distill response failed %s",
				detailWithheld)
		}
		return nil, Usage{}, &transientError{err: cause}
	}
	if len(parsed.Choices) == 0 {
		// The body parsed, so its usage is real cost even without choices.
		return nil, parsed.Usage, &transientError{
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
	if choice.FinishReason != "stop" {
		// A filtered or otherwise cut-off response can still carry valid
		// JSON with fewer entries; only "stop" means the model finished.
		// Deterministic for the same input, so it fails fast.
		return nil, parsed.Usage, fmt.Errorf(
			"distill response finished with %q instead of completing; "+
				"refusing possibly incomplete entries",
			c.responseToken(choice.FinishReason),
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
		violation := fmt.Errorf(
			"%w: distilled content violates the response schema (does "+
				"the server enforce json_schema?): %w",
			errProtocolViolation, err,
		)
		if c.credentialedEndpoint() {
			// The violating content is endpoint-supplied; its detail
			// (unknown keys, field values) can reflect the credential.
			violation = fmt.Errorf(
				"%w: distilled content violates the response schema "+
					"(does the server enforce json_schema?); detail %s",
				errProtocolViolation, detailWithheld,
			)
		}
		return nil, parsed.Usage, violation
	}
	return entries, parsed.Usage, nil
}

// credentialedEndpoint reports whether the configured endpoint URL
// carries credential material: userinfo, or any raw query segment whose
// key is not the api-version surface selector (mirroring the config
// redactor's fail-closed allowlist). Raw wire segments, no parser:
// url.ParseQuery would reject exactly the malformed queries that still
// travel verbatim, and a rejection must not fail open. An unparseable URL
// counts as credentialed for the same reason.
func (c *Client) credentialedEndpoint() bool {
	endpoint, err := url.Parse(c.BaseURL)
	if err != nil {
		return true
	}
	if endpoint.User != nil {
		return true
	}
	for _, segment := range strings.FieldsFunc(
		endpoint.RawQuery,
		func(r rune) bool { return r == '&' || r == ';' },
	) {
		key, _, _ := strings.Cut(segment, "=")
		if !strings.EqualFold(key, "api-version") {
			return true
		}
	}
	// Path segments outside the known API-surface vocabulary count too:
	// webhook-style gateways authenticate through path tokens, which can
	// be short or split across segments, so no length heuristic is safe.
	// The allowlist covers the standard OpenAI-compatible shapes (local
	// inference servers at /v1, gateways at /api/v1, Gemini's
	// /v1beta/openai) — the doctor's main diagnostic targets. Anything
	// else, Azure deployment names included, fails closed to withholding
	// the endpoint-provided detail; the HTTP status always survives.
	for segment := range strings.SplitSeq(endpoint.Path, "/") {
		if !safeEndpointPathSegments[strings.ToLower(segment)] {
			return true
		}
	}
	return false
}

// safeEndpointPathSegments are the only path segments that keep
// endpoint-provided diagnostics flowing: names that select the API
// surface, never credentials. The empty string covers leading, trailing,
// and doubled slashes.
var safeEndpointPathSegments = map[string]bool{
	"":       true,
	"v1":     true,
	"v1beta": true,
	"api":    true,
	"openai": true,
}

// detailWithheld replaces every endpoint-derived detail in error text when
// the endpoint URL carries credential material: a server that knows the
// credential can reflect it through any response field — body,
// finish_reason, a schema-violating key — in re-encodings no replacement
// list can enumerate, and these errors reach persisted failure rows,
// scheduler logs, doctor stderr, and CI logs.
const detailWithheld = "(withheld: endpoint URL carries credential material)"

// responseDetail prepares a response-body excerpt for error text. Bodies
// are attacker-influenced; for credentialed endpoints they are withheld
// outright, and otherwise kept as a control-stripped, length-capped
// excerpt, which is where the diagnostic value lives (wrong model name,
// malformed field).
func (c *Client) responseDetail(raw []byte) string {
	if _, err := url.Parse(c.BaseURL); err != nil {
		return "(response body withheld: unparseable endpoint)"
	}
	if c.credentialedEndpoint() {
		return "(response body withheld: endpoint URL carries " +
			"credential material)"
	}
	detail := stripControls(string(raw))
	if len(detail) > 200 {
		cut := 200
		for cut > 0 && !utf8.RuneStart(detail[cut]) {
			cut--
		}
		detail = detail[:cut]
	}
	return detail
}

// transportErrorDetail prepares a transport-layer error's text for error
// messages. Context errors pass through: their text is fixed by the
// runtime and says why the request ended (canceled, timed out). Anything
// else can quote bytes the server chose, so it is withheld for
// credentialed endpoints and control-stripped and length-bounded
// otherwise.
func (c *Client) transportErrorDetail(cause error) string {
	if errors.Is(cause, context.Canceled) ||
		errors.Is(cause, context.DeadlineExceeded) {
		return cause.Error()
	}
	if c.credentialedEndpoint() {
		return detailWithheld
	}
	return boundedToken(cause.Error(), 200)
}

// responseToken prepares a short endpoint-supplied token (a finish_reason)
// for error text: withheld for credentialed endpoints, bounded otherwise.
func (c *Client) responseToken(value string) string {
	if c.credentialedEndpoint() {
		return detailWithheld
	}
	return boundedToken(value, 60)
}

// stripControls replaces C0/C1 control characters and invalid UTF-8 with
// spaces: response bytes reach terminals and logs, where an escape
// sequence or carriage return forges or hides output.
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) ||
			r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
}

// boundedToken caps an endpoint-supplied token for error text: a hostile
// finish_reason or entry field can approach the transport limit, and these
// errors persist into per-session failure rows.
func boundedToken(value string, maxRunes int) string {
	value = stripControls(value)
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…(truncated)"
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
	if len(rawEntries) > maxResponseEntries {
		return nil, fmt.Errorf(
			"response carries %d entries, limit %d",
			len(rawEntries), maxResponseEntries,
		)
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
				i, boundedToken(entry.Type, 60),
				strings.Join(entryTypes, ", "),
			)
		}
		if entry.Title, err = strictString(fields["title"], "title"); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if strings.TrimSpace(entry.Title) == "" {
			return nil, fmt.Errorf("entry %d: title is blank", i)
		}
		if n := utf8.RuneCountInString(entry.Title); n > maxEntryTitleChars {
			return nil, fmt.Errorf(
				"entry %d: title is %d characters, limit %d",
				i, n, maxEntryTitleChars,
			)
		}
		if entry.Body, err = strictString(fields["body"], "body"); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if strings.TrimSpace(entry.Body) == "" {
			return nil, fmt.Errorf("entry %d: body is blank", i)
		}
		if n := utf8.RuneCountInString(entry.Body); n > maxEntryBodyChars {
			return nil, fmt.Errorf(
				"entry %d: body is %d characters, limit %d",
				i, n, maxEntryBodyChars,
			)
		}
		rawEntities, err := strictArray(fields["entities"], "entities")
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if len(rawEntities) > maxEntryEntities {
			return nil, fmt.Errorf(
				"entry %d: carries %d entities, limit %d",
				i, len(rawEntities), maxEntryEntities,
			)
		}
		entry.Entities = make([]string, 0, len(rawEntities))
		for j, rawEntity := range rawEntities {
			entity, err := strictString(rawEntity, "entity")
			if err != nil {
				return nil, fmt.Errorf("entry %d, entity %d: %w", i, j, err)
			}
			if n := utf8.RuneCountInString(entity); n > maxEntityChars {
				return nil, fmt.Errorf(
					"entry %d, entity %d: %d characters, limit %d",
					i, j, n, maxEntityChars,
				)
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
			// The key is endpoint-supplied and can approach the transport
			// limit; bound it before it reaches logs and failure rows.
			return nil, fmt.Errorf("unknown key %q", boundedToken(key, 60))
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
	if status == http.StatusNotImplemented {
		// 501 is deterministic: the route itself is unimplemented, and
		// retrying the identical request cannot implement it.
		return false
	}
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
	containsAny := func(needles []string) bool {
		return slices.ContainsFunc(needles, func(needle string) bool {
			return strings.Contains(lower, needle)
		})
	}
	hasOverflowTerm := containsAny(
		[]string{"exceed", "too long", "too large", "maximum"},
	)
	// Explicit input-side length evidence wins outright — including
	// combined-budget messages like "input tokens plus max_tokens exceed
	// the model maximum", which splitting the input does fix. Bare "input"
	// and bare "prompt" are deliberately not subjects: servers prefix
	// arbitrary validation errors with "Input validation error:", and
	// parameter names like prompt_logprobs would pair with any overflow
	// term. Length-qualified forms are safe — "invalid input length
	// parameter" has no overflow term.
	if containsAny([]string{
		"input is too long", "input too long",
		"input is too large", "input too large",
		"prompt is too long", "prompt too long",
		"prompt is too large", "prompt too large",
	}) {
		return true
	}
	if hasOverflowTerm && containsAny([]string{
		"input length", "input tokens", "prompt length", "prompt tokens",
	}) {
		return true
	}
	// With no input-side evidence, a message naming an output-budget
	// parameter is a configuration error — even "max_tokens exceeds the
	// context window" is not fixed by splitting the input.
	if containsAny([]string{
		"max_tokens", "max_new_tokens", "max_completion_tokens",
	}) {
		return false
	}
	// Bare "context" paired with an overflow term covers the common server
	// phrasings: "maximum context length is ...", "exceeds the available
	// context size".
	return hasOverflowTerm && strings.Contains(lower, "context")
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
