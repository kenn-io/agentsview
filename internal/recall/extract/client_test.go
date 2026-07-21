package extract

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type scriptedResponse struct {
	status       int
	finishReason string
	content      string
	errorBody    string
	noChoices    bool
}

func newScriptedServer(
	t *testing.T, responses []scriptedResponse, requests *[]map[string]any,
) *httptest.Server {
	t.Helper()
	var index atomic.Int64
	return httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
				t.Errorf("request path = %q, want /chat/completions", r.URL.Path)
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decoding request: %v", err)
			}
			*requests = append(*requests, payload)
			i := int(index.Add(1)) - 1
			if i >= len(responses) {
				t.Errorf("unexpected request %d", i)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			resp := responses[i]
			if resp.status != 0 && resp.status != http.StatusOK {
				w.WriteHeader(resp.status)
				errorBody := resp.errorBody
				if errorBody == "" {
					errorBody = `{"error":"scripted"}`
				}
				_, _ = w.Write([]byte(errorBody))
				return
			}
			choices := []map[string]any{{
				"finish_reason": resp.finishReason,
				"message": map[string]any{
					"role":    "assistant",
					"content": resp.content,
				},
			}}
			if resp.noChoices {
				choices = nil
			}
			body := map[string]any{
				"choices": choices,
				"usage": map[string]any{
					"prompt_tokens":     7,
					"completion_tokens": 3,
				},
			}
			_ = json.NewEncoder(w).Encode(body)
		}))
}

func entriesJSON(t *testing.T, titles ...string) string {
	t.Helper()
	entries := make([]map[string]any, 0, len(titles))
	for _, title := range titles {
		entries = append(entries, map[string]any{
			"type": "fact", "title": title, "body": "b",
			"entities": []string{},
		})
	}
	raw, err := json.Marshal(map[string]any{"entries": entries})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func testClient(url string) *Client {
	return &Client{
		BaseURL: url,
		Model:   "test-model",
		// Keep transient-retry backoff out of test wall-clock time.
		RetryBackoff: time.Millisecond,
		Request: RequestShape{
			Temperature: 0,
			MaxTokens:   100,
			ExtraBody: map[string]any{
				"chat_template_kwargs": map[string]any{
					"enable_thinking": false,
				},
			},
		},
	}
}

func TestClientDistillParsesEntriesAndSendsShape(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "stop", content: entriesJSON(t, "one", "two")},
	}, &requests)
	defer server.Close()

	entries, usage, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "system prompt", "unit text", 3,
	)
	if err != nil {
		t.Fatalf("DistillWithRecovery: %v", err)
	}
	if len(entries) != 2 || entries[0].Title != "one" {
		t.Fatalf("entries = %+v", entries)
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	payload := requests[0]
	if payload["temperature"] != float64(0) {
		t.Fatalf("temperature = %v", payload["temperature"])
	}
	if payload["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens = %v", payload["max_tokens"])
	}
	if _, ok := payload["chat_template_kwargs"]; !ok {
		t.Fatal("extra body must be merged into the request")
	}
	if _, ok := payload["response_format"]; !ok {
		t.Fatal("constrained decoding must be requested")
	}
}

func TestClientTrailingSlashBaseURL(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "stop", content: entriesJSON(t, "one")},
	}, &requests)
	defer server.Close()

	client := testClient(server.URL + "/")
	entries, _, err := client.DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestClientTruncationIsTypedSplitSignal(t *testing.T) {
	// Truncation is never retried or compacted: any retry that caps the
	// entry count would look complete while silently dropping entries, so
	// the only recovery is the caller splitting the unit.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "length", content: ""},
	}, &requests)
	defer server.Close()

	_, usage, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "unit text", 3,
	)
	if !errors.Is(err, ErrPersistentTruncation) {
		t.Fatalf("err = %v, want ErrPersistentTruncation", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1 (truncation is deterministic)",
			len(requests))
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want the truncated attempt accounted", usage)
	}
}

func TestClientBadRequestMentioningContextIsNotOverflow(t *testing.T) {
	// "context" alone must not classify as overflow: 400s for unrelated
	// problems can mention the word without describing an input-length
	// error, and splitting the unit would loop uselessly.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{
			status: http.StatusBadRequest,
			errorBody: `{"error":{"message":"unknown field ` +
				`\"chat_template_kwargs\" in request context"}}`,
		},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err == nil || errors.Is(err, ErrContextOverflow) {
		t.Fatalf("err = %v, must be a permanent non-overflow error", err)
	}
}

func TestClientContextOverflowIsTyped(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{
			status: http.StatusBadRequest,
			errorBody: `{"error":{"message":"This model's maximum context ` +
				`length is 32768 tokens."}}`,
		},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("err = %v, want ErrContextOverflow", err)
	}
}

