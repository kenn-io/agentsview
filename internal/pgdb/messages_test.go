package pgdb

import "testing"

func TestStripFTSQuotes(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`"hello world"`, "hello world"},
		{`hello`, "hello"},
		{`"single`, `"single`},
		{`""`, ""},
		{`"a"`, "a"},
		{`already unquoted`, "already unquoted"},
	}
	for _, tt := range tests {
		got := stripFTSQuotes(tt.input)
		if got != tt.want {
			t.Errorf("stripFTSQuotes(%q) = %q, want %q",
				tt.input, got, tt.want)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"100%", `100\%`},
		{"under_score", `under\_score`},
		{`back\slash`, `back\\slash`},
		{`%_\`, `\%\_\\`},
	}
	for _, tt := range tests {
		got := escapeLike(tt.input)
		if got != tt.want {
			t.Errorf("escapeLike(%q) = %q, want %q",
				tt.input, got, tt.want)
		}
	}
}
