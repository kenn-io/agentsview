package filing_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/friction/filing"
)

func TestReopenAllowed(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		{"done", true},
		{"", false},
		{"wontfix", false},
		{"duplicate", false},
		{"superseded", false},
		{"audit-no-change", false},
		{"anything-else", false},
		{"Done", false},
	}
	for _, tt := range tests {
		t.Run("reopen_allowed_only_for_done/"+tt.reason, func(t *testing.T) {
			assert.Equal(t, tt.want, filing.ReopenAllowed(tt.reason))
		})
	}
}

func TestRecurrenceCommentAndKey(t *testing.T) {
	assert.Equal(t, "friction-recur-01J0ABCDEF0000000000000001-2026-09-03", filing.RecurrenceCommentKey("01J0ABCDEF0000000000000001", "2026-09-03"))
	tests := []struct {
		name, url, want string
	}{
		{name: "no_public_url", want: "Recurred on 2026-09-03 — closure may have been premature."},
		{
			name: "with_session_url", url: "https://av.example.test/sessions/claude/s1?msg=3",
			want: "Recurred on 2026-09-03 — closure may have been premature.\nSession: https://av.example.test/sessions/claude/s1?msg=3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, filing.RecurrenceComment("2026-09-03", tt.url)) })
	}
}