func TestClientBadRequestOtherThanOverflowIsPermanent(t *testing.T) {
	// A 400 for a wrong model name or malformed field must not masquerade as
	// an overflow (which would make the caller split the unit), and it will
	// not fix itself, so it must not burn the transient retry budget either.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{
			status:    http.StatusBadRequest,
			errorBody: `{"error":{"message":"model \"test-model\" not found"}}`,
		},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err == nil {
		t.Fatal("bad request must be an error")
	}
	if errors.Is(err, ErrContextOverflow) {
		t.Fatalf("err = %v, must not be ErrContextOverflow", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, must carry the server detail", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1 (bad requests are not retried)",
			len(requests))
	}
}

func TestClientTruncationAfterTransientRetryAccountsUsage(t *testing.T) {
	// A transient failure followed by truncation must surface the split
	// signal with the usage of every attempt that reported it.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{status: http.StatusInternalServerError},
		{finishReason: "length", content: ""},
	}, &requests)
	defer server.Close()

	_, usage, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if !errors.Is(err, ErrPersistentTruncation) {
		t.Fatalf("err = %v, want ErrPersistentTruncation", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2 (one transient retry, then "+
			"truncation)", len(requests))
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want the truncated attempt accounted", usage)
	}
}

func TestClientRetriesTransientErrors(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{status: http.StatusInternalServerError},
		{status: http.StatusTooManyRequests},
		{finishReason: "stop", content: entriesJSON(t, "ok")},
	}, &requests)
	defer server.Close()

	entries, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3 (5xx and 429 are transient)",
			len(requests))
	}
}

func TestClientPermanentHTTPStatusFailsFast(t *testing.T) {
	// 401/403/404 will not fix themselves; retrying burns the budget and
	// hides the configuration problem behind attempt noise.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{status: http.StatusUnauthorized},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err == nil {
		t.Fatal("unauthorized must be an error")
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1 (permanent statuses are not retried)",
			len(requests))
	}
}

func TestClientRejectsReservedExtraBodyKeys(t *testing.T) {
	// A profile or override smuggling max_tokens through the extra body
	// would bypass validation and desynchronize the generation fingerprint
	// from the request actually sent.
	var requests []map[string]any
	server := newScriptedServer(t, nil, &requests)
	defer server.Close()

	client := testClient(server.URL)
	client.Request.ExtraBody = map[string]any{"max_tokens": 5}
	_, _, err := client.DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("err = %v, want reserved-key rejection naming the key", err)
	}
	if len(requests) != 0 {
		t.Fatalf("requests = %d, want 0 (rejected before any call)",
			len(requests))
	}
}

func TestIsContextOverflowDetail(t *testing.T) {
	overflow := []string{
		`{"error":{"code":"context_length_exceeded"}}`,
		`This model's maximum context length is 32768 tokens.`,
		`the request exceeds the available context size`,
		`prompt is too long: 20000 tokens > 16000 maximum`,
		`input is too large for the model context`,
		`input length 5000 exceeds maximum 4096`,
		`input tokens (6000) exceed the model maximum`,
		`input tokens plus max_tokens exceed the model maximum`,
		`prompt is too large for this model`,
		`prompt too large: reduce the input`,
	}
	for _, body := range overflow {
		if !isContextOverflowDetail(body) {
			t.Errorf("must classify as overflow: %q", body)
		}
	}
	// A length-related noun alone is not an overflow: these are validation
	// errors that splitting the unit can never fix.
	notOverflow := []string{
		`{"error":{"message":"context window must be an integer"}}`,
		`invalid input length parameter`,
		`unknown field "context_size" in request`,
		`max_tokens must be a positive integer`,
		`max_tokens exceeds the maximum allowed value`,
		`max_new_tokens exceeds the model limit`,
		`Input validation error: max_tokens exceeds the maximum allowed value`,
		`Input validation error: temperature exceeds the maximum allowed value`,
		`invalid input: repetition_penalty exceeds maximum`,
		`prompt_logprobs exceeds maximum allowed value`,
		`max_tokens (5000) exceeds the context window (4096)`,
		`model "test-model" not found`,
	}
	for _, body := range notOverflow {
		if isContextOverflowDetail(body) {
			t.Errorf("must not classify as overflow: %q", body)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Fatalf("parseRetryAfter(2) = %v", got)
	}
	future := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(future)
	if got <= 0 || got > 5*time.Second {
		t.Fatalf("parseRetryAfter(http-date) = %v", got)
	}
	for _, value := range []string{"", "garbage", "-3"} {
		if got := parseRetryAfter(value); got != 0 {
			t.Fatalf("parseRetryAfter(%q) = %v, want 0", value, got)
		}
	}
}

func TestClientChoicelessResponseAccountsUsageAcrossRetry(t *testing.T) {
	// A choiceless 200 still reports token usage; the retry that recovers
	// from it must not drop that cost from the accounting.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{noChoices: true},
		{finishReason: "stop", content: entriesJSON(t, "ok")},
	}, &requests)
	defer server.Close()

	entries, usage, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2 (choiceless responses retry)",
			len(requests))
	}
	if usage.PromptTokens != 14 || usage.CompletionTokens != 6 {
		t.Fatalf("usage = %+v, want both attempts accounted (14/6)", usage)
	}
}

