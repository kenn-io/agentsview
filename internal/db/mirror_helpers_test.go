package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
