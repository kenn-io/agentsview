package friction

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Adapted from jilog detectors.rs tests :670-820 (coding corrections). Fixtures
// carry corrective wording because agentsview gates coding corrections on it.
func TestDetectCorrections(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want []string
	}{
		{
			"corrections_basic_triple",
			[]Message{assistant("first reply"), user("no, you misunderstood the goal here"), assistant("second reply")},
			[]string{"no, you misunderstood the goal here"},
		},
		{
			"corrections_too_short_skipped",
			[]Message{assistant("first"), user("wrong one here"), assistant("second")},
			[]string{},
		},
		{
			"corrections_too_long_skipped",
			[]Message{assistant("a"), user("wrong " + strings.Repeat("x", 195)), assistant("b")},
			[]string{},
		},
		{
			"corrections_exact_minimum_length",
			[]Message{assistant("a"), user("wrong one here."), assistant("b")},
			[]string{"wrong one here."},
		},
		{
			"corrections_exact_maximum_length",
			[]Message{assistant("a"), user("wrong " + strings.Repeat("x", 194)), assistant("b")},
			[]string{"wrong " + strings.Repeat("x", 194)},
		},
		{
			"corrections_wrong_role_pattern_skipped",
			[]Message{user("no, first message in transcript"), user("no, another short user message"), assistant("reply")},
			[]string{},
		},
		{
			"corrections_multiple_in_one_transcript",
			[]Message{assistant("a1"), user("no, first correction please"), assistant("a2"), user("no, second correction please"), assistant("a3")},
			[]string{"no, first correction please", "no, second correction please"},
		},
		{"corrections_empty_transcript/none", nil, []string{}},
		{"corrections_empty_transcript/one", []Message{assistant("a")}, []string{}},
		{"corrections_empty_transcript/two", []Message{assistant("a"), user("hi there friend")}, []string{}},
		{
			"corrections_real_user_string_still_detected",
			[]Message{assistant("first"), user("no, you misunderstood the goal here"), assistant("second")},
			[]string{"no, you misunderstood the goal here"},
		},
		// agentsview additions.
		{"plain_instruction_skipped", []Message{assistant("a"), user("now run the tests"), assistant("b")}, []string{}},
		{
			"length_is_bytes_not_runes",
			// 6 bytes + 65 three-byte runes = 201 bytes: over the raw limit although 71 runes.
			[]Message{assistant("a"), user("wrong " + strings.Repeat("日", 65)), assistant("b")},
			[]string{},
		},
		{
			"trim_is_measured_but_context_is_untrimmed",
			[]Message{assistant("a"), user("   wrong one here.   "), assistant("b")},
			[]string{"   wrong one here.   "},
		},
		{
			"system_role_breaks_the_window",
			[]Message{assistant("a"), {Role: "system", Text: "note"}, user("no, you misunderstood the goal"), assistant("b")},
			[]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectCorrections(numbered(tt.msgs...), "s1")
			assert.Equal(t, tt.want, contexts(got))
			for _, s := range got {
				assert.Equal(t, "s1", s.SubjectID)
				assert.Equal(t, KindCorrection, s.Kind)
				assert.Equal(t, SubjectSession, s.SubjectKind)
				assert.Equal(t, DetectorCorrectionCoding, s.Detector)
			}
		})
	}
}

func TestDetectCorrectionsFields(t *testing.T) {
	ts := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msgs := []Message{
		{Ordinal: 3, Role: "assistant", Text: "a"},
		{Ordinal: 7, Role: "user", Text: "no, you misunderstood the goal here", Timestamp: ts},
		{Ordinal: 8, Role: "assistant", Text: "b"},
	}
	got := DetectCorrections(msgs, "sess")
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Ordinal)
	assert.Equal(t, 7, *got[0].Ordinal)
	assert.Nil(t, got[0].CallIndex)
	assert.Equal(t, ts, got[0].OccurredAt)
	assert.Equal(t, Dims{}, got[0].Dims)
	assert.Zero(t, got[0].Seq)
}

