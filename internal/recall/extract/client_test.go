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
		// The compact floor covers the short unit texts these tests send, so
		// truncation exercises the compact retry unless a test says otherwise.
		CompactFloorChars: 64,
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

	entries, _, err := testClient(server.URL).DistillWithRecovery(
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
}

func TestClientPersistentTruncationIsTyped(t *testing.T) {
	var requests []map[string]any
	server := newScriptedServer(t, []scriptedResponse{
		{finishReason: "length", content: ""},
		{finishReason: "length", content: ""},
	}, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if !errors.Is(err, ErrPersistentTruncation) {
		t.Fatalf("err = %v, want ErrPersistentTruncation", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2 (below the floor: one compact retry)",
			len(requests))
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
		{finishReason: "stop", content: entriesJSON(t, "ok")},
	}, &requests)
	defer server.Close()

	entries, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestClientEmptyContentIsError(t *testing.T) {
	// A model that burns its budget on hidden reasoning returns empty
	// content with finish_reason stop; that must surface as an error, not
	// as zero entries.
	var requests []map[string]any
	responses := make([]scriptedResponse, 3)
	for i := range responses {
		responses[i] = scriptedResponse{finishReason: "stop", content: ""}
	}
	server := newScriptedServer(t, responses, &requests)
	defer server.Close()

	_, _, err := testClient(server.URL).DistillWithRecovery(
		context.Background(), "p", "text", 3,
	)
	if err == nil {
		t.Fatal("empty content must be an error")
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
