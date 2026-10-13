package codexcloudtest

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// UsageRequestGroup is one group of a plan usage request.
type UsageRequestGroup struct {
	ThreadID    string   `json:"thread_id"`
	CreatedAt   string   `json:"created_at"`
	Descendants []string `json:"descendant_thread_ids"`
}

// UsageRequest is one plan usage request the fake received.
type UsageRequest struct {
	Header  http.Header
	Threads []UsageRequestGroup
}

// UsageHost is a fake of OpenAI's plan usage report (thread_usage/query_v2)
// on a loopback HTTP server. It answers each requested root with its
// scripted row, and leaves out roots with none.
type UsageHost struct {
	server *httptest.Server

	mu       sync.Mutex
	rows     map[string]any
	dataAsOf time.Time
	refuse   *refusal
	requests []UsageRequest
}

// NewUsageHost starts a plan usage fake. It stops when the test ends.
func NewUsageHost(t *testing.T, dataAsOf time.Time) *UsageHost {
	t.Helper()
	u := &UsageHost{rows: map[string]any{}, dataAsOf: dataAsOf}
	u.server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.server.Close)
	return u
}

// URL is the fake's endpoint.
func (u *UsageHost) URL() *url.URL {
	parsed, err := url.Parse(u.server.URL + "/backend-api/wham/usage/thread_usage/query_v2")
	if err != nil {
		panic(err)
	}
	return parsed
}

// SetRow scripts the row returned for threadID; nil leaves it out.
func (u *UsageHost) SetRow(threadID string, row any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if row == nil {
		delete(u.rows, threadID)
		return
	}
	u.rows[threadID] = row
}

// SetDataAsOf changes the reply's data_as_of.
func (u *UsageHost) SetDataAsOf(t time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.dataAsOf = t
}

// Refuse makes later requests answer status with header and body.
func (u *UsageHost) Refuse(status int, header http.Header, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refuse = &refusal{status: status, header: header, body: body}
}

// Accept undoes Refuse.
func (u *UsageHost) Accept() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refuse = nil
}

// Requests returns every request received so far, in order.
func (u *UsageHost) Requests() []UsageRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]UsageRequest(nil), u.requests...)
}

// Row builds a plan usage row in the server's shape. credits is a decimal
// string or nil.
func Row(threadID, status string, weekly *float64, credits *string) map[string]any {
	return map[string]any{
		"thread_id": threadID, "data_status": status, "usage_source": "included_plan",
		"weekly_limit_percent": weekly, "five_hour_limit_percent": nil, "balance_usage_credits": credits,
		"groups": []any{map[string]any{
			"model": "gpt-a", "reasoning_effort": "high", "speed": "standard", "product_experience": "codex",
			"weekly_limit_percent": weekly, "five_hour_limit_percent": nil, "balance_usage_credits": credits,
		}},
	}
}

func (u *UsageHost) serve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Threads []UsageRequestGroup `json:"threads"`
	}
	if r.Method != http.MethodPost || json.UnmarshalRead(r.Body, &body) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	u.requests = append(u.requests, UsageRequest{Header: r.Header.Clone(), Threads: body.Threads})
	refuse, asOf := u.refuse, u.dataAsOf
	rows := []any{}
	for _, g := range body.Threads {
		if row, ok := u.rows[g.ThreadID]; ok {
			rows = append(rows, row)
		}
	}
	u.mu.Unlock()
	if refuse != nil {
		maps.Copy(w.Header(), refuse.header)
		w.WriteHeader(refuse.status)
		_, _ = w.Write([]byte(refuse.body))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, map[string]any{
		"data_as_of": asOf.UTC().Format(time.RFC3339), "comparison_plan": nil, "threads": rows,
	})
}
