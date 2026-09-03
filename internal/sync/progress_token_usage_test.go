package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TokenUsageBlanked must reach the CLI summary like every other sanitize
// counter. The pass silently drops a malformed provider usage blob, and a
// silent drop with no reporting is what made the underlying bug
// (a panicking GET /api/v1/sessions/{id}/messages) hard to trace.
func TestSanitizeStatsIncludesTokenUsageBlanked(t *testing.T) {
	s := SanitizeStats{TokenUsageBlanked: 3}
	assert.Equal(t, 3, s.Total())
	assert.False(t, s.IsZero())
}

func TestAnomalyStatsAccumulatesTokenUsageBlanked(t *testing.T) {
	var a AnomalyStats
	a.addSanitize(validationStats{TokenUsageBlanked: 2})
	a.addSanitize(validationStats{TokenUsageBlanked: 1})
	assert.Equal(t, 3, a.Sanitize.TokenUsageBlanked)

	var b AnomalyStats
	b.merge(a)
	assert.Equal(t, 3, b.Sanitize.TokenUsageBlanked)
	assert.Equal(t, 3, b.Sanitize.Total())
}
