package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	maxOutputBytes = 2 << 20
	// Completed events may repeat the full output with JSON escaping/metadata.
	maxEventBytes  = 4 << 20
	maxStreamBytes = 16 << 20
)

// Complete returns text only after a completed Responses stream. Partial output
// is never accepted as an insight. A rejected token permits one refresh/retry.
func (c *Client) Complete(ctx context.Context, model, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("ChatGPT model is required")
	}
	cr, err := c.access(ctx, "")
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"model": model, "instructions": "Generate the requested insight from the supplied session data.",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": prompt}}}},
		"store": false, "stream": true,
	})
	if err != nil {
		return "", errors.New("cannot encode ChatGPT insight request")
	}
	for attempt := range 2 {
		text, status, err := c.completeRequest(ctx, cr, body)
		if err != nil {
			return "", err
		}
		if status != 401 || attempt == 1 {
			if status < 200 || status >= 300 {
				return "", fmt.Errorf("ChatGPT insight request returned HTTP %d", status)
			}
			return text, nil
		}
		cr, err = c.access(ctx, cr.AccessToken)
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("ChatGPT insight request failed")
}

func (c *Client) completeRequest(ctx context.Context, cr credential, body []byte) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.responsesURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, errors.New("cannot create ChatGPT insight request")
	}
	req.Header.Set("Authorization", "Bearer "+cr.AccessToken)
	req.Header.Set("chatgpt-account-id", cr.AccountID)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "agentsview")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		return "", 0, errors.New("ChatGPT insight request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", resp.StatusCode, nil
	}
	text, err := readResponse(ctx, resp.Body)
	return text, resp.StatusCode, err
}

func readResponse(ctx context.Context, r io.Reader) (string, error) {
	scanner := bufio.NewScanner(io.LimitReader(r, maxStreamBytes+1))
	scanner.Buffer(make([]byte, 4096), maxEventBytes+1)
	// Preserve line endings until the wire budget is counted. ScanLines drops
	// CR bytes and would let a CRLF stream exceed its limit.
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i+1], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var output strings.Builder
	var event strings.Builder
	total := 0
	consume := func() (bool, error) {
		if event.Len() == 0 {
			return false, nil
		}
		data := strings.TrimSuffix(event.String(), "\n")
		event.Reset()
		if strings.TrimSpace(data) == "[DONE]" {
			return false, errors.New("ChatGPT stream ended before completion")
		}
		var e struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response struct {
				Status string `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(data), &e) != nil {
			return false, errors.New("invalid ChatGPT response event")
		}
		if e.Type != "response.completed" && len(data) > maxOutputBytes {
			return false, errors.New("ChatGPT response event too large")
		}
		switch e.Type {
		case "response.output_text.delta":
			if len(e.Delta) > maxOutputBytes-output.Len() {
				return false, errors.New("ChatGPT response output too large")
			}
			output.WriteString(e.Delta)
		case "response.completed":
			if e.Response.Status != "completed" {
				return false, errors.New("ChatGPT response did not complete")
			}
			if strings.TrimSpace(output.String()) == "" {
				return false, errors.New("ChatGPT response content is empty")
			}
			return true, nil
		case "response.failed", "response.incomplete", "error":
			return false, errors.New("ChatGPT response failed or was incomplete")
		}
		return false, nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line := scanner.Text()
		total += len(line)
		if total > maxStreamBytes {
			return "", errors.New("ChatGPT response stream too large")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			done, err := consume()
			if err != nil {
				return "", err
			}
			if done {
				return output.String(), nil
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			value := after
			value = strings.TrimPrefix(value, " ")
			if len(value)+1 > maxEventBytes-event.Len() {
				return "", errors.New("ChatGPT response event too large")
			}
			event.WriteString(value)
			event.WriteByte('\n')
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if scanner.Err() != nil {
		return "", errors.New("invalid or oversized ChatGPT response stream")
	}
	return "", errors.New("ChatGPT stream ended before completion")
}
