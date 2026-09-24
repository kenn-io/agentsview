package ledger

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports ledger-spool lib.rs:70 source_name_pattern.
func TestValidSourceName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"source_name_pattern/good/host-b", "host-b", true},
		{"source_name_pattern/good/host-a", "host-a", true},
		{"source_name_pattern/good/host-2", "host-2", true},
		{"source_name_pattern/good/a", "a", true},
		{"source_name_pattern/good/A.b_c-9", "A.b_c-9", true},
		{"source_name_pattern/good/0x", "0x", true},
		{"source_name_pattern/good/64_chars", strings.Repeat("x", 64), true},
		{"source_name_pattern/good/default_local_source", "av-0123456789abcdef0123456789abcdef", true},
		{"source_name_pattern/bad/empty", "", false},
		{"source_name_pattern/bad/traversal", "../evil", false},
		{"source_name_pattern/bad/slash", "a/b", false},
		{"source_name_pattern/bad/hidden", ".hidden", false},
		{"source_name_pattern/bad/dash", "-dash", false},
		{"source_name_pattern/bad/under", "_under", false},
		{"source_name_pattern/bad/space", "has space", false},
		{"source_name_pattern/bad/nul", "host\x00", false},
		{"source_name_pattern/bad/65_chars", strings.Repeat("x", 65), false},
		{"non_ascii_letter", "café", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ValidSourceName(tt.in))
		})
	}
}

// The expected values come from the Rust uuid crate (testdata/uuid-v5.txt),
// so any implementation of the same rule reproduces them.
func TestDeterministicEventID(t *testing.T) {
	assert.Equal(t, "16f05014-0274-5b1e-8e2a-a20f580338dd", EventIDNamespace.String())
	tests := []struct{ source, key, want string }{
		{"av-0123", "diagnostic:disk-full:host-a", "08cc4887-38ea-5214-b432-940c09f51989"},
		{"host", "", "00c404da-9595-577f-a79c-2362f433a56e"},
	}
	for _, tt := range tests {
		t.Run(tt.source+"/"+tt.key, func(t *testing.T) {
			got := DeterministicEventID(tt.source, tt.key)
			assert.Equal(t, tt.want, got.String())
			assert.Equal(t, uuid.Version(5), got.Version())
			assert.Equal(t, got, DeterministicEventID(tt.source, tt.key), "stable")
		})
	}
	assert.NotEqual(t, DeterministicEventID("ab", "c"), DeterministicEventID("a", "bc"),
		"the NUL separator keeps source and key apart")
}

func TestNewEventIDIsV7AndOrdered(t *testing.T) {
	a := NewEventID()
	b := NewEventID()
	require.Equal(t, uuid.Version(7), a.Version())
	assert.Equal(t, uuid.RFC4122, a.Variant())
	assert.Less(t, a.String(), b.String(), "v7 IDs sort by creation time")
}
