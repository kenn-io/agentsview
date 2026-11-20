package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVectorUnavailableReason(t *testing.T) {
	for _, tc := range []struct {
		name, version, reason string
		halfvec               bool
	}{
		{"missing extension", "", "pgvector extension unavailable", false},
		{"missing halfvec", "0.6.2", "pgvector 0.6.2 lacks the halfvec type; upgrade to pgvector 0.8.0+", false},
		{"old version", "0.7.4", "pgvector 0.7.4 lacks iterative scan; upgrade to pgvector 0.8.0+", true},
		{"minimum version", "0.8.0", "", true},
		{"newer version", "0.10.0", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.reason, vectorUnavailableReason(tc.halfvec, tc.version))
		})
	}
}
