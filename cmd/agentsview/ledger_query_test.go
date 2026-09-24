package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/ledger"
)

func TestLedgerQueryCommandLocal(t *testing.T) {
	dataDir := testDataDir(t)
	writeLedgerTestConfig(t, dataDir, "[ledger]\nenabled = true\nsource = \"host-a\"\n")
	_, err := runLedger(t, "append", "--class", "state-change", "--subsystem", "hook-a", "--summary", "first")
	require.NoError(t, err)
	_, err = runLedger(t, "append", "--class", "health", "--subsystem", "other", "--summary", "second")
	require.NoError(t, err)

	out, err := runLedger(t, "query", "--subsystem", "hook-*")
	require.NoError(t, err)
	assert.Contains(t, out, "Found 1 event(s) since 7d\nSubsystem filter: [\"hook-*\"]\n\nZone: default (1 events)\n")
	assert.Contains(t, out, "statechange    hook-a")

	out, err = runLedger(t, "query", "--format", "json")
	require.NoError(t, err)
	var flat []struct {
		Event map[string]any `json:"event"`
		Zone  string         `json:"zone"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &flat))
	require.Len(t, flat, 2)
	assert.Equal(t, "second", flat[0].Event["payload"].(map[string]any)["summary"], "newest first")

	out, err = runLedger(t, "query", "--class", "approval")
	require.NoError(t, err)
	assert.Equal(t, "No events found since 7d.\n", out)
}

func TestLedgerQueryCommandErrors(t *testing.T) {
	dataDir := testDataDir(t)
	writeLedgerTestConfig(t, dataDir, "[ledger]\nenabled = true\n")
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"query", "--since", "2026-02-30"}, "invalid date: 2026-02-30 (expected Nh, Nd, Nw, or YYYY-MM-DD)"},
		{[]string{"query", "--class", "review"}, "unknown event class 'review'"},
		{[]string{"query", "--zone", "ops"}, `zone "ops" is not configured`},
	} {
		t.Run(tt.args[len(tt.args)-1], func(t *testing.T) {
			_, err := runLedger(t, tt.args...)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestRequestLedgerQueryRebuildsEventsFromSerde(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	w := ledger.NewWriter(d, "default", "host-a", nil)
	body, err := decodeLedgerPayload(`{"subsystem":"api","summary":"exact","n":18446744073709551615}`)
	require.NoError(t, err)
	seg, err := w.Append(t.Context(), []ledger.Event{{
		EventClass: ledger.ClassStateChange, PayloadTier: ledger.TierStructured,
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC), Payload: body,
	}})
	require.NoError(t, err)
	serde, err := seg.Events[0].MarshalSerde()
	require.NoError(t, err)

	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, `{"since":"7d","results":[{"zone":"default","events":[{"serde":`+
			jsonString(string(serde))+`}]}]}`)
	}))
	defer ts.Close()

	results, err := requestLedgerQuery(t.Context(), transport{Mode: transportHTTP, URL: ts.URL}, "",
		ledgerQueryRequest{Since: "7d", Subsystems: []string{"api"}, Limit: 100})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "subsystem=api")
	local, err := queryLedgerLocal(t.Context(), config.Config{}, d,
		ledgerQueryRequest{Since: "7d", Subsystems: []string{"api"}, Limit: 100}, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	remote, err := ledger.FormatJSON(results)
	require.NoError(t, err)
	direct, err := ledger.FormatJSON(local)
	require.NoError(t, err)
	assert.Equal(t, string(direct), string(remote), "the daemon path prints what a local query prints")
}

func TestRequestLedgerAppendPreservesPayloadNumbers(t *testing.T) {
	payload, err := decodeLedgerPayload(`{"n":18446744073709551615,"f":1e+16}`)
	require.NoError(t, err)
	var requestBody string
	var readErr error
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			readErr = err
			http.Error(w, "read request body", http.StatusBadRequest)
			return
		}
		requestBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"zone":"default","source":"host-a","source_seq":1,"checksum":1,"event_ids":["00000000-0000-0000-0000-000000000001"]}`)
	}))
	defer ts.Close()
	result, err := requestLedgerAppend(t.Context(), transport{Mode: transportHTTP, URL: ts.URL}, "", "default", ledger.Event{
		EventClass: ledger.ClassStateChange, PayloadTier: ledger.TierStructured, Payload: payload,
	})
	require.NoError(t, err)
	require.NoError(t, readErr)
	assert.Equal(t, uint64(1), result.SourceSeq)
	assert.Contains(t, requestBody, `"n":18446744073709551615`)
	assert.Contains(t, requestBody, `"f":1e+16`)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
