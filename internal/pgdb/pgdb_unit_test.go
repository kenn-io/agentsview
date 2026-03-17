package pgdb

import (
	"testing"

	"github.com/wesm/agentsview/internal/pgutil"
)

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			"postgres://user:secret@myhost:5432/db?sslmode=disable",
			"myhost",
		},
		{
			"postgres://user:secret@myhost:5432/db?password=leaked",
			"myhost",
		},
		{
			"postgres://myhost/db",
			"myhost",
		},
		{
			"not a url",
			"",
		},
	}
	for _, tt := range tests {
		got := pgutil.RedactDSN(tt.input)
		if got != tt.want {
			t.Errorf("RedactDSN(%q) = %q, want %q",
				tt.input, got, tt.want)
		}
	}
}