// Ports jilog detectors.rs tests :823-897 (chat corrections).
func TestDetectCorrectionsChat(t *testing.T) {
	window := func(text string) []Message { return []Message{assistant("a"), user(text), assistant("b")} }
	tests := []struct {
		name string
		msgs []Message
		want []string
	}{
		{
			"chat_corrections_require_corrective_language",
			[]Message{assistant("Here is the summary you asked for."), user("thanks, that looks really great"), assistant("Happy to help.")},
			[]string{},
		},
		{"chat_corrections_detect_corrective_markers/no_helper", window("no helper, don't answer in that channel"), []string{"no helper, don't answer in that channel"}},
		{"chat_corrections_detect_corrective_markers/stop_replying", window("stop replying to every message"), []string{"stop replying to every message"}},
		{"chat_corrections_detect_corrective_markers/thats_not", window("that's not what the group decided"), []string{"that's not what the group decided"}},
		{"chat_corrections_detect_corrective_markers/wrong_group", window("wrong group — that was for the steering committee"), []string{"wrong group — that was for the steering committee"}},
		{"chat_corrections_detect_corrective_markers/should_never", window("you should never post invoices here"), []string{"you should never post invoices here"}},
		{"chat_corrections_ignore_conversational_marker_lookalikes/stop_by", window("let's stop by the cafe after the session"), []string{}},
		{"chat_corrections_ignore_conversational_marker_lookalikes/never_been", window("I've never been to that part of town"), []string{}},
		{"chat_corrections_ignore_conversational_marker_lookalikes/instead", window("let's meet on Zoom instead of in person"), []string{}},
		{"chat_corrections_ignore_conversational_marker_lookalikes/actually", window("actually, that sounds great to me"), []string{}},
		{
			"chat_corrections_anchor_matches_after_leading_whitespace",
			window("  no, that message was for the other group"),
			[]string{"  no, that message was for the other group"},
		},
		{
			"chat_corrections_keep_length_window_and_tool_result_filter/tool_echo",
			[]Message{assistant("a"), toolResultUser("<tool_use_error>don't do that, wrong file</tool_use_error>"), assistant("b")},
			[]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectCorrectionsChat(numbered(tt.msgs...), "s1")
			assert.Equal(t, tt.want, contexts(got))
			for _, s := range got {
				assert.Equal(t, DetectorCorrectionChat, s.Detector)
			}
		})
	}
}

func TestDetectCorrectionsChatMarkers(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"no prefix", "no, choose the other draft"},
		{"don't", "don't publish this summary"},
		{"do not", "do not publish this summary"},
		{"please stop", "please stop right now"},
		{"stop action", "stop adding unrelated lines"},
		{"wrong", "that is wrong for this group"},
		{"incorrect", "this is incorrect for the group"},
		{"not right", "that is not right for us"},
		{"should never", "you should never post here"},
		{"that's not", "that's not our meeting"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectCorrectionsChat([]Message{assistant("a"), user(tt.text), assistant("b")}, "s1")
			require.Len(t, got, 1)
			assert.Equal(t, tt.text, got[0].Text)
		})
	}
}

func TestFirstMatch(t *testing.T) {
	patterns := compileAll([]string{"alpha", "beta"})
	tests := []struct {
		name      string
		text      string
		wantIndex int
		wantMatch bool
	}{
		{"declaration order wins over text position", "beta then alpha", 0, true},
		{"second pattern alone", "beta only", 1, true},
		{"no match", "gamma only", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index, matched := firstMatch(patterns, tt.text)
			assert.Equal(t, tt.wantIndex, index)
			assert.Equal(t, tt.wantMatch, matched)
		})
	}
}

// TestChatCorrectionWordBoundary pins the accepted RE2 difference (spec
// §6, Open question 8): Go's \b is ASCII-only, so a non-ASCII letter next
// to a marker still counts as a boundary. Rust regex 1.12.3 returns false
// for all three texts (captured with a throwaway Rust program).
func TestChatCorrectionWordBoundary(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		goWant bool
	}{
		{"accented letter after wrong", "that is so wrongé today", true},
		{"accented letter before wrong", "that is éwrong today now", true},
		{"accented letter before don't", "please Ödon't do it again", true},
		{"underscore is a word char in both", "that is wrong_ today now", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectCorrectionsChat([]Message{assistant("a"), user(tt.text), assistant("b")}, "s1")
			assert.Equal(t, tt.goWant, len(got) == 1)
		})
	}
}
