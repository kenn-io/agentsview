package cursorusage

import (
	"context"
	"encoding/json"
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
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/teams/filtered-usage-events", r.URL.Path)

		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, "cursor-key", user)
		assert.Equal(t, "", pass)

		var body requestBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, 1, body.PageSize)
		assert.Equal(t, "member@example.com", body.Email)
		assert.Equal(t, int64(152683922), body.UserID)
		assert.Equal(t, int64(1777593600000), body.StartDate)
		assert.Equal(t, int64(1777766399000), body.EndDate)

		enc := json.NewEncoder(w)
		w.Header().Set("Content-Type", "application/json")
		switch body.Page {
		case 1:
			require.NoError(t, enc.Encode(page1))
		case 2:
			require.NoError(t, enc.Encode(page2))
		default:
			require.NoError(t, enc.Encode(usageEventsEnvelope{}))
		}
	}))
	t.Cleanup(srv.Close)

	client := NewClientWithBaseURL(srv.URL, "cursor-key")
	events, err := client.FetchAllUsageEvents(
		context.Background(),
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
	assert.Equal(t, false, events[0].IsHeadless)
	assert.Equal(t, time.UnixMilli(1748700000000).UTC(), events[0].Timestamp)
}

func TestParseOptionalCentsRejectsNegativeCharge(t *testing.T) {
	_, err := parseOptionalCents(json.Number("-1"))
	assert.ErrorIs(t, err, money.ErrNegative)
}

func TestListUsageEventsRejectsNonNumericUserID(t *testing.T) {
	client := NewClientWithBaseURL("https://example.test", "cursor-key")

	_, err := client.ListUsageEvents(context.Background(), Query{
		StartDate: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		EndDate:   time.Date(2026, 5, 2, 23, 59, 59, 0, time.UTC),
		UserID:    "user_123",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid Cursor user ID "user_123"`)
}
