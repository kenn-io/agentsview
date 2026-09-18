package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectScopeRows runs rows through a reducer and returns emitted rows.
func collectScopeRows(t *testing.T, f ScopeFilter, rows []MessageInput) ([]ScopedMessage, error) {
	t.Helper()
	var out []ScopedMessage
	r := NewScopeReducer(f, func(m ScopedMessage) { out = append(out, m) })
	for _, row := range rows {
		if err := r.Push(row); err != nil {
			return out, err
		}
	}
	return out, nil
}

func scopeUser(session string, ord int) MessageInput {
	return MessageInput{SessionID: session, Ordinal: ord, Role: "user", HasLocalTime: true}
}

func scopeAssistant(session string, ord int, model string) MessageInput {
	return MessageInput{SessionID: session, Ordinal: ord, Role: "assistant", Model: model, HasLocalTime: true}
}

func TestScopeReducerPairing(t *testing.T) {
	f := ScopeFilter{Models: map[string]struct{}{"sonnet": {}}}

	t.Run("selected assistant flushes preceding user", func(t *testing.T) {
		out, err := collectScopeRows(t, f, []MessageInput{scopeUser("s1", 0), scopeAssistant("s1", 1, "sonnet")})
		require.NoError(t, err)
		require.Len(t, out, 2)
		assert.Equal(t, "user", out[0].Role)
		assert.Equal(t, "assistant", out[1].Role)
	})

	t.Run("non-selected assistant drops preceding user", func(t *testing.T) {
		out, err := collectScopeRows(t, f, []MessageInput{scopeUser("s1", 0), scopeAssistant("s1", 1, "opus")})
		require.NoError(t, err)
		assert.Empty(t, out)
	})

	t.Run("session change drops pending", func(t *testing.T) {
		out, err := collectScopeRows(t, f, []MessageInput{scopeUser("s1", 0), scopeAssistant("s2", 0, "sonnet")})
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "s2", out[0].SessionID)
	})

	t.Run("day/hour filter drops both user and assistant", func(t *testing.T) {
		ff := ScopeFilter{Models: map[string]struct{}{"sonnet": {}}, Hour: new(9)}
		mon14 := time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
		rows := []MessageInput{
			{SessionID: "s1", Ordinal: 0, Role: "user", HasLocalTime: true, LocalTime: mon14},
			{SessionID: "s1", Ordinal: 1, Role: "assistant", Model: "sonnet", HasLocalTime: true, LocalTime: mon14},
		}
		out, err := collectScopeRows(t, ff, rows)
		require.NoError(t, err)
		assert.Empty(t, out)
	})

	// The day/hour match applies per message, not per pair: a flushed user and
	// its triggering assistant are filtered independently.
	t.Run("day/hour filter keeps matching user, drops non-matching assistant", func(t *testing.T) {
		ff := ScopeFilter{Models: map[string]struct{}{"sonnet": {}}, Hour: new(9)}
		mon09 := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
		mon14 := time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
		rows := []MessageInput{
			{SessionID: "s1", Ordinal: 0, Role: "user", HasLocalTime: true, LocalTime: mon09},
			{SessionID: "s1", Ordinal: 1, Role: "assistant", Model: "sonnet", HasLocalTime: true, LocalTime: mon14},
		}
		out, err := collectScopeRows(t, ff, rows)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "user", out[0].Role)
	})

	t.Run("day/hour filter drops non-matching user, keeps matching assistant", func(t *testing.T) {
		ff := ScopeFilter{Models: map[string]struct{}{"sonnet": {}}, Hour: new(9)}
		mon09 := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
		mon14 := time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
		rows := []MessageInput{
			{SessionID: "s1", Ordinal: 0, Role: "user", HasLocalTime: true, LocalTime: mon14},
			{SessionID: "s1", Ordinal: 1, Role: "assistant", Model: "sonnet", HasLocalTime: true, LocalTime: mon09},
		}
		out, err := collectScopeRows(t, ff, rows)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "assistant", out[0].Role)
	})

	// A selected non-assistant row interleaved with a still-pending user emits
	// immediately, ahead of the buffered user (ordinals 1, 0, 2). This pins
	// parity with the reference getAnalyticsModelScopedMessages default case
	// (analytics.go); changing it is a cross-backend behavior change that must
	// update all backends and the oracle together.
	t.Run("selected non-assistant row emits ahead of pending user (reference parity)", func(t *testing.T) {
		rows := []MessageInput{
			{SessionID: "s1", Ordinal: 0, Role: "user", HasLocalTime: true},
			{SessionID: "s1", Ordinal: 1, Role: "user", IsSystem: true, Model: "sonnet", HasLocalTime: true},
			{SessionID: "s1", Ordinal: 2, Role: "assistant", Model: "sonnet", HasLocalTime: true},
		}
		out, err := collectScopeRows(t, f, rows)
		require.NoError(t, err)
		require.Len(t, out, 3)
		assert.Equal(t, []int{1, 0, 2}, []int{out[0].Ordinal, out[1].Ordinal, out[2].Ordinal})
	})
}

func TestScopeReducerOrdering(t *testing.T) {
	f := ScopeFilter{Models: map[string]struct{}{"sonnet": {}}}

	t.Run("decreasing ordinal within session errors", func(t *testing.T) {
		_, err := collectScopeRows(t, f, []MessageInput{scopeAssistant("s1", 5, "sonnet"), scopeAssistant("s1", 2, "sonnet")})
		require.Error(t, err)
	})

	// Sessions need only be grouped, not byte-ordered. PostgreSQL "ORDER BY
	// session_id" under a non-C collation can hand the reducer groups in an
	// order Go string comparison calls "backwards"; that is valid input and
	// must not error, or model-filtered analytics would fail on those rows.
	t.Run("non-byte-ordered session groups are accepted", func(t *testing.T) {
		out, err := collectScopeRows(t, f, []MessageInput{
			scopeUser("s2", 0), scopeAssistant("s2", 1, "sonnet"),
			scopeUser("s1", 0), scopeAssistant("s1", 1, "sonnet"),
		})
		require.NoError(t, err)
		require.Len(t, out, 4)
		assert.Equal(t, "s2", out[0].SessionID)
		assert.Equal(t, "s1", out[2].SessionID)
	})

	t.Run("session reappearing after its group ends errors", func(t *testing.T) {
		_, err := collectScopeRows(t, f, []MessageInput{
			scopeAssistant("s1", 0, "sonnet"),
			scopeAssistant("s2", 0, "sonnet"),
			scopeAssistant("s1", 1, "sonnet"),
		})
		require.Error(t, err)
	})

	t.Run("grouped ascending stream is fine", func(t *testing.T) {
		_, err := collectScopeRows(t, f, []MessageInput{scopeAssistant("s1", 0, "sonnet"), scopeAssistant("s1", 1, "sonnet"), scopeAssistant("s2", 0, "sonnet")})
		require.NoError(t, err)
	})
}
