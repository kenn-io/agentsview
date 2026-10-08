package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMirroredSessionMachine(t *testing.T) {
	tests := []struct {
		name           string
		sessionMachine string
		want           string
	}{
		{
			name:           "explicit source machine",
			sessionMachine: "source-machine",
			want:           "source-machine",
		},
		{
			name:           "empty source machine",
			sessionMachine: "",
			want:           "push-machine",
		},
		{
			name:           "local sentinel",
			sessionMachine: "local",
			want:           "push-machine",
		},
		{
			name:           "explicit whitespace is preserved",
			sessionMachine: " source-machine ",
			want:           " source-machine ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MirroredSessionMachine(
				Session{Machine: tt.sessionMachine}, "push-machine",
			))
		})
	}
}

func TestMirroredSessionGroupsStaySeparate(t *testing.T) {
	var entries []TopSessionEntry
	for _, tc := range []struct{ machine, fallback, key, want string }{
		{"local", "host-a", "job-a@12345678", "host-a~job-a@12345678"},
		{"", "host-b", "job-a@12345678", "host-b~job-a@12345678"},
		{"host-c", "host-a", "job-a@12345678", "host-c~job-a@12345678"},
		{"host-d", "host-a", "host-d~job-a@12345678", "host-d~job-a@12345678"},
	} {
		key := MirroredSessionGroupKey(Session{Machine: tc.machine, GroupKey: tc.key}, tc.fallback)
		assert.Equal(t, tc.want, key)
		entries = append(entries, TopSessionEntry{Project: "hermes-cron", GroupKey: key})
	}
	rows, err := GroupTopSessions(entries, 100, TopSessionsSortCost, UsageTokenTypesAll)
	require.NoError(t, err)
	assert.Len(t, rows, 4)
	assert.Empty(t, MirroredSessionGroupKey(Session{}, "host-a"))
}
