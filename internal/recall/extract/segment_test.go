package extract

import (
	"encoding/json/v2"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type goldenFixture struct {
	MaxWindowChars int `json:"max_window_chars"`
	Messages       []struct {
		Ordinal  int    `json:"ordinal"`
		Role     string `json:"role"`
		Content  string `json:"content"`
		IsSystem int    `json:"is_system"`
	} `json:"messages"`
	Units []struct {
		Kind         string `json:"kind"`
		Text         string `json:"text"`
		OrdinalStart int    `json:"ordinal_start"`
		OrdinalEnd   int    `json:"ordinal_end"`
	} `json:"units"`
}

// TestTurnsV1GoldenParity asserts that the segmenter reproduces the pinned
// golden units exactly. Resume cursors and entry identity both depend on this
// determinism, so any divergence here is a correctness bug, not a style
// choice.
func TestTurnsV1GoldenParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/turnsv1_golden.json")
	if err != nil {
		require.FailNowf(t, "test failed", "reading golden fixtures: %v", err)
	}
	var fixtures map[string]goldenFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		require.FailNowf(t, "test failed", "parsing golden fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		require.FailNow(t, "no fixtures found")
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			segmenter := TurnsV1{MaxWindowChars: fixture.MaxWindowChars}
			messages := make([]Message, 0, len(fixture.Messages))
			for _, m := range fixture.Messages {
				messages = append(messages, Message{
					Ordinal:  m.Ordinal,
					Role:     m.Role,
					Content:  m.Content,
					IsSystem: m.IsSystem != 0,
				})
			}
			units := segmenter.Units(messages)
			if len(units) != len(fixture.Units) {
				require.FailNowf(t, "test failed", "unit count = %d, want %d", len(units), len(fixture.Units))
			}
			for i, want := range fixture.Units {
				got := units[i]
				if string(got.Role) != roleForKind(t, want.Kind) {
					assert.Failf(t, "test failed", "unit %d role = %q, want kind %q", i, got.Role, want.Kind)
				}
				if got.Text != want.Text {
					assert.Failf(t, "test failed", "unit %d text mismatch:\ngot:  %q\nwant: %q", i, got.Text, want.Text)
				}
				if got.OrdinalStart != want.OrdinalStart || got.OrdinalEnd != want.OrdinalEnd {
					assert.Failf(t, "test failed", "unit %d ordinals = (%d,%d), want (%d,%d)",
						i, got.OrdinalStart, got.OrdinalEnd, want.OrdinalStart, want.OrdinalEnd)
				}
			}
		})
	}
}

func roleForKind(t *testing.T, kind string) string {
	t.Helper()
	switch kind {
	case "intent":
		return string(RoleIntent)
	case "action_run":
		return string(RoleAction)
	default:
		require.FailNowf(t, "test failed", "unknown fixture unit kind %q", kind)
		return ""
	}
}

func TestTurnsV1Identity(t *testing.T) {
	segmenter := TurnsV1{MaxWindowChars: 50000}
	assert.Equal(t, "turns-v1", segmenter.Name())
	params := segmenter.Params()
	assert.Equal(t, map[string]any{"max_window_chars": 50000, "tool_use_version": 1}, params)
	assert.Equal(t, params, segmenter.Params())
}

func TestTurnsV1PromptRoles(t *testing.T) {
	roles := TurnsV1{MaxWindowChars: 50000}.PromptRoles()
	if len(roles) != 2 || roles[0] != RoleIntent || roles[1] != RoleAction {
		assert.Failf(t, "test failed", "PromptRoles() = %v, want [intent action]", roles)
	}
}

func TestTurnsV1EmptySession(t *testing.T) {
	units := TurnsV1{MaxWindowChars: 50000}.Units(nil)
	if len(units) != 0 {
		assert.Failf(t, "test failed", "Units(nil) = %d units, want 0", len(units))
	}
}

// TestTurnsV1SplitsActionRunsAtOrdinalGaps pins that a run of assistant
// messages never packs across a missing ordinal. Ingest filtering can drop
// rows after ordinals are assigned (e.g. tool-result-only user messages), and
// evidence provenance requires gap-free transcript ranges — a unit spanning
// the hole would fail verification on every commit attempt.
func TestTurnsV1SplitsActionRunsAtOrdinalGaps(t *testing.T) {
	units := TurnsV1{MaxWindowChars: 50000}.Units([]Message{
		{Ordinal: 0, Role: "user", Content: "fix the bug"},
		{Ordinal: 1, Role: "assistant", Content: "first step"},
		{Ordinal: 3, Role: "assistant", Content: "second step"},
	})
	if len(units) != 3 {
		require.FailNowf(t, "test failed", "unit count = %d, want 3 (intent + one action unit per "+
			"side of the gap)", len(units))
	}
	first, second := units[1], units[2]
	if first.Role != RoleAction || first.OrdinalStart != 1 || first.OrdinalEnd != 1 {
		assert.Failf(t, "test failed", "unit 1 = %s (%d,%d), want action (1,1)",
			first.Role, first.OrdinalStart, first.OrdinalEnd)
	}
	if second.Role != RoleAction || second.OrdinalStart != 3 || second.OrdinalEnd != 3 {
		assert.Failf(t, "test failed", "unit 2 = %s (%d,%d), want action (3,3)",
			second.Role, second.OrdinalStart, second.OrdinalEnd)
	}
}