func TestClientNonStopFinishReasonIsError(t *testing.T) {
	// A content-filtered or otherwise cut-off response can still carry
	// valid JSON with fewer (or zero) entries; accepting it would advance
	// progress over silently lost facts. Only "stop" means complete.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "content_filter", content: entriesJSON(t, "partial")},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err == nil || !strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("err = %v, want an error naming the finish reason", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1 (a filtered response is "+
			"deterministic)", len(requests))
	}
}

func TestClientEmptyContentIsError(t *testing.T) {
	// A model that burns its budget on hidden reasoning returns empty
	// content with finish_reason stop; that must surface as an error, not
	// as zero entries — and at temperature zero a same-input retry gives
	// the same emptiness, so it must not be retried.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "stop", content: ""},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err == nil {
		t.Fatal("empty content must be an error")
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1 (deterministic emptiness is not retried)",
			len(requests))
	}
}

func TestClientRejectsSchemaViolatingContent(t *testing.T) {
	// Constrained decoding is requested, but not every server enforces it;
	// content that violates the schema must fail the unit instead of
	// advancing progress with silently lost or malformed entries. At
	// temperature zero the violation is deterministic, so no retry.
	cases := map[string]string{
		"empty object":        `{}`,
		"null":                `null`,
		"top-level array":     `[]`,
		"unknown type":        `{"entries":[{"type":"story","title":"t","body":"b","entities":[]}]}`,
		"blank title":         `{"entries":[{"type":"fact","title":" ","body":"b","entities":[]}]}`,
		"blank body":          `{"entries":[{"type":"fact","title":"t","body":"","entities":[]}]}`,
		"missing entities":    `{"entries":[{"type":"fact","title":"t","body":"b"}]}`,
		"unknown field":       `{"entries":[{"type":"fact","title":"t","body":"b","entities":[],"extra":1}]}`,
		"case-mismatched key": `{"Entries":[]}`,
		"null entries":        `{"entries":null}`,
		"null entry":          `{"entries":[null]}`,
		"null title":          `{"entries":[{"type":"fact","title":null,"body":"b","entities":[]}]}`,
		"null entity element": `{"entries":[{"type":"fact","title":"t","body":"b","entities":["a",null]}]}`,
		"non-string entity":   `{"entries":[{"type":"fact","title":"t","body":"b","entities":[1]}]}`,
		"trailing delimiter":  `{"entries":[]}]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			var requests []map[string]any
			server := newScriptedServer(t, []scriptedResponse{
				{finishReason: "stop", content: content},
			}, &requests)
			defer server.Close()

			entries, _, err := testClient(server.URL).DistillWithRecovery(
				context.Background(), "p", "text", 3,
			)
			if err == nil {
				t.Fatalf("content %q must be rejected, got entries %+v",
					content, entries)
			}
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want 1 (schema violations are "+
					"deterministic)", len(requests))
			}
		})
	}
}

func TestClientAcceptsEmptyEntriesArray(t *testing.T) {
	// A unit can legitimately yield nothing; an explicit empty array is
	// schema-valid and distinct from a response that lacks the array.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "stop", content: `{"entries":[]}`},
	}, &requests)
	defer server.Close()

	entries, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err != nil {
		t.Fatalf("DistillWithRecovery: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none", entries)
	}
}

// TestClientErrorDetailRedactsReflectedCredentials pins that response-body
// excerpts embedded in non-transient errors cannot leak the configured
// endpoint's credentials: proxies and gateways echo the request back —
// the URI with its query values (raw or URL-escaped) and the Basic-auth
// header the userinfo becomes — and these error strings reach doctor
// stderr and stored failure rows.
func TestClientErrorDetailRedactsReflectedCredentials(t *testing.T) {
	const user, pass, keyValue = "tester", "hunter2pass", "sekret-value"
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	reflected := fmt.Sprintf(
		`{"error":"denied for http://%s:%s@upstream/v1?api_key=%s "+
			"escaped=%s auth=Basic %s"}`,
		user, pass, keyValue, url.QueryEscape(keyValue), basic,
	)
	for name, status := range map[string]int{
		"bad request": http.StatusBadRequest,
		"not found":   http.StatusNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			var requests []map[string]any
			server := newScriptedServer(t, []scriptedResponse{
				{status: status, errorBody: reflected},
			}, &requests)
			defer server.Close()

			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatalf("parsing test server URL: %v", err)
			}
			endpoint.User = url.UserPassword(user, pass)
			endpoint.RawQuery = "api_key=" + keyValue
			client := testClient(endpoint.String())

			_, _, derr := client.DistillWithRecovery(
				context.Background(), "p", "text", 1,
			)
			if derr == nil {
				t.Fatal("scripted failure must surface an error")
			}
			for _, secret := range []string{pass, keyValue, basic} {
				if strings.Contains(derr.Error(), secret) {
					t.Fatalf("error leaks reflected credential %q: %v",
						secret, derr)
				}
			}
		})
	}
}

// TestClientErrorDetailRedactsRawQueryForms pins the two ways decoded-form
// masking alone fails: a query url.ParseQuery rejects (masking must not
// fail open with it), and a noncanonically encoded value whose exact wire
// bytes — what a body echoing the request URI carries — differ from both
// the decoded and the canonically re-encoded form.
func TestClientErrorDetailRedactsRawQueryForms(t *testing.T) {
	cases := map[string]struct {
		rawQuery string
		echo     string
		secrets  []string
	}{
		"malformed separator": {
			rawQuery: "sig=hunter2secret;api-version=2024",
			echo:     `{"error":"denied: /v1?sig=hunter2secret;api-version=2024"}`,
			secrets:  []string{"hunter2secret"},
		},
		"noncanonical escape": {
			rawQuery: "api_key=se%6Bret-value",
			echo: `{"error":"denied for /v1?api_key=se%6Bret-value ` +
				`(decoded sekret-value)"}`,
			secrets: []string{"se%6Bret-value", "sekret-value"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var requests []map[string]any
			server := newScriptedServer(t, []scriptedResponse{
				{status: http.StatusForbidden, errorBody: tc.echo},
			}, &requests)
			defer server.Close()

			client := testClient(server.URL + "?" + tc.rawQuery)
			_, _, err := client.DistillWithRecovery(
				context.Background(), "p", "text", 1,
			)
			if err == nil {
				t.Fatal("scripted failure must surface an error")
			}
			for _, secret := range tc.secrets {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks reflected credential %q: %v",
						secret, err)
				}
			}
		})
	}
}

