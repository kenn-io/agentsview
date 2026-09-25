package friction

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Message.Text holds the assistant text extracted from content blocks. The
// tool-only examples pass the resulting text, rather than tool input.
func TestDetectWorkarounds(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want []string
	}{
		{"for_now", []Message{assistant("Using a hardcoded value for now until config is wired.")}, []string{"for now"}},
		{"one_per_message", []Message{assistant("TODO: refactor this hack workaround later.")}, []string{"workaround"}},
		{"lowest_pattern_index_not_text_position", []Message{assistant("hack temporary")}, []string{"temporary"}},
		{"hardcoded", []Message{assistant("A hardcoded value remains.")}, []string{"hardcoded"}},
		{"skip_tool_use_blocks", []Message{assistant("I ran the command.")}, []string{}},
		{"text_blocks_in_list", []Message{assistant("Quick fix incoming.")}, []string{"quick fix"}},
		{"case_insensitive", []Message{assistant("HACK: this is gross.")}, []string{"hack"}},
		{"no_match", []Message{assistant("All systems nominal. Tests passing.")}, []string{}},
		{"user_role_skipped", []Message{user("TODO this is from the user, should not match")}, []string{}},
		{"no_word_boundaries", []Message{assistant("the hackathon was temporarily moved")}, []string{"hack"}},
		{"todo_in_ordinary_prose", []Message{assistant("All data collected. Let me update the todo list and finalize")}, []string{"TODO"}},
		{"empty_text_skipped", []Message{assistant("")}, []string{}},
		{"tool_role_skipped", []Message{tool("bash", "for now")}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectWorkarounds(numbered(tt.msgs...), "s1")
			assert.Equal(t, tt.want, labels(got))
			for _, s := range got {
				assert.Equal(t, KindWorkaround, s.Kind)
				assert.Equal(t, SubjectSession, s.SubjectKind)
				assert.Equal(t, DetectorWorkaround, s.Detector)
				assert.Equal(t, "s1", s.SubjectID)
			}
		})
	}
}

func TestDetectWorkaroundsContext(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"ascii", "FIXME " + strings.Repeat("x", 300)},
		{"multibyte_counts_runes", "FIXME " + strings.Repeat("日", 300)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectWorkarounds([]Message{assistant(tt.text)}, "s1")
			require.Len(t, got, 1)
			assert.Equal(t, 200, utf8.RuneCountInString(got[0].Text))
			assert.True(t, strings.HasPrefix(tt.text, got[0].Text))
		})
	}
	short := DetectWorkarounds([]Message{assistant("for now")}, "s1")
	require.Len(t, short, 1)
	assert.Equal(t, "for now", short[0].Text)
}

func TestDetectDeferrals(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want []string
	}{
		{"come_back", []Message{assistant("I'll come back to this after the tests pass.")}, []string{"come back later"}},
		{"short_text", []Message{assistant("next session")}, []string{"next session"}},
		{"first_match_wins", []Message{assistant("I'll come back to this next session.")}, []string{"come back later"}},
		{"user_role_skipped", []Message{user("please punt on this until next session")}, []string{}},
		{"tool_blocks_excluded", []Message{assistant("I ran the command.")}, []string{}},
		{"no_match", []Message{assistant("All requested work is complete.")}, []string{}},
		{"defer", []Message{assistant("Let me defer that until the next pass.")}, []string{"defer"}},
		{"deferring", []Message{assistant("Deferring this until review.")}, []string{"deferring"}},
		{"punt", []Message{assistant("I'm punting on that one.")}, []string{"punt"}},
		{"leave_for_later", []Message{assistant("Leaving it for later.")}, []string{"leave for later"}},
		{"park_for_now", []Message{assistant("Parking this for now.")}, []string{"park for now"}},
		{"circle_back", []Message{assistant("We can circle back on the naming.")}, []string{"circle back"}},
		{"skipping_for_now", []Message{assistant("Skipping for now.")}, []string{"skipping for now"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectDeferrals(numbered(tt.msgs...), "s1")
			assert.Equal(t, tt.want, labels(got))
			for _, s := range got {
				assert.Equal(t, KindDeferral, s.Kind)
				assert.Equal(t, SubjectSession, s.SubjectKind)
				assert.Equal(t, DetectorDeferral, s.Detector)
				assert.Equal(t, "s1", s.SubjectID)
				assert.Empty(t, s.Text)
			}
		})
	}
}

func TestTextDetectorsIndependentAndPreserveMessageIdentity(t *testing.T) {
	when := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	msgs := []Message{
		{Ordinal: 7, Role: "assistant", Text: "Skipping for now while we validate the migration.", Timestamp: when},
		{Ordinal: 8, Role: "assistant", Text: "FIXME before the next session.", Timestamp: when.Add(time.Minute)},
	}
	workarounds := DetectWorkarounds(msgs, "s1")
	deferrals := DetectDeferrals(msgs, "s1")
	require.Len(t, workarounds, 2)
	require.Len(t, deferrals, 2)
	assert.Equal(t, []string{"for now", "FIXME"}, labels(workarounds))
	assert.Equal(t, []string{"skipping for now", "next session"}, labels(deferrals))
	for _, signals := range [][]Signal{workarounds, deferrals} {
		for i, s := range signals {
			require.NotNil(t, s.Ordinal)
			assert.Equal(t, msgs[i].Ordinal, *s.Ordinal)
			assert.Equal(t, msgs[i].Timestamp, s.OccurredAt)
			assert.Equal(t, "s1", s.SubjectID)
		}
	}
}
