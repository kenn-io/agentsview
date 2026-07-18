package extract

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
}

func newScriptedServer(
	t *testing.T, responses []scriptedResponse, requests *[]map[string]any,
) *httptest.Server {
	t.Helper()
	var index atomic.Int64
	return httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/chat/completions" {
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
			body := map[string]any{
				"choices": []map[string]any{{
					"finish_reason": resp.finishReason,
					"message": map[string]any{
						"role":    "assistant",
						"content": resp.content,
					},
				}},
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
			// The compact floor covers the short unit texts these tests
			// send, so truncation exercises the compact retry unless a
			// test says otherwise.
			CompactFloorChars: 64,
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

func TestClientCompactFloorCountsRunesNotBytes(t *testing.T) {
	// Segmentation budgets count code points, so the compact floor must
	// too: 40 three-byte runes are 120 bytes but still one unsplittable
	// 40-rune unit under the 64-rune floor, and must get the compact retry.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "length", content: ""},
		{finishReason: "stop", content: entriesJSON(t, "compact")},
	}, &requests)
	defer server.Close()

	text := strings.Repeat("日", 40)
	entries, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", text, 3,
	)
	if err != nil {
		t.Fatalf("DistillWithRecovery: %v", err)
	}
	if len(entries) != 1 || len(requests) != 2 {
		t.Fatalf("entries=%d requests=%d, want compact retry to run",
			len(entries), len(requests))
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

func TestClientCompactRetryOnTruncation(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "length", content: ""},
		{finishReason: "stop", content: entriesJSON(t, "compact")},
	}, &requests)
	defer server.Close()

	entries, usage, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "unit text", 3,
	)
	if err != nil {
		t.Fatalf("DistillWithRecovery: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2 (truncation triggers one compact retry)", len(requests))
	}
	second := requests[1]["messages"].([]any)[1].(map[string]any)
	if !strings.Contains(second["content"].(string), "output budget is tight") {
		t.Fatal("compact retry must append the compact instruction")
	}
	if usage.PromptTokens != 14 || usage.CompletionTokens != 6 {
		t.Fatalf("usage = %+v, want both attempts accounted (14/6)", usage)
	}
}

func TestClientPersistentTruncationIsTyped(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "length", content: ""},
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
		t.Fatalf("requests = %d, want 2 (below the floor: one compact retry)",
			len(requests))
	}
	if usage.PromptTokens != 14 || usage.CompletionTokens != 6 {
		t.Fatalf("usage = %+v, want truncated attempts accounted (14/6)",
			usage)
	}
}

func TestClientTruncationAboveFloorSplitsInsteadOfCompacting(t *testing.T) {
	// The compact retry caps the entry count, so on a unit large enough to
	// split it would silently drop entries; truncation must surface the
	// typed split signal without a compact attempt.
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "length", content: ""},
	}, &requests)
	defer server.Close()

	longText := strings.Repeat("dense unit text ", 32)
	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", longText, 3,
	)
	if !errors.Is(err, ErrPersistentTruncation) {
		t.Fatalf("err = %v, want ErrPersistentTruncation", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1 (no compact retry above the floor)",
			len(requests))
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

func TestSplitFloorChars(t *testing.T) {
	if got := SplitFloorChars(50000); got != 2000 {
		t.Fatalf("SplitFloorChars(50000) = %d, want 2000", got)
	}
	if got := SplitFloorChars(800); got != 100 {
		t.Fatalf("SplitFloorChars(800) = %d, want 100", got)
	}
}