// TestClientErrorDetailWithholdsBodyForCredentialedEndpoints pins that a
// response body from an endpoint whose URL carries credential material is
// never excerpted at all: literal scrubbing loses against an endpoint that
// re-encodes the credential (JSON \u escapes turn sekret into
// se\u006bret) or against secrets shorter than any masking floor.
func TestClientErrorDetailWithholdsBodyForCredentialedEndpoints(t *testing.T) {
	cases := map[string]struct {
		mutate  func(u *url.URL)
		echo    string
		secrets []string
	}{
		"short password": {
			mutate: func(u *url.URL) {
				u.User = url.UserPassword("tester", "zq7")
			},
			echo:    `{"error":"denied for http://tester:zq7@upstream/v1"}`,
			secrets: []string{"zq7"},
		},
		"json unicode escape": {
			mutate: func(u *url.URL) {
				u.RawQuery = "api_key=sekret-value"
			},
			echo:    `{"error":"denied for /v1?api_key=se\u006bret-value"}`,
			secrets: []string{`se\u006bret-value`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var requests []map[string]any
			server := newScriptedServer(t, []scriptedResponse{
				{status: http.StatusForbidden, errorBody: tc.echo},
			}, &requests)
			defer server.Close()

			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatalf("parsing test server URL: %v", err)
			}
			tc.mutate(endpoint)
			client := testClient(endpoint.String())

			_, _, derr := client.DistillWithRecovery(
				context.Background(), "p", "text", 1,
			)
			if derr == nil {
				t.Fatal("scripted failure must surface an error")
			}
			for _, secret := range tc.secrets {
				if strings.Contains(derr.Error(), secret) {
					t.Fatalf("error leaks reflected credential %q: %v",
						secret, derr)
				}
			}
			if strings.Contains(derr.Error(), "denied") {
				t.Fatalf("error carries attacker-controlled body content "+
					"from a credentialed endpoint: %v", derr)
			}
		})
	}
}

// TestClientErrorDetailStripsControlCharacters pins that a kept response
// excerpt (credential-free endpoint) cannot smuggle terminal escapes or
// carriage returns into doctor stderr and CI logs.
func TestClientErrorDetailStripsControlCharacters(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{
			status:    http.StatusNotFound,
			errorBody: "{\"error\":\"\x1b]0;pwned\x07\rbad model\"}",
		},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 1,
	)
	if err == nil {
		t.Fatal("scripted failure must surface an error")
	}
	if !strings.Contains(err.Error(), "bad model") {
		t.Fatalf("error must keep the printable server detail: %v", err)
	}
	for _, banned := range []string{"\x1b", "\x07", "\r"} {
		if strings.Contains(err.Error(), banned) {
			t.Fatalf("error carries control byte %q: %q", banned, err.Error())
		}
	}
}

