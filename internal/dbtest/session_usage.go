package dbtest

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// Session IDs written by SeedSessionUsagePins.
const (
	UsagePinEmptyID    = "codex:usage-pin-empty"
	UsagePinMessagesID = "usage-pin-messages"
	UsagePinEventsID   = "codex:usage-pin-events"
	UsagePinGrowingID  = "codex:usage-pin-growing"
)

// usagePinModel is priced at $3 per million input tokens and $15 per
// million output tokens.
const usagePinModel = "usage-pin-model"

func usagePinSession(id, agent, startedAt string, messages int) db.Session {
	return db.Session{
		ID: id, Project: "usage-pins", Machine: "local", Agent: agent,
		FirstMessage: Ptr("hello"), StartedAt: Ptr(startedAt), EndedAt: Ptr(startedAt),
		MessageCount: messages, UserMessageCount: 1,
	}
}

func usagePinUserMessage(id, ts string) db.Message {
	return db.Message{SessionID: id, Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5, Timestamp: ts}
}

func usagePinWrite(t *testing.T, d *db.DB, writes ...db.SessionBatchWrite) {
	t.Helper()
	for i := range writes {
		writes[i].ReplaceMessages = true
		writes[i].DataVersion = 1
		writes[i].Signals = db.SessionSignalUpdate{
			QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
		}
	}
	_, err := d.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err, "writing session usage pins")
}

// SeedSessionUsagePins writes the sessions AssertSessionUsagePins reads: one
// with no usage (a single user message and no token data), one whose usage
// comes from message token counts, one whose usage comes only from usage
// events, and one with no usage yet that GrowSessionUsagePin adds usage to.
func SeedSessionUsagePins(t *testing.T, d *db.DB) {
	t.Helper()
	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  usagePinModel,
		InputPerMTok:  money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}}))
	const day = "2026-03-01T10:00:00Z"
	messages := usagePinSession(UsagePinMessagesID, "claude", day, 2)
	messages.TotalOutputTokens, messages.HasTotalOutputTokens = 200, true
	messages.PeakContextTokens, messages.HasPeakContextTokens = 1000, true
	usagePinWrite(t, d,
		db.SessionBatchWrite{
			Session:  usagePinSession(UsagePinEmptyID, "codex", day, 1),
			Messages: []db.Message{usagePinUserMessage(UsagePinEmptyID, day)},
		},
		db.SessionBatchWrite{
			Session: messages,
			Messages: []db.Message{
				usagePinUserMessage(UsagePinMessagesID, day),
				{
					SessionID: UsagePinMessagesID, Ordinal: 1, Role: "assistant",
					Content: "answer", ContentLength: 6, Timestamp: "2026-03-01T10:00:05Z",
					Model: usagePinModel, TokenUsage: []byte(`{"input_tokens":1000,"output_tokens":200}`),
					ContextTokens: 1000, OutputTokens: 200, HasContextTokens: true, HasOutputTokens: true,
				},
			},
		},
		db.SessionBatchWrite{
			Session:  usagePinSession(UsagePinEventsID, "codex", day, 1),
			Messages: []db.Message{usagePinUserMessage(UsagePinEventsID, day)},
			UsageEvents: []db.UsageEvent{{
				Source: "hermes", Model: usagePinModel, InputTokens: 2000, OutputTokens: 100,
				OccurredAt: "2026-03-01T10:00:07Z", DedupKey: "usage-pin-events-1",
			}},
		},
		db.SessionBatchWrite{
			Session:  usagePinSession(UsagePinGrowingID, "codex", day, 1),
			Messages: []db.Message{usagePinUserMessage(UsagePinGrowingID, day)},
		},
	)
}

// GrowSessionUsagePin adds usage to the session SeedSessionUsagePins left
// without usage, as a session still being written receives it: a usage
// event, with the session row still carrying no token totals.
func GrowSessionUsagePin(t *testing.T, d *db.DB) {
	t.Helper()
	const day = "2026-03-01T10:00:00Z"
	usagePinWrite(t, d, db.SessionBatchWrite{
		Session:  usagePinSession(UsagePinGrowingID, "codex", day, 1),
		Messages: []db.Message{usagePinUserMessage(UsagePinGrowingID, day)},
		UsageEvents: []db.UsageEvent{{
			Source: "hermes", Model: usagePinModel, InputTokens: 500, OutputTokens: 50,
			OccurredAt: "2026-03-01T10:00:09Z", DedupKey: "usage-pin-growing-1",
		}},
	})
}

const usagePinNoUsageJSON = `{"session_id":%q,"agent":"codex","project":"usage-pins",
	"total_output_tokens":0,"peak_context_tokens":0,"has_token_data":false,
	"cost":{"microdollars":0},"has_cost":false,"models":[],"breakdown_count":0,"breakdown":[]}`