// TestTurnsV1PacksRunsAcrossSkippedRows pins the complement: system and
// empty rows are skipped from unit text but still occupy their ordinals in
// the stored transcript, so a run packed across them stays verifiable and
// must not be split.
func TestTurnsV1PacksRunsAcrossSkippedRows(t *testing.T) {
	units := TurnsV1{MaxWindowChars: 50000}.Units([]Message{
		{Ordinal: 0, Role: "assistant", Content: "a"},
		{Ordinal: 1, Role: "assistant", Content: "   "},
		{Ordinal: 2, Role: "user", Content: "sys note", IsSystem: true},
		{Ordinal: 3, Role: "assistant", Content: "b"},
	})
	if len(units) != 1 {
		require.FailNowf(t, "test failed", "unit count = %d, want 1 (skipped rows keep the run "+
			"contiguous)", len(units))
	}
	if units[0].OrdinalStart != 0 || units[0].OrdinalEnd != 3 {
		assert.Failf(t, "test failed", "unit range = (%d,%d), want (0,3)",
			units[0].OrdinalStart, units[0].OrdinalEnd)
	}
}

// TestTurnsV1ToolUse pins which units carry execution evidence. Only flags
// change: text, roles, and ordinal ranges must match the same transcript
// with every ToolUse cleared.
func TestTurnsV1ToolUse(t *testing.T) {
	cases := []struct {
		name     string
		messages []Message
		want     []bool
	}{
		{
			name: "assistant row with a tool call",
			messages: []Message{
				{Ordinal: 0, Role: "assistant", Content: "ran it", ToolUse: true},
			},
			want: []bool{true},
		},
		{
			name: "no tool anywhere",
			messages: []Message{
				{Ordinal: 0, Role: "user", Content: "set up CI"},
				{Ordinal: 1, Role: "assistant", Content: "I suggest a workflow"},
			},
			want: []bool{false, false},
		},
		{
			name: "tool-only row after a block",
			messages: []Message{
				{Ordinal: 0, Role: "assistant", Content: "checking"},
				{Ordinal: 1, Role: "assistant", Content: "", ToolUse: true},
			},
			want: []bool{true},
		},
		{
			name: "tool-only row before a block",
			messages: []Message{
				{Ordinal: 0, Role: "user", Content: "fix it"},
				{Ordinal: 1, Role: "assistant", Content: " ", ToolUse: true},
				{Ordinal: 2, Role: "assistant", Content: "fixed"},
			},
			want: []bool{false, true},
		},
		{
			name: "tool-only row alone before a user message",
			messages: []Message{
				{Ordinal: 0, Role: "assistant", Content: "", ToolUse: true},
				{Ordinal: 1, Role: "user", Content: "thanks"},
				{Ordinal: 2, Role: "assistant", Content: "I suggest more tests"},
			},
			want: []bool{false, false},
		},
		{
			name: "tool-only row across an ordinal gap",
			messages: []Message{
				{Ordinal: 0, Role: "assistant", Content: "", ToolUse: true},
				{Ordinal: 2, Role: "assistant", Content: "I suggest more tests"},
			},
			want: []bool{false},
		},
		{
			name: "tool-only row ending a run before a gap",
			messages: []Message{
				{Ordinal: 0, Role: "assistant", Content: "checking"},
				{Ordinal: 1, Role: "assistant", Content: "", ToolUse: true},
				{Ordinal: 3, Role: "assistant", Content: "I suggest more tests"},
			},
			want: []bool{true, false},
		},
		{
			name: "tool-role row marks the preceding block",
			messages: []Message{
				{Ordinal: 0, Role: "assistant", Content: "running tests"},
				{Ordinal: 1, Role: "tool", Content: "ok", ToolUse: true},
			},
			want: []bool{true},
		},
		{
			name: "system row without evidence marks nothing",
			messages: []Message{
				{Ordinal: 0, Role: "user", Content: "note", IsSystem: true},
				{Ordinal: 1, Role: "assistant", Content: "I suggest more tests"},
			},
			want: []bool{false},
		},
		{
			name: "system tool-result row inside a run marks it without flushing",
			messages: []Message{
				{Ordinal: 0, Role: "assistant", Content: "I will run the tests."},
				{Ordinal: 1, Role: "user", Content: "ok", IsSystem: true, ToolUse: true},
				{Ordinal: 2, Role: "assistant", Content: "Tests pass."},
			},
			want: []bool{true},
		},
		{
			name: "visible user tool result hands evidence to the next block",
			messages: []Message{
				{Ordinal: 0, Role: "assistant", Content: "writing config", ToolUse: true},
				{Ordinal: 1, Role: "user", Content: "[1 tool result(s)]", ToolUse: true},
				{Ordinal: 2, Role: "assistant", Content: "Done, I added the config."},
			},
			want: []bool{true, false, true},
		},
		{
			name: "ordinary user message clears pending evidence",
			messages: []Message{
				{Ordinal: 0, Role: "assistant", Content: "writing config", ToolUse: true},
				{Ordinal: 1, Role: "user", Content: "thanks"},
				{Ordinal: 2, Role: "assistant", Content: "Done, I added the config."},
			},
			want: []bool{true, false, false},
		},
	}
	segmenter := TurnsV1{MaxWindowChars: 50000}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			units := segmenter.Units(tc.messages)
			got := make([]bool, 0, len(units))
			for _, unit := range units {
				got = append(got, unit.ToolUse)
			}
			assert.Equal(t, tc.want, got)

			stripped := make([]Message, len(tc.messages))
			copy(stripped, tc.messages)
			for i := range stripped {
				stripped[i].ToolUse = false
			}
			plain := segmenter.Units(stripped)
			require.Len(t, units, len(plain))
			for i := range units {
				unit := units[i]
				unit.ToolUse = false
				assert.Equal(t, plain[i], unit, "unit %d", i)
			}
		})
	}
}

