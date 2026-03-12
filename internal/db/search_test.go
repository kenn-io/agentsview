package db

import (
	"context"
	"testing"
)

func TestSearchSession(t *testing.T) {
	t.Parallel()
	d := testDB(t)

	insertSession(t, d, "s1", "proj")
	insertSession(t, d, "s2", "proj")

	insertMessages(t, d,
		userMsg("s1", 0, "Hello world, this is a test message"),
		asstMsg("s1", 1, "Here is some Python code: import os; print(os.getcwd())"),
		userMsg("s1", 2, "Can you search for **bold markdown** syntax?"),
		asstMsg("s1", 3, "Another message with no special content"),
		userMsg("s2", 0, "This belongs to a different session entirely"),
	)

	tests := []struct {
		name      string
		sessionID string
		query     string
		want      []int // expected ordinals
	}{
		{
			name:      "simple substring match",
			sessionID: "s1",
			query:     "test",
			want:      []int{0},
		},
		{
			name:      "case insensitive",
			sessionID: "s1",
			query:     "HELLO",
			want:      []int{0},
		},
		{
			name:      "matches multiple messages",
			sessionID: "s1",
			query:     "message",
			want:      []int{0, 3},
		},
		{
			name:      "matches inside code content",
			sessionID: "s1",
			query:     "import os",
			want:      []int{1},
		},
		{
			name:      "matches raw markdown syntax",
			sessionID: "s1",
			query:     "bold markdown",
			want:      []int{2},
		},
		{
			name:      "no match returns empty",
			sessionID: "s1",
			query:     "nonexistent",
			want:      []int{},
		},
		{
			name:      "scoped to session — does not bleed across sessions",
			sessionID: "s1",
			query:     "different session",
			want:      []int{},
		},
		{
			name:      "other session scoped correctly",
			sessionID: "s2",
			query:     "different session",
			want:      []int{0},
		},
		{
			name:      "empty query returns nil",
			sessionID: "s1",
			query:     "",
			want:      []int{},
		},
		{
			name:      "LIKE special chars escaped — percent sign",
			sessionID: "s1",
			query:     "%",
			want:      []int{},
		},
		{
			name:      "LIKE special chars escaped — underscore",
			sessionID: "s1",
			query:     "_",
			want:      []int{},
		},
		{
			name:      "results ordered by ordinal ascending",
			sessionID: "s1",
			query:     "is",
			want:      []int{0, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := d.SearchSession(context.Background(), tt.sessionID, tt.query)
			if err != nil {
				t.Fatalf("SearchSession(%q, %q): unexpected error: %v", tt.sessionID, tt.query, err)
			}
			if got == nil {
				got = []int{}
			}
			if len(got) != len(tt.want) {
				t.Fatalf("SearchSession(%q, %q) = %v, want %v", tt.sessionID, tt.query, got, tt.want)
			}
			for i, ord := range got {
				if ord != tt.want[i] {
					t.Errorf("ordinal[%d] = %d, want %d", i, ord, tt.want[i])
				}
			}
		})
	}
}