// TestClientBoundsUnknownFinishReasonDetail pins that a hostile 200 whose
// finish_reason approaches the transport limit cannot balloon the error —
// the manager persists error text into a failure row per session.
func TestClientBoundsUnknownFinishReasonDetail(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: strings.Repeat("x", 1<<20), content: `{"entries":[]}`},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 1,
	)
	if err == nil {
		t.Fatal("an unknown finish reason must surface an error")
	}
	if len(err.Error()) > 500 {
		t.Fatalf("error carries %d bytes of endpoint-controlled text, "+
			"want a bounded excerpt", len(err.Error()))
	}
}

// TestClientDeterministicStatusesAreEndpointScoped pins that statuses a
// same-configured endpoint answers identically for every request — wrong
// method or media type on the route (405, 415), unimplemented route (501)
// — fail fast without transient retries and classify as endpoint-scoped,
// so the manager aborts the pass instead of burning one doomed call per
// session.
func TestClientDeterministicStatusesAreEndpointScoped(t *testing.T) {
	for _, status := range []int{
		http.StatusMethodNotAllowed,
		http.StatusUnsupportedMediaType,
		http.StatusNotImplemented,
	} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			var requests []map[string]any
			server := newScriptedServer(t, []scriptedResponse{
				{status: status}, {status: status}, {status: status},
			}, &requests)
			defer server.Close()

			_, _, err := testClient(server.URL).DistillWithRecovery(
				context.Background(), "p", "text", 3,
			)
			if err == nil {
				t.Fatal("scripted failure must surface an error")
			}
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want 1: a deterministic status "+
					"must not consume transient retries", len(requests))
			}
			if !endpointScopedRejection(err) {
				t.Fatalf("error must classify as endpoint-scoped: %v", err)
			}
		})
	}
}

// TestClientWithholdsSuccessDiagnosticsForCredentialedEndpoints pins that
// HTTP 200 error paths get the same treatment as non-200 bodies: a server
// that knows the credential can reflect it through finish_reason, unknown
// keys, or any schema-violating field, and those errors reach persisted
// failure rows, scheduler logs, and doctor output.
func TestClientWithholdsSuccessDiagnosticsForCredentialedEndpoints(t *testing.T) {
	const secret = "sekret-value"
	cases := map[string]scriptedResponse{
		"finish reason": {
			finishReason: secret, content: `{"entries":[]}`,
		},
		"unknown key": {
			finishReason: "stop",
			content:      `{"entries":[],"` + secret + `":true}`,
		},
		"entry type": {
			finishReason: "stop",
			content: `{"entries":[{"type":"` + secret +
				`","title":"t","body":"b","entities":[]}]}`,
		},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			var requests []map[string]any
			server := newScriptedServer(
				t, []scriptedResponse{response}, &requests,
			)
			defer server.Close()

			client := testClient(server.URL + "?api_key=" + secret)
			_, _, err := client.DistillWithRecovery(
				context.Background(), "p", "text", 1,
			)
			if err == nil {
				t.Fatal("scripted violation must surface an error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error reflects the endpoint credential: %v", err)
			}
		})
	}
}

// TestClientWithholdsBodyForPathTokenEndpoints pins that path segments
// outside the known API-surface vocabulary count as credential material:
// webhook-style gateways authenticate through path tokens — long or short,
// possibly split across segments — and a body echoing the request URI
// would otherwise carry them into persisted errors and logs through the
// kept excerpt.
func TestClientWithholdsBodyForPathTokenEndpoints(t *testing.T) {
	cases := map[string]string{
		"long capability token": "cap-4bcdefgh1jklmn0pqrst",
		"short path secret":     "zq7-hook",
		"segmented secret":      "t0k/3n-p4rt",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			var requests []map[string]any
			server := newScriptedServer(t, []scriptedResponse{{
				status:    http.StatusForbidden,
				errorBody: `{"error":"denied for /` + token + `/v1"}`,
			}}, &requests)
			defer server.Close()

			client := testClient(server.URL + "/" + token + "/v1")
			_, _, err := client.DistillWithRecovery(
				context.Background(), "p", "text", 1,
			)
			if err == nil {
				t.Fatal("scripted failure must surface an error")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("error reflects the path token: %v", err)
			}
			if strings.Contains(err.Error(), "denied") {
				t.Fatalf("error carries attacker-controlled body content "+
					"from a credentialed endpoint: %v", err)
			}
		})
	}
}

