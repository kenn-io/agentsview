package cursorusage

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

func TestListDailyUsageUsersPages(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/teams/daily-usage-data", r.URL.Path)
		var q struct {
			Start    int64 `json:"startDate"`
			End      int64 `json:"endDate"`
			Page     int   `json:"page"`
			PageSize int   `json:"pageSize"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &q)) {
			return
		}
		assert.Equal(t, 1000, q.PageSize)
		calls++
		switch calls {
		case 1:
			assert.Equal(t, start.UnixMilli(), q.Start)
			assert.Equal(t, 1, q.Page)
			_, _ = w.Write([]byte(`{"data":[{"userId":12,"isActive":false}],"pagination":{"hasNextPage":true}}`))
		case 2:
			assert.Equal(t, 2, q.Page)
			_, _ = w.Write([]byte(`{"data":[{"userId":34}],"pagination":{"hasNextPage":false}}`))
		default:
			assert.Fail(t, "unexpected roster request", "request %d", calls)
		}
	}))
	defer srv.Close()
	c := NewClientWithBaseURL(srv.URL, "key")
	c.DailyRequestInterval = 0
	users, err := c.ListDailyUsageUsers(t.Context(), start, start.Add(30*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, []string{"12", "34"}, users)
	assert.Equal(t, 2, calls)
}

func TestListDailyUsageUsersPacesSeparateWindows(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":[],"pagination":{"hasNextPage":false}}`))
	}))
	defer server.Close()
	client := NewClientWithBaseURL(server.URL, "key")
	client.DailyRequestInterval = time.Hour
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := client.ListDailyUsageUsers(t.Context(), start, start.Add(24*time.Hour))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond) //nolint:kennlint // deadlinetest: cancellation expires while the request waits for its slot.
	defer cancel()
	_, err = client.ListDailyUsageUsers(ctx, start.AddDate(20, 0, 0), start.AddDate(20, 0, 1))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, calls)
}

func TestFetchAllUsageEvents(t *testing.T) {
	fixturePath := filepath.Join("testdata", "admin_usage_page.json")
	fixture, err := os.ReadFile(fixturePath)
	require.NoError(t, err)

	var envelope usageEventsEnvelope
	require.NoError(t, json.Unmarshal(fixture, &envelope))
	require.Len(t, envelope.Display, 2)

	page1 := usageEventsEnvelope{
		TotalCount: envelope.TotalCount,
		Display:    envelope.Display[:1],
	}
	page2 := usageEventsEnvelope{
		TotalCount: envelope.TotalCount,
		Display:    envelope.Display[1:],
	}

	type requestBody struct {
		StartDate int64  `json:"startDate"`
		EndDate   int64  `json:"endDate"`
		Page      int    `json:"page"`
		PageSize  int    `json:"pageSize"`
		Email     string `json:"email"`
		UserID    int64  `json:"userId"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, http.MethodPost, r.Method) {
			return
		}
		if !assert.Equal(t, "/teams/filtered-usage-events", r.URL.Path) {
			return
		}

		user, pass, ok := r.BasicAuth()
		if !assert.True(t, ok) {
			return
		}
		assert.Equal(t, "cursor-key", user)
		assert.Empty(t, pass)

		var body requestBody
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		assert.Equal(t, 1, body.PageSize)
		assert.Equal(t, "member@example.com", body.Email)
		assert.Equal(t, int64(152683922), body.UserID)
		assert.Equal(t, int64(1777593600000), body.StartDate)
		assert.Equal(t, int64(1777766399000), body.EndDate)

		w.Header().Set("Content-Type", "application/json")
		switch body.Page {
		case 1:
			assert.NoError(t, json.MarshalWrite(w, page1))
		case 2:
			assert.NoError(t, json.MarshalWrite(w, page2))
		default:
			assert.NoError(t, json.MarshalWrite(w, usageEventsEnvelope{}))
		}
	}))
	t.Cleanup(srv.Close)

	client := NewClientWithBaseURL(srv.URL, "cursor-key")
	client.RequestInterval = 0
	events, err := client.FetchAllUsageEvents(
		t.Context(),
		Query{
			StartDate: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			EndDate:   time.Date(2026, 5, 2, 23, 59, 59, 0, time.UTC),
			PageSize:  1,
			Email:     "member@example.com",
			UserID:    "152683922",
		},
	)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "claude-4.6-opus-high-thinking", events[0].Model)
	assert.Equal(t, 1234, events[0].TokenUsage.InputTokens)
	assert.Equal(t, money.Money{Microdollars: 156_600}, events[0].Charged)
	assert.Equal(t, money.Money{Microdollars: 33_200}, events[0].CursorTokenFee)
	assert.Equal(t, "member@example.com", events[0].UserEmail)
	assert.False(t, events[0].IsHeadless)
	assert.Equal(t, time.UnixMilli(1748700000000).UTC(), events[0].Timestamp)
}

func TestParseOptionalCentsRejectsNegativeCharge(t *testing.T) {
	_, err := parseOptionalCents(jsontext.Value("-1"))
	assert.ErrorIs(t, err, money.ErrNegative)
}

func TestListUsageEventsRejectsNonNumericUserID(t *testing.T) {
	client := NewClientWithBaseURL("https://example.test", "cursor-key")

	_, err := client.ListUsageEvents(t.Context(), Query{
		StartDate: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		EndDate:   time.Date(2026, 5, 2, 23, 59, 59, 0, time.UTC),
		UserID:    "user_123",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid Cursor user ID "user_123"`)
}

func TestParseUsageEventConversationID(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    string
	}{
		{`{"timestamp":"2026-07-28T10:00:00Z","conversationId":" conversation-a "}`, "conversation-a"},
		{`{"timestamp":"2026-07-28T10:00:00Z"}`, ""},
	} {
		var raw rawUsageEvent
		require.NoError(t, json.Unmarshal([]byte(tc.payload), &raw))
		event, err := parseUsageEvent(raw)
		require.NoError(t, err)
		assert.Equal(t, tc.want, event.ConversationID)
	}
}
