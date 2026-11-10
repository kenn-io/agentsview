package chatgpt

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func sseDelta(t *testing.T, text string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": text})
	require.NoError(t, err)
	return "data: " + string(data) + "\n\n"
}

func TestResponsesSSEPartitionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		stream := sseDelta(t, text) + "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
		width := hegel.Draw(ht, hegel.Integers(1, len(stream)))
		got, err := readResponse(t.Context(), &partitionReader{data: []byte(stream), width: width})
		if strings.TrimSpace(text) == "" {
			require.Error(ht, err)
		} else {
			require.NoError(ht, err)
			assert.Equal(ht, text, got)
		}
	})
}

type partitionReader struct {
	data  []byte
	width int
}

func (r *partitionReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.width, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestResponsesSSEContract(t *testing.T) {
	for name, stream := range map[string]string{
		"eof":        sseDelta(t, "partial"),
		"done only":  sseDelta(t, "partial") + "data: [DONE]\n\n",
		"failed":     sseDelta(t, "partial") + "data: {\"type\":\"response.failed\",\"error\":{\"message\":\"private-echo\"}}\n\n",
		"incomplete": "data: {\"type\":\"response.incomplete\"}\n\n",
		"error":      "data: {\"type\":\"error\",\"message\":\"private-echo\"}\n\n",
		"invalid":    "data: not-json\n\n",
		"no text":    "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
		"bad delta":  "data: {\"type\":\"response.output_text.delta\",\"delta\":42}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readResponse(t.Context(), strings.NewReader(stream))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-echo")
		})
	}
	got, err := readResponse(t.Context(), strings.NewReader(": keepalive\r\ndata: {\"type\":\"response.output_text.delta\",\r\ndata: \"delta\":\"hello\"}\r\n\r\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\r\n\r\n"))
	require.NoError(t, err)
	assert.Equal(t, "hello", got)
	// A completed event can repeat the full response rather than being tiny.
	text := strings.Repeat("x", 2<<20)
	completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]string{"text": text}}}})
	got, err = readResponse(t.Context(), strings.NewReader(sseDelta(t, text[:len(text)/2])+sseDelta(t, text[len(text)/2:])+"data: "+string(completed)+"\n\n"))
	require.NoError(t, err)
	assert.Equal(t, text, got)
	oversized := sseDelta(t, text[:len(text)/2]) + sseDelta(t, text[len(text)/2:]+"x") +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	_, err = readResponse(t.Context(), strings.NewReader(oversized))
	require.ErrorContains(t, err, "ChatGPT response output too large")
}

func TestResponsesRequestAndRetry(t *testing.T) {
	old := token(t, "account", time.Now().Add(time.Hour))
	next := token(t, "new-account", time.Now().Add(time.Hour))
	var requests, refreshes atomic.Int32
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			assert.NoError(t, r.ParseForm())
			assert.Equal(t, "refresh", r.Form.Get("refresh_token"))
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"access_token": next, "expires_in": 3600}))
			return
		}
		requests.Add(1)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		assert.Equal(t, "agentsview", r.Header.Get("originator"))
		assert.Equal(t, "responses=experimental", r.Header.Get("OpenAI-Beta"))
		var body map[string]any
		assert.NoError(t, json.UnmarshalRead(r.Body, &body))
		assert.Equal(t, "model", body["model"])
		assert.Equal(t, false, body["store"])
		assert.Equal(t, true, body["stream"])
		assert.Contains(t, fmt.Sprint(body["input"]), "prompt")
		assert.NotContains(t, body, "max_tokens")
		assert.NotContains(t, body, "temperature")
		if r.Header.Get("Authorization") == "Bearer "+old {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, "private-echo")
			return
		}
		assert.Equal(t, "Bearer "+next, r.Header.Get("Authorization"))
		assert.Equal(t, "new-account", r.Header.Get("chatgpt-account-id"))
		fmt.Fprint(w, sseDelta(t, "result")+"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	})
	require.NoError(t, c.saveCredential(t.Context(), credential{AccessToken: old, RefreshToken: "refresh", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}))
	got, err := c.Complete(t.Context(), "model", "prompt")
	require.NoError(t, err)
	assert.Equal(t, "result", got)
	assert.EqualValues(t, 2, requests.Load())
	assert.EqualValues(t, 1, refreshes.Load())
	cr, err := c.loadCredential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "refresh", cr.RefreshToken)
	// A delayed 401 for the previous token must not rotate the current token.
	other := New(c.dataDir)
	other.authBase = c.authBase
	other.http = c.http
	cr, err = other.access(t.Context(), old)
	require.NoError(t, err)
	assert.Equal(t, next, cr.AccessToken)
	assert.EqualValues(t, 1, refreshes.Load())
}