// TestClientWithholdsMalformedRedirectDetail pins the transport-error
// leak path: a malformed redirect Location is parsed before CheckRedirect
// runs, and Go's parse error quotes the raw header — a server that knows
// the endpoint credential can reflect it there, bypassing the redirect
// and response-body redaction.
func TestClientWithholdsMalformedRedirectDetail(t *testing.T) {
	reflected := "reflected-cap-4bcdefgh1jklmn0pqrst"
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://evil.example/%zz"+reflected)
			w.WriteHeader(http.StatusFound)
		}))
	defer server.Close()

	client := testClient(server.URL + "/cap-4bcdefgh1jklmn0pqrst/v1")
	_, _, err := client.DistillWithRecovery(
		context.Background(), "p", "text", 1,
	)
	if err == nil {
		t.Fatal("malformed redirect must surface an error")
	}
	if strings.Contains(err.Error(), reflected) {
		t.Fatalf("error reflects the malformed Location header: %v", err)
	}
	if !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("credentialed transport-error detail must be withheld: %v",
			err)
	}
}

// TestClientBoundsTransportErrorDetail pins the credential-free side: the
// malformed-Location diagnostic stays — that is where the operator sees
// the broken proxy — control-stripped and length-bounded.
func TestClientBoundsTransportErrorDetail(t *testing.T) {
	long := strings.Repeat("A", 600)
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://example.com/%zz"+long)
			w.WriteHeader(http.StatusFound)
		}))
	defer server.Close()

	client := testClient(server.URL + "/v1")
	_, _, err := client.DistillWithRecovery(
		context.Background(), "p", "text", 1,
	)
	if err == nil {
		t.Fatal("malformed redirect must surface an error")
	}
	if !strings.Contains(err.Error(), "%zz") {
		t.Fatalf("credential-free transport diagnostics must be kept: %v", err)
	}
	if strings.Contains(err.Error(), long) {
		t.Fatalf("transport-error text must be length-bounded: %v", err)
	}
	if !strings.Contains(err.Error(), "…(truncated)") {
		t.Fatalf("transport-error text must note truncation: %v", err)
	}
}

// TestClientOmitsRedirectTargetForCredentialedEndpoints pins that a
// refused redirect's error names no target when the configured endpoint
// carries credential material: a redirect can put the credential in the
// target hostname, and RedactedEndpoint preserves hostnames.
func TestClientOmitsRedirectTargetForCredentialedEndpoints(t *testing.T) {
	token := "cap-4bcdefgh1jklmn0pqrst"
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://"+token+".evil.example/")
			w.WriteHeader(http.StatusFound)
		}))
	defer server.Close()

	client := testClient(server.URL + "/" + token + "/v1")
	_, _, err := client.DistillWithRecovery(
		context.Background(), "p", "text", 1,
	)
	if err == nil {
		t.Fatal("refused redirect must surface an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error reflects the redirect target hostname: %v", err)
	}
	if !strings.Contains(err.Error(), "do not follow redirects") {
		t.Fatalf("error must keep the redirect-refusal cause: %v", err)
	}
}

// TestClientBoundsRedirectTargetDetail pins the credential-free side: the
// redirect target stays in the error — it is the diagnostic — but a
// hostile Location cannot balloon persisted failure rows.
func TestClientBoundsRedirectTargetDetail(t *testing.T) {
	long := strings.Repeat("a", 300)
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://"+long+".example/")
			w.WriteHeader(http.StatusFound)
		}))
	defer server.Close()

	client := testClient(server.URL + "/v1")
	_, _, err := client.DistillWithRecovery(
		context.Background(), "p", "text", 1,
	)
	if err == nil {
		t.Fatal("refused redirect must surface an error")
	}
	if !strings.Contains(err.Error(), "refusing redirect to") {
		t.Fatalf("credential-free redirect diagnostics must be kept: %v", err)
	}
	if strings.Contains(err.Error(), long) {
		t.Fatalf("redirect target must be length-bounded: %v", err)
	}
	if !strings.Contains(err.Error(), "…(truncated)") {
		t.Fatalf("redirect target must note truncation: %v", err)
	}
}

// TestClientSanitizesBodyReadErrorDetail pins the body-read twin of the
// transport-error policy: a malformed chunked trailer line is echoed
// verbatim by Go's read error ('malformed MIME header: missing colon:
// "<raw line>"'), so read errors carry raw wire bytes into doctor
// output, scheduler logs, and persisted failure rows unless routed
// through the same sanitizer.
func TestClientSanitizesBodyReadErrorDetail(t *testing.T) {
	reflected := "reflected-cap-4bcdefgh1jklmn0pqrst"
	newTrailerServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = conn.Write([]byte(
					"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" +
						"3\r\nabc\r\n0\r\n" + reflected + " junk\r\n\r\n"))
			}))
	}

	t.Run("credentialed endpoint withholds", func(t *testing.T) {
		server := newTrailerServer()
		defer server.Close()
		client := testClient(server.URL + "/cap-4bcdefgh1jklmn0pqrst/v1")
		_, _, err := client.DistillWithRecovery(
			context.Background(), "p", "text", 1,
		)
		if err == nil {
			t.Fatal("malformed trailer must surface an error")
		}
		if strings.Contains(err.Error(), reflected) {
			t.Fatalf("error reflects the malformed trailer line: %v", err)
		}
		if !strings.Contains(err.Error(), "withheld") {
			t.Fatalf("credentialed read-error detail must be withheld: %v",
				err)
		}
	})

	t.Run("bare endpoint keeps bounded diagnostic", func(t *testing.T) {
		server := newTrailerServer()
		defer server.Close()
		client := testClient(server.URL + "/v1")
		_, _, err := client.DistillWithRecovery(
			context.Background(), "p", "text", 1,
		)
		if err == nil {
			t.Fatal("malformed trailer must surface an error")
		}
		if !strings.Contains(err.Error(), "malformed MIME header") {
			t.Fatalf("credential-free read diagnostics must be kept: %v", err)
		}
	})
}

