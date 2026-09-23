package friction

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var kindsBase = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

func TestDetectFrustration(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"phrase !!!", "why is this failing!!!", true},
		{"phrase ???", "what happened to the build???", true},
		{"phrase wtf", "wtf is this output", true},
		{"phrase come on", "come on, just run the tests", true},
		{"phrase why won't", "why won't the build pass", true},
		{"phrase this is broken", "this is broken again", true},
		{"phrase doesn't work", "the fix doesn't work here", true},
		{"phrase does not work", "the fix does not work here", true},
		{"phrase still broken", "the login page is still broken", true},
		{"phrase same error", "I get the same error now", true},
		{"phrase you broke", "you broke the login page", true},
		{"phrase fucking", "this fucking test again", true},
		{"phrase fuck", "fuck, the test failed", true},
		{"caps case", "WHY IS THE BUILD FAILING AGAIN", true},
		{"calm prompt", "please run the tests again", false},
		{"too short after normalizing", "wtf", false},
		{"phrase only inside a code fence", "please look at this log:\n```\nsame error\n```", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := []Message{
				{Ordinal: 3, Role: "user", Text: tt.text, Timestamp: kindsBase},
				{Ordinal: 4, Role: "assistant", Text: tt.text, Timestamp: kindsBase},
			}
			got := DetectFrustration(msgs, "s1")
			if !tt.want {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, []Signal{{
				Kind: KindFrustration, SubjectID: "s1", SubjectKind: SubjectSession,
				Detector: DetectorFrustration, Text: tt.text,
				Ordinal: new(3), OccurredAt: kindsBase,
			}}, got, "assistant text never counts")
		})
	}

	t.Run("text truncated to 200 runes", func(t *testing.T) {
		long := "this is broken " + strings.Repeat("é", 300)
		got := DetectFrustration([]Message{{Ordinal: 1, Role: "user", Text: long}}, "s1")
		require.Len(t, got, 1)
		assert.Len(t, []rune(got[0].Text), 200)
	})
}

func TestDetectInterruptions(t *testing.T) {
	tests := []struct {
		name  string
		marks []Message
		want  []Signal
	}{
		{"none", nil, nil},
		{
			"each interruption counts and carries no text",
			[]Message{
				{Ordinal: 6, Role: "user", Text: "[Request interrupted by user]", Timestamp: kindsBase},
				{
					Ordinal: 9, Role: "user", Text: "[Request interrupted by user for tool use]",
					Timestamp: kindsBase.Add(time.Minute),
				},
			},
			[]Signal{
				{
					Kind: KindInterruption, SubjectID: "s1", SubjectKind: SubjectSession,
					Detector: DetectorInterruption, Ordinal: new(6), OccurredAt: kindsBase,
				},
				{
					Kind: KindInterruption, SubjectID: "s1", SubjectKind: SubjectSession,
					Detector: DetectorInterruption, Ordinal: new(9), OccurredAt: kindsBase.Add(time.Minute),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DetectInterruptions(tt.marks, "s1"))
		})
	}
}
