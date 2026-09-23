package friction

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var reviewBase = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

func rmin(m int) time.Time { return reviewBase.Add(time.Duration(m) * time.Minute) }

func reviewFixture(dims Dims) SessionInput {
	msgs := []RawMessage{
		{Ordinal: 0, Role: "user", Content: "please run the tests", Timestamp: rmin(0)},
		{Ordinal: 1, Role: "assistant", Content: "Skipping for now, I'll come back to this.", Timestamp: rmin(1)},
		{Ordinal: 2, Role: "user", Content: "thanks, that looks really great", Timestamp: rmin(2)},
		{Ordinal: 3, Role: "assistant", Content: "Ran them.", Timestamp: rmin(3)},
		{Ordinal: 4, Role: "user", Content: "this is broken again, same error", Timestamp: rmin(4)},
		{Ordinal: 5, Role: "assistant", Content: "Looking.", Timestamp: rmin(5)},
		{
			Ordinal: 6, Role: "user", Content: "[Request interrupted by user]", IsSystem: true,
			SourceSubtype: SourceSubtypeInterrupted, Timestamp: rmin(6),
		},
	}
	// The failing call belongs to the last assistant turn, so its tool
	// message does not break either (assistant, user, assistant) triple.
	calls := []RawToolCall{{
		MessageOrdinal: 5, ToolName: "Bash", Category: "Bash",
		EventStatus: "errored", LastEventContent: "error[E0308]: mismatched types",
	}}
	return BuildSessionInput("s1", dims, false, msgs, calls, BuildOptions{})
}

func TestReview(t *testing.T) {
	t.Run("coding session order dims subject kind and seq", func(t *testing.T) {
		dims := Dims{Seat: "seat-01", Agent: "claude", Machine: "laptop", Channel: "ignored"}
		got := Review(reviewFixture(dims))
		var detectors []string
		for i, s := range got {
			detectors = append(detectors, s.Detector)
			assert.Equal(t, i, s.Seq)
			assert.Equal(t, SubjectSession, s.SubjectKind)
			assert.Equal(t, Dims{Seat: "seat-01", Agent: "claude", Machine: "laptop"}, s.Dims,
				"channel is stamped only with a persona")
		}
		assert.Equal(t, []string{
			DetectorCorrectionCoding, DetectorCorrectionCoding,
			DetectorError, DetectorWorkaround, DetectorDeferral,
			DetectorFrustration, DetectorInterruption,
		}, detectors)
	})

	t.Run("persona selects chat corrections", func(t *testing.T) {
		dims := Dims{Persona: "helper", Channel: "general"}
		got := Review(reviewFixture(dims))
		require.NotEmpty(t, got)
		for _, s := range got {
			assert.NotEqual(t, DetectorCorrectionCoding, s.Detector)
			assert.Equal(t, dims, s.Dims)
		}
		assert.Equal(t, DetectorError, got[0].Detector,
			"plain short chat is not a chat correction")
	})

	t.Run("excluded session yields nil", func(t *testing.T) {
		in := reviewFixture(Dims{})
		in.Excluded = true
		assert.Nil(t, Review(in))
	})

	t.Run("patterns precede the agentsview kinds", func(t *testing.T) {
		in := reviewFixture(Dims{})
		p := 0.97
		in.Patterns.PressureMax = &p
		got := Review(in)
		n := len(got)
		assert.Equal(t, "pattern.context_pressure", got[n-3].Detector)
		assert.Equal(t, DetectorFrustration, got[n-2].Detector)
		assert.Equal(t, DetectorInterruption, got[n-1].Detector)
		assert.Equal(t, n-1, got[n-1].Seq)
	})
}