// TestClientKeepsDiagnosticsForKnownAPIPaths pins the other side: the
// standard OpenAI-compatible path shapes are API surface, not secrets,
// and withholding their diagnostics would gut doctor output for every
// local inference server.
func TestClientKeepsDiagnosticsForKnownAPIPaths(t *testing.T) {
	for _, path := range []string{"/v1", "/api/v1", "/v1beta/openai"} {
		t.Run(path, func(t *testing.T) {
			var requests []map[string]any
			server := newScriptedServer(t, []scriptedResponse{{
				status:    http.StatusNotFound,
				errorBody: `{"error":"model not found"}`,
			}}, &requests)
			defer server.Close()

			client := testClient(server.URL + path)
			_, _, err := client.DistillWithRecovery(
				context.Background(), "p", "text", 1,
			)
			if err == nil {
				t.Fatal("scripted failure must surface an error")
			}
			if !strings.Contains(err.Error(), "model not found") {
				t.Fatalf("error must keep the server detail for a "+
					"vocabulary-only path: %v", err)
			}
		})
	}
}

// TestClientBoundsUnknownKeyDetail pins that an unknown JSON key — which a
// hostile 200 can grow toward the transport limit — reaches error text
// only as a bounded token.
func TestClientBoundsUnknownKeyDetail(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{{
		finishReason: "stop",
		content: `{"entries":[],"` + strings.Repeat("x", 1<<20) +
			`":true}`,
	}}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 1,
	)
	if err == nil {
		t.Fatal("an unknown key must surface an error")
	}
	if len(err.Error()) > 500 {
		t.Fatalf("error carries %d bytes of endpoint-controlled text, "+
			"want a bounded excerpt", len(err.Error()))
	}
}

// TestClientRedirectRefusalIsEndpointScoped pins that a refused redirect —
// deterministic for every request against the same configuration — fails
// on the first attempt and classifies as endpoint-scoped, so the manager
// aborts the pass instead of retrying three times per unit and marking
// every session failed.
func TestClientRedirectRefusalIsEndpointScoped(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere",
				http.StatusTemporaryRedirect)
		}))
	defer server.Close()

	client := testClient(server.URL)
	client.HTTPClient = &http.Client{CheckRedirect: RefuseRedirects}
	_, _, err := client.DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err == nil {
		t.Fatal("a refused redirect must surface an error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1: a deterministic redirect must "+
			"not consume transient retries", got)
	}
	if !endpointScopedRejection(err) {
		t.Fatalf("error must classify as endpoint-scoped: %v", err)
	}
}

// TestClientRequestEntityTooLargeSplits pins that HTTP 413 — the transport
// telling us the request body is too big — surfaces as ErrContextOverflow
// so the caller splits the unit, exactly like an in-band context-overflow
// 400. As a permanent failure the session would retry unchanged after
// every backoff, forever.
func TestClientRequestEntityTooLargeSplits(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{status: http.StatusRequestEntityTooLarge},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("err = %v, want ErrContextOverflow", err)
	}
}

