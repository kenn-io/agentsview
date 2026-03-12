package server_test

import (
	"net/url"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

func TestHandleSearchSession(t *testing.T) {
	t.Parallel()

	te := setup(t)
	te.seedSession(t, "s1", "proj", 4)
	te.seedMessages(t, "s1", 4, func(i int, m *db.Message) {
		switch i {
		case 0:
			m.Content = "Hello world, this is a test"
		case 1:
			m.Content = "import os; print(os.getcwd())"
		case 2:
			m.Content = "Another message about testing"
		case 3:
			m.Content = "No special content here"
		}
		m.ContentLength = len(m.Content)
	})

	// Second session to verify isolation
	te.seedSession(t, "s2", "proj", 1)
	te.seedMessages(t, "s2", 1, func(_ int, m *db.Message) {
		m.Content = "test content in other session"
		m.ContentLength = len(m.Content)
	})

	type searchResp struct {
		Ordinals []int `json:"ordinals"`
	}

	tests := []struct {
		name         string
		sessionID    string
		query        string
		wantStatus   int
		wantOrdinals []int
	}{
		{
			name:         "matches single message",
			sessionID:    "s1",
			query:        "Hello",
			wantStatus:   200,
			wantOrdinals: []int{0},
		},
		{
			name:         "case insensitive match",
			sessionID:    "s1",
			query:        "IMPORT",
			wantStatus:   200,
			wantOrdinals: []int{1},
		},
		{
			name:         "matches multiple messages",
			sessionID:    "s1",
			query:        "test",
			wantStatus:   200,
			wantOrdinals: []int{0, 2},
		},
		{
			name:         "no match returns empty slice",
			sessionID:    "s1",
			query:        "nonexistent",
			wantStatus:   200,
			wantOrdinals: []int{},
		},
		{
			name:         "scoped to session — does not include s2 results",
			sessionID:    "s1",
			query:        "other session",
			wantStatus:   200,
			wantOrdinals: []int{},
		},
		{
			name:         "empty query returns empty slice",
			sessionID:    "s1",
			query:        "",
			wantStatus:   200,
			wantOrdinals: []int{},
		},
		{
			name:         "results in ordinal order",
			sessionID:    "s1",
			query:        "test",
			wantStatus:   200,
			wantOrdinals: []int{0, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := "/api/v1/sessions/" + tt.sessionID + "/search"
			if tt.query != "" {
				path += "?q=" + url.QueryEscape(tt.query)
			}
			w := te.get(t, path)
			assertStatus(t, w, tt.wantStatus)

			resp := decode[searchResp](t, w)
			if resp.Ordinals == nil {
				resp.Ordinals = []int{}
			}
			if len(resp.Ordinals) != len(tt.wantOrdinals) {
				t.Fatalf("ordinals = %v, want %v", resp.Ordinals, tt.wantOrdinals)
			}
			for i, ord := range resp.Ordinals {
				if ord != tt.wantOrdinals[i] {
					t.Errorf("ordinal[%d] = %d, want %d", i, ord, tt.wantOrdinals[i])
				}
			}
		})
	}
}
