package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseIntParam(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		param      string
		wantVal    int
		wantOK     bool
		wantStatus int
	}{
		{
			name:       "absent param returns zero",
			query:      "",
			param:      "limit",
			wantVal:    0,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid integer",
			query:      "limit=42",
			param:      "limit",
			wantVal:    42,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "negative integer",
			query:      "limit=-5",
			param:      "limit",
			wantVal:    -5,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "non-numeric returns 400",
			query:      "limit=abc",
			param:      "limit",
			wantVal:    0,
			wantOK:     false,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "float returns 400",
			query:      "limit=3.5",
			param:      "limit",
			wantVal:    0,
			wantOK:     false,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, r := newTestRequest(t, tt.query)

			val, ok := parseIntParam(w, r, tt.param)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantVal, val)
			assert.Equal(t, tt.wantStatus, w.Code)
		})
	}
}

func TestParseBoolParam(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantVal    bool
		wantOK     bool
		wantStatus int
	}{
		{
			name:       "absent",
			query:      "",
			wantVal:    false,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "true",
			query:      "dry_run=true",
			wantVal:    true,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "false",
			query:      "dry_run=false",
			wantVal:    false,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "one rejected",
			query:      "dry_run=1",
			wantVal:    false,
			wantOK:     false,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "yes rejected",
			query:      "dry_run=yes",
			wantVal:    false,
			wantOK:     false,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "uppercase rejected",
			query:      "dry_run=TRUE",
			wantVal:    false,
			wantOK:     false,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, r := newTestRequest(t, tt.query)

			val, ok := parseBoolParam(w, r, "dry_run")
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantVal, val)
			assert.Equal(t, tt.wantStatus, w.Code)
		})
	}
}

// TestParseNonNegativeIntParam pins the cursor-validation contract:
// negative integers, which would flow through to SQL OFFSET and 500
// on PostgreSQL, must be rejected with a 400 at the handler.
func TestParseNonNegativeIntParam(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantVal    int
		wantOK     bool
		wantStatus int
	}{
		{
			name:       "absent",
			query:      "",
			wantVal:    0,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "zero",
			query:      "cursor=0",
			wantVal:    0,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "positive",
			query:      "cursor=50",
			wantVal:    50,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "negative rejected",
			query:      "cursor=-1",
			wantVal:    0,
			wantOK:     false,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "non-numeric rejected",
			query:      "cursor=abc",
			wantVal:    0,
			wantOK:     false,
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, r := newTestRequest(t, tt.query)
			val, ok := parseNonNegativeIntParam(w, r, "cursor")
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantVal, val)
			assert.Equal(t, tt.wantStatus, w.Code)
		})
	}
}

func TestClampLimit(t *testing.T) {
	const max = 1000
	const defaultLimit = 100
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{"zero uses default", 0, defaultLimit},
		{"negative uses default", -1, defaultLimit},
		{"within range", defaultLimit / 2, defaultLimit / 2},
		{"at max", max, max},
		{"exceeds max", max + 1, max},
		{"default itself", defaultLimit, defaultLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clampLimit(tt.limit, defaultLimit, max))
		})
	}
}