// TestClientRejectsOversizedContent pins the local resource bounds: a
// configured or compromised endpoint can answer within the transport size
// limit yet carry tens of thousands of entries or multi-megabyte fields,
// and accepting them would balloon the archive and hold its write lock
// through the inserts. Violations are deterministic, so no retry.
func TestClientRejectsOversizedContent(t *testing.T) {
	makeEntries := func(count int, title, body string, entities []string) string {
		t.Helper()
		if entities == nil {
			entities = []string{}
		}
		entry := map[string]any{
			"type": "fact", "title": title, "body": body,
			"entities": entities,
		}
		list := make([]map[string]any, count)
		for i := range list {
			list[i] = entry
		}
		raw, err := json.Marshal(map[string]any{"entries": list})
		if err != nil {
			t.Fatalf("marshaling scripted entries: %v", err)
		}
		return string(raw)
	}
	long := func(n int) string { return strings.Repeat("a", n) }
	manyEntities := make([]string, maxEntryEntities+1)
	for i := range manyEntities {
		manyEntities[i] = "e"
	}
	cases := map[string]string{
		"too many entries": makeEntries(
			maxResponseEntries+1, "t", "b", nil),
		"oversized title": makeEntries(
			1, long(maxEntryTitleChars+1), "b", nil),
		"oversized body": makeEntries(
			1, "t", long(maxEntryBodyChars+1), nil),
		"too many entities": makeEntries(1, "t", "b", manyEntities),
		"oversized entity": makeEntries(
			1, "t", "b", []string{long(maxEntityChars + 1)}),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			var requests []map[string]any
			server := newScriptedServer(t, []scriptedResponse{
				{finishReason: "stop", content: content},
			}, &requests)
			defer server.Close()

			entries, _, err := testClient(server.URL).DistillWithRecovery(
				context.Background(), "p", "text", 3,
			)
			if err == nil {
				t.Fatalf("oversized content must be rejected, got %d entries",
					len(entries))
			}
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want 1 (limit violations are "+
					"deterministic)", len(requests))
			}
		})
	}
}

// TestEntrySchemaMirrorsResponseLimits pins that the limits enforced locally
// are also requested from the server: a compliant constrained-decoding
// endpoint then never produces a response the client refuses.
func TestEntrySchemaMirrorsResponseLimits(t *testing.T) {
	entriesSchema, ok := entrySchema["properties"].(map[string]any)["entries"].(map[string]any)
	if !ok {
		t.Fatal("entry schema has no entries property")
	}
	if got := entriesSchema["maxItems"]; got != maxResponseEntries {
		t.Fatalf("entries maxItems = %v, want %d", got, maxResponseEntries)
	}
	fields, ok := entriesSchema["items"].(map[string]any)["properties"].(map[string]any)
	if !ok {
		t.Fatal("entry schema has no item properties")
	}
	if got := fields["title"].(map[string]any)["maxLength"]; got != maxEntryTitleChars {
		t.Fatalf("title maxLength = %v, want %d", got, maxEntryTitleChars)
	}
	if got := fields["body"].(map[string]any)["maxLength"]; got != maxEntryBodyChars {
		t.Fatalf("body maxLength = %v, want %d", got, maxEntryBodyChars)
	}
	entities, ok := fields["entities"].(map[string]any)
	if !ok {
		t.Fatal("entry schema has no entities property")
	}
	if got := entities["maxItems"]; got != maxEntryEntities {
		t.Fatalf("entities maxItems = %v, want %d", got, maxEntryEntities)
	}
	if got := entities["items"].(map[string]any)["maxLength"]; got != maxEntityChars {
		t.Fatalf("entity maxLength = %v, want %d", got, maxEntityChars)
	}
}

func TestSplitFloorChars(t *testing.T) {
	if got := SplitFloorChars(50000); got != 2000 {
		t.Fatalf("SplitFloorChars(50000) = %d, want 2000", got)
	}
	if got := SplitFloorChars(800); got != 100 {
		t.Fatalf("SplitFloorChars(800) = %d, want 100", got)
	}
}

// TestClientEndpointWithQueryRoutesCorrectly pins URL construction for
// endpoints carrying query parameters (Azure-style ?api-version=...):
// string concatenation would land the route inside the query value and hit
// the bare endpoint path instead.
func TestClientEndpointWithQueryRoutesCorrectly(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotQuery = r.URL.Query().Get("api-version")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(completionBody(t, entriesJSON(t, "one"))))
		}))
	defer server.Close()

	client := testClient(server.URL + "/v1?api-version=2024-06-01")
	entries, _, err := client.DistillWithRecovery(
		context.Background(), "p", "text", 1,
	)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("request path = %q, want /v1/chat/completions", gotPath)
	}
	if gotQuery != "2024-06-01" {
		t.Fatalf("api-version = %q; the endpoint query must survive "+
			"route joining", gotQuery)
	}
}

// TestClientTransportErrorRedactsEndpoint pins that connection failures do
// not echo endpoint credentials: these errors land in doctor output and in
// stored progress rows, and endpoints may carry Basic-auth userinfo or API
// keys in query parameters. (Go's url.Error already masks the password but
// keeps the username and the query string.)
func TestClientTransportErrorRedactsEndpoint(t *testing.T) {
	client := testClient(
		"http://tester:hunter2@127.0.0.1:1/v1?api_key=sekret")
	_, _, err := client.DistillWithRecovery(
		context.Background(), "p", "text", 1,
	)
	if err == nil {
		t.Fatal("expected a connection error against a closed port")
	}
	for _, secret := range []string{"hunter2", "sekret", "tester"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("transport error leaks %q: %v", secret, err)
		}
	}
}