// AssertSessionUsagePins checks every session SeedSessionUsagePins wrote,
// with and without the breakdown, against the responses the session usage
// endpoint returns for them.
func AssertSessionUsagePins(t *testing.T, store db.Store) {
	t.Helper()
	for _, tc := range []struct {
		id, summary, breakdown string
	}{
		{
			id:      UsagePinEmptyID,
			summary: fmt.Sprintf(usagePinNoUsageJSON, UsagePinEmptyID),
		},
		{
			id:      UsagePinGrowingID,
			summary: fmt.Sprintf(usagePinNoUsageJSON, UsagePinGrowingID),
		},
		{
			id: UsagePinMessagesID,
			summary: `{"session_id":"usage-pin-messages","agent":"claude","project":"usage-pins",
				"total_output_tokens":200,"peak_context_tokens":1000,"has_token_data":true,
				"cost":{"microdollars":6000},"has_cost":true,"cost_usd":0.006,"cost_source":"computed",
				"models":["usage-pin-model"],"breakdown_count":1,"breakdown":[]}`,
			breakdown: `[{"ordinal":1,"message_ordinal":1,"source":"message","label":"Prompt 2",
				"timestamp":"2026-03-01T10:00:05Z","model":"usage-pin-model",
				"input_tokens":1000,"output_tokens":200,"cache_creation_input_tokens":0,
				"cache_read_input_tokens":0,"cost":{"microdollars":6000},"has_cost":true}]`,
		},
		{
			id: UsagePinEventsID,
			summary: `{"session_id":"codex:usage-pin-events","agent":"codex","project":"usage-pins",
				"total_output_tokens":0,"peak_context_tokens":0,"has_token_data":false,
				"cost":{"microdollars":7500},"has_cost":true,"cost_usd":0.0075,"cost_source":"computed",
				"models":["usage-pin-model"],"breakdown_count":1,"breakdown":[]}`,
			breakdown: `[{"ordinal":1,"source":"hermes","label":"hermes",
				"timestamp":"2026-03-01T10:00:07Z","model":"usage-pin-model",
				"input_tokens":2000,"output_tokens":100,"cache_creation_input_tokens":0,
				"cache_read_input_tokens":0,"cost":{"microdollars":7500},"has_cost":true}]`,
		},
	} {
		assertSessionUsagePin(t, store, tc.id, tc.summary, tc.breakdown)
	}
}

// AssertGrownSessionUsagePin checks the session GrowSessionUsagePin added
// usage to.
func AssertGrownSessionUsagePin(t *testing.T, store db.Store) {
	t.Helper()
	assertSessionUsagePin(t, store, UsagePinGrowingID,
		`{"session_id":"codex:usage-pin-growing","agent":"codex","project":"usage-pins",
			"total_output_tokens":0,"peak_context_tokens":0,"has_token_data":false,
			"cost":{"microdollars":2250},"has_cost":true,"cost_usd":0.00225,"cost_source":"computed",
			"models":["usage-pin-model"],"breakdown_count":1,"breakdown":[]}`,
		`[{"ordinal":1,"source":"hermes","label":"hermes",
			"timestamp":"2026-03-01T10:00:09Z","model":"usage-pin-model",
			"input_tokens":500,"output_tokens":50,"cache_creation_input_tokens":0,
			"cache_read_input_tokens":0,"cost":{"microdollars":2250},"has_cost":true}]`)
}

// assertSessionUsagePin reads the session without and with the breakdown.
// The summary is the response without the breakdown; the response with it
// differs only in its breakdown entries, which are empty when breakdown is.
func assertSessionUsagePin(t *testing.T, store db.Store, id, summary, breakdown string) {
	t.Helper()
	if breakdown == "" {
		breakdown = "[]"
	}
	require.Contains(t, summary, `"breakdown":[]`, "summary of %s", id)
	for _, includeBreakdown := range []bool{false, true} {
		got, err := store.GetSessionUsage(t.Context(), id, includeBreakdown)
		require.NoError(t, err, "GetSessionUsage %s breakdown=%v", id, includeBreakdown)
		require.NotNil(t, got, "GetSessionUsage %s breakdown=%v", id, includeBreakdown)
		assert.Nil(t, got.PlanUsage, "plan usage of %s", id)
		body, err := json.Marshal(got)
		require.NoError(t, err)
		want := summary
		if includeBreakdown {
			want = strings.Replace(summary, `"breakdown":[]`, `"breakdown":`+breakdown, 1)
		}
		assert.JSONEq(t, want, string(body), "GetSessionUsage %s breakdown=%v", id, includeBreakdown)
	}
}
