package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileSeatPatterns(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		wantErr string
	}{
		{"none", nil, ""},
		{"spec example", []string{"*/profiles/{seat}/projects/*"}, ""},
		{"no placeholder", []string{"*/profiles/*"}, `seat pattern "*/profiles/*" must contain exactly one {seat}`},
		{"two placeholders", []string{"{seat}/{seat}"}, `seat pattern "{seat}/{seat}" must contain exactly one {seat}`},
		{"partial segment", []string{"profiles/seat-{seat}"}, `seat pattern "profiles/seat-{seat}": {seat} must be a whole path segment`},
		{"bad glob", []string{"[profiles/{seat}"}, `seat pattern "[profiles/{seat}": syntax error in pattern`},
		{"one bad entry fails the list", []string{"a/{seat}/b", "nope"}, `seat pattern "nope" must contain exactly one {seat}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CompileSeatPatterns(tt.in)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Len(t, got, len(tt.in))
		})
	}
}

func TestSeatFromPath(t *testing.T) {
	compile := func(t *testing.T, p ...string) []SeatPattern {
		t.Helper()
		out, err := CompileSeatPatterns(p)
		require.NoError(t, err)
		return out
	}
	tests := []struct {
		name     string
		path     string
		patterns []string
		want     string
	}{
		{"seat_from_path_no_patterns", "/home/user/agents/profiles/seat-01/projects/p/s.jsonl", nil, ""},
		{
			"spec example on absolute path", "/home/user/agents/profiles/seat-01/projects/app/s.jsonl",
			[]string{"*/profiles/{seat}/projects/*"},
			"seat-01",
		},
		{
			"pool sessions layout", "/srv/pool/seat-02/sessions/2026/09/13/rollout-x.jsonl",
			[]string{"pool/{seat}/sessions"},
			"seat-02",
		},
		{
			"star stays within one segment", "/home/user/a/b/profiles/seat-03/projects/p/s.jsonl",
			[]string{"/home/*/profiles/{seat}"},
			"",
		},
		{
			"anchored pattern", "/home/user/profiles/seat-04/projects/p/s.jsonl",
			[]string{"/home/*/profiles/{seat}"},
			"seat-04",
		},
		{
			"anchored pattern does not float", "/mnt/home/user/profiles/seat-05/s.jsonl",
			[]string{"/home/*/profiles/{seat}"},
			"",
		},
		{
			"no match", "/home/user/.claude/projects/p/s.jsonl",
			[]string{"*/profiles/{seat}/projects/*"},
			"",
		},
		{
			"first pattern wins", "/x/profiles/seat-06/pool/seat-07/s.jsonl",
			[]string{"pool/{seat}", "profiles/{seat}"},
			"seat-07",
		},
		{
			"leftmost window wins", "/x/profiles/seat-08/profiles/seat-09/s.jsonl",
			[]string{"profiles/{seat}"},
			"seat-08",
		},
		{
			"windows separators", `C:\Users\user\pool\seat-10\sessions\rollout-x.jsonl`,
			[]string{"pool/{seat}/sessions"},
			"seat-10",
		},
		{
			"character class glob", "/srv/pool-b/seat-11/s.jsonl",
			[]string{"pool-[ab]/{seat}"},
			"seat-11",
		},
		{"empty path", "", []string{"*/{seat}"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SeatFromPath(tt.path, compile(t, tt.patterns...)))
		})
	}
}
