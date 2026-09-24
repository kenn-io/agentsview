package friction

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSessionInputNanoClawEnvelope(t *testing.T) {
	ts := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	env := func(body string) string {
		return "<context timezone=\"UTC\" />\n<message id=\"1\" from=\"chat-mg-1\">" + body + "</message>"
	}
	msgs := []RawMessage{
		{Ordinal: 0, Role: "assistant", Content: "Posting the summary here.", Timestamp: ts},
		{Ordinal: 1, Role: "user", Content: env("no helper, don&#39;t answer in that channel"), Timestamp: ts},
		{Ordinal: 2, Role: "assistant", Content: "Understood.", Timestamp: ts},
		{Ordinal: 3, Role: "user", Content: "<message id=\"2\"> </message>", Timestamp: ts},
		{Ordinal: 4, Role: "assistant", Content: "Anything else?", Timestamp: ts},
	}

	t.Run("unwrap_on", func(t *testing.T) {
		in := BuildSessionInput("s", Dims{Persona: "helper"}, false, msgs, nil,
			BuildOptions{UnwrapNanoClawEnvelope: true})
		var users []string
		for _, m := range in.Messages {
			if m.Role == "user" {
				users = append(users, m.Text)
			}
		}
		assert.Equal(t, []string{"no helper, don't answer in that channel"}, users,
			"an all-empty envelope is dropped from the detector stream")
		assert.Equal(t, []int{1, 3}, in.Patterns.UserOrdinals,
			"both envelopes are still user activity for iteration_runaway")
	})

	t.Run("unwrap_off_keeps_raw_text", func(t *testing.T) {
		in := BuildSessionInput("s", Dims{}, false, msgs, nil, BuildOptions{})
		require.Equal(t, "user", in.Messages[1].Role)
		assert.Equal(t, env("no helper, don&#39;t answer in that channel"), in.Messages[1].Text)
		assert.Equal(t, []int{1, 3}, in.Patterns.UserOrdinals)
	})
}

// The envelope is longer than MaxCorrectionLength; its text is not. jilog
// measures the unwrapped text (nanoclaw.rs:28-31), so the correction fires
// only when the envelope is unwrapped.
func TestEnvelopeUnwrapChangesCorrectionLength(t *testing.T) {
	ts := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	envelope := "<context timezone=\"UTC\" />\n<message id=\"2\" from=\"chat-mg-1\" sender=\"100@example.invalid\" time=\"Jul 8, 2026, 4:11 PM\" " +
		"reply-to=\"" + strings.Repeat("x", 120) + "\">no, don&#39;t post the summary there</message>"
	require.Greater(t, len(envelope), MaxCorrectionLength)
	msgs := []RawMessage{
		{Ordinal: 0, Role: "assistant", Content: "Posting the summary here.", Timestamp: ts},
		{Ordinal: 1, Role: "user", Content: envelope, Timestamp: ts},
		{Ordinal: 2, Role: "assistant", Content: "Understood.", Timestamp: ts},
	}
	tests := []struct {
		name   string
		unwrap bool
		want   int
	}{
		{"cell_session_unwrapped_fires", true, 1},
		{"raw_envelope_is_too_long", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := BuildSessionInput("s", Dims{Persona: "helper"}, false, msgs, nil,
				BuildOptions{UnwrapNanoClawEnvelope: tt.unwrap})
			var corrections []Signal
			for _, s := range Review(in) {
				if s.Kind == KindCorrection {
					corrections = append(corrections, s)
				}
			}
			require.Len(t, corrections, tt.want)
			if tt.want == 1 {
				assert.Equal(t, "no, don't post the summary there", corrections[0].Text)
				assert.Equal(t, "correction.chat", corrections[0].Detector)
			}
		})
	}
}