func TestTurnsV1ToolUseIsWindowLocal(t *testing.T) {
	long := strings.Repeat("x", 40)
	cases := []struct {
		name     string
		contents []string
		tools    []bool
		budget   int
		want     []Unit
	}{
		{
			name: "tool then oversized proposal", contents: []string{"tool", long}, tools: []bool{true, false}, budget: 40,
			want: []Unit{
				{Role: RoleAction, Text: "[0] ASSISTANT:\ntool", OrdinalStart: 0, OrdinalEnd: 0, ToolUse: true},
				{Role: RoleAction, Text: "[1] ASSISTANT:\n" + long, OrdinalStart: 1, OrdinalEnd: 1},
			},
		},
		{
			name: "tool then ordinary packed proposal", contents: []string{long, "idea", "plan"}, tools: []bool{true, false, false}, budget: 60,
			want: []Unit{
				{Role: RoleAction, Text: "[0] ASSISTANT:\n" + long, OrdinalStart: 0, OrdinalEnd: 0, ToolUse: true},
				{Role: RoleAction, Text: "[1] ASSISTANT:\nidea\n\n[2] ASSISTANT:\nplan", OrdinalStart: 1, OrdinalEnd: 2},
			},
		},
		{
			name: "proposal then tool", contents: []string{long, long}, tools: []bool{false, true}, budget: 60,
			want: []Unit{
				{Role: RoleAction, Text: "[0] ASSISTANT:\n" + long, OrdinalStart: 0, OrdinalEnd: 0},
				{Role: RoleAction, Text: "[1] ASSISTANT:\n" + long, OrdinalStart: 1, OrdinalEnd: 1, ToolUse: true},
			},
		},
		{
			name: "tool then proposal then tool", contents: []string{long, long, long}, tools: []bool{true, false, true}, budget: 60,
			want: []Unit{
				{Role: RoleAction, Text: "[0] ASSISTANT:\n" + long, OrdinalStart: 0, OrdinalEnd: 0, ToolUse: true},
				{Role: RoleAction, Text: "[1] ASSISTANT:\n" + long, OrdinalStart: 1, OrdinalEnd: 1},
				{Role: RoleAction, Text: "[2] ASSISTANT:\n" + long, OrdinalStart: 2, OrdinalEnd: 2, ToolUse: true},
			},
		},
		{
			name: "hidden tool row between split blocks", contents: []string{long, "", long},
			tools: []bool{false, true, false}, budget: 60,
			want: []Unit{
				{Role: RoleAction, Text: "[0] ASSISTANT:\n" + long, OrdinalStart: 0, OrdinalEnd: 0, ToolUse: true},
				{Role: RoleAction, Text: "[2] ASSISTANT:\n" + long, OrdinalStart: 2, OrdinalEnd: 2, ToolUse: true},
			},
		},
		{
			name: "two tool blocks in one window", contents: []string{"a", "b"}, tools: []bool{true, true}, budget: 60,
			want: []Unit{
				{Role: RoleAction, Text: "[0] ASSISTANT:\na\n\n[1] ASSISTANT:\nb", OrdinalStart: 0, OrdinalEnd: 1, ToolUse: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			messages := make([]Message, 0, len(tc.tools))
			for i, tool := range tc.tools {
				messages = append(messages, Message{
					Ordinal: i, Role: "assistant", Content: tc.contents[i], ToolUse: tool,
				})
			}
			units := TurnsV1{MaxWindowChars: tc.budget}.Units(messages)
			assert.Equal(t, tc.want, units)
		})
	}
}