func TestResponsesFailuresAreBoundedAndPrivate(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "https://example.invalid")
				w.WriteHeader(status)
				fmt.Fprint(w, "private-echo")
			})
			jwt := token(t, "account", time.Now().Add(time.Hour))
			require.NoError(t, c.saveCredential(t.Context(), credential{AccessToken: jwt, RefreshToken: "private-refresh", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}))
			_, err := c.Complete(t.Context(), "model", "private-prompt")
			require.Error(t, err)
			for _, secret := range []string{"private-echo", "private-refresh", "private-prompt", jwt} {
				assert.NotContains(t, err.Error(), secret)
			}
			assert.LessOrEqual(t, calls.Load(), int32(2))
		})
	}
}

func TestResponsesDoNotFollowRedirects(t *testing.T) {
	var sourceCalls, targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sourceCalls.Add(1)
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer source.Close()

	c := New(t.TempDir())
	c.responsesURL = source.URL
	jwt := token(t, "account", time.Now().Add(time.Hour))
	require.NoError(t, c.saveCredential(t.Context(), credential{
		AccessToken: jwt, RefreshToken: "private-refresh", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour),
	}))
	_, err := c.Complete(t.Context(), "model", "private-prompt")
	require.ErrorContains(t, err, "HTTP 301")
	assert.EqualValues(t, 1, sourceCalls.Load())
	assert.Zero(t, targetCalls.Load())
}

func TestRefreshCoalescesAcrossClients(t *testing.T) {
	var refreshes atomic.Int32
	jwt := token(t, "account", time.Now().Add(time.Hour))
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		refreshes.Add(1)
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"access_token": jwt, "expires_in": 3600}))
	})
	require.NoError(t, c.saveCredential(t.Context(), credential{AccessToken: "expired", RefreshToken: "refresh", AccountID: "account", ExpiresAt: time.Now().Add(-time.Hour)}))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			other := New(c.dataDir)
			other.http = c.http
			other.authBase = c.authBase
			cr, err := other.access(t.Context(), "expired")
			if assert.NoError(t, err) {
				assert.Equal(t, jwt, cr.AccessToken)
			}
		})
	}
	wg.Wait()
	assert.EqualValues(t, 1, refreshes.Load())
}

func TestResponsesCancellation(t *testing.T) {
	started := make(chan struct{})
	c := fixtureClient(t, func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	})
	require.NoError(t, c.saveCredential(t.Context(), credential{AccessToken: token(t, "account", time.Now().Add(time.Hour)), RefreshToken: "refresh", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := c.Complete(ctx, "model", "prompt"); result <- err }()
	<-started
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
}

func TestResponsesMalformedCompletionRegression(t *testing.T) {
	for _, terminal := range []string{`{"type":"response.completed"}`, `{"type":"response.completed","response":null}`, `{"type":"response.completed","response":{}}`} {
		_, err := readResponse(t.Context(), strings.NewReader(sseDelta(t, "partial")+"data: "+terminal+"\n\n"))
		require.Error(t, err)
	}
}

func TestResponsesNonterminalEventLimitRegression(t *testing.T) {
	event := `{"type":"response.output_text.delta","delta":"x","padding":"` + strings.Repeat("x", (2<<20)) + `"}`
	_, err := readResponse(t.Context(), strings.NewReader("data: "+event+"\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
	require.Error(t, err)
}

func TestResponsesRawCRLFStreamLimitRegression(t *testing.T) {
	prefix := strings.ReplaceAll(sseDelta(t, "x"), "\n", "\r\n")
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\r\n\r\n"
	for _, size := range []int{16 << 20, (16 << 20) + 1} {
		paddingSize := size - len(prefix) - len(terminal)
		comment := ":" + strings.Repeat("x", 1021) + "\r\n"
		padding := strings.Repeat(comment, paddingSize/len(comment))
		remaining := paddingSize - len(padding)
		if remaining > 0 {
			padding += ":" + strings.Repeat("x", remaining-1)
		}
		// A nonempty final comment needs a newline; move two bytes from its body.
		if remaining >= 3 {
			padding = padding[:len(padding)-2] + "\r\n"
		}
		stream := prefix + padding + terminal
		require.Len(t, stream, size)
		got, err := readResponse(t.Context(), strings.NewReader(stream))
		if size > 16<<20 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			assert.Equal(t, "x", got)
		}
	}
}

func TestResponsesSecondUnauthorizedStopsRetry(t *testing.T) {
	var requests, refreshes atomic.Int32
	jwt := token(t, "new-account", time.Now().Add(time.Hour))
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"access_token": jwt, "expires_in": 3600}))
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	require.NoError(t, c.saveCredential(t.Context(), credential{AccessToken: "old", RefreshToken: "refresh", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}))
	_, err := c.Complete(t.Context(), "model", "prompt")
	require.ErrorContains(t, err, "HTTP 401")
	assert.EqualValues(t, 2, requests.Load())
	assert.EqualValues(t, 1, refreshes.Load())
}
