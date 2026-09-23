package friction

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/signals"
)

func patternInput(calls []signals.ToolCallRow, start time.Time) PatternInput {
	in := PatternInput{Calls: calls}
	for i := range calls {
		in.CallTimes = append(in.CallTimes, start.Add(time.Duration(i)*time.Minute))
	}
	return in
}

func TestDetectPatterns(t *testing.T) {
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	ord := func(i int) *int { return &i }

	t.Run("retry loop per run", func(t *testing.T) {
		var calls []signals.ToolCallRow
		for i := range 4 {
			calls = append(calls, signals.ToolCallRow{ToolName: "Bash", Category: "Bash",
				InputJSON: `{"command":"cargo build"}`, MessageOrdinal: 10 + 2*i})
		}
		got := DetectPatterns("s1", false, patternInput(calls, base))
		require.Len(t, got, 1)
		assert.Equal(t, Signal{
			Kind: KindPattern, SubjectID: "s1", SubjectKind: SubjectSession,
			Detector: "pattern.retry_loop",
			Label:    "retry_loop",
			Text:     "retry loop: `Bash` called 4 times with identical arguments",
			Evidence: "`Bash` x4 identical arguments 09:00-09:03",
			Ordinal:  ord(10), OccurredAt: base,
		}, got[0])
	})

	t.Run("retry loop with empty tool name says unknown", func(t *testing.T) {
		calls := []signals.ToolCallRow{{InputJSON: "{}"}, {InputJSON: "{}"}, {InputJSON: "{}"}}
		got := DetectPatterns("s1", false, patternInput(calls, base))
		require.Len(t, got, 1)
		assert.Equal(t, "retry loop: `unknown` called 3 times with identical arguments", got[0].Text)
	})

	t.Run("runaway loop", func(t *testing.T) {
		var calls []signals.ToolCallRow
		for i := range 12 {
			calls = append(calls, signals.ToolCallRow{ToolName: "Bash", Category: "Bash",
				InputJSON:   `{"command":"npm run step-` + string(rune('a'+i)) + `"}`,
				EventStatus: "errored", MessageOrdinal: i + 1})
		}
		got := DetectPatterns("s1", false, patternInput(calls, base))
		require.Len(t, got, 1)
		assert.Equal(t, "pattern.runaway_loop", got[0].Detector)
		assert.Equal(t, "runaway tool loop: 12 tool calls with repeated failures", got[0].Text)
		assert.Equal(t, "12 tool calls 09:00-09:11", got[0].Evidence)
		assert.Equal(t, ord(1), got[0].Ordinal)
	})

	t.Run("edit_churn_uses_base_name_for_posix_and_windows_paths", func(t *testing.T) {
		mk := func(path string, o int) signals.ToolCallRow {
			return signals.ToolCallRow{ToolName: "Edit", Category: "Edit",
				InputJSON:      `{"file_path":"` + path + `","old_string":"` + string(rune('a'+o)) + `"}`,
				MessageOrdinal: o}
		}
		win := `C:\\Users\\user\\app\\main.go`
		calls := []signals.ToolCallRow{
			mk("/home/user/app/internal/db/store.go", 1), mk(win, 2),
			mk("/home/user/app/internal/db/store.go", 3), mk(win, 4),
			mk("/home/user/app/internal/db/store.go", 5), mk(win, 6),
		}
		got := DetectPatterns("s1", false, patternInput(calls, base))
		require.Len(t, got, 2)
		assert.Equal(t, "edit churn: `store.go` edited 3 times within 10 messages", got[0].Text)
		assert.Equal(t, "`store.go` x3 edits 09:00-09:04", got[0].Evidence)
		assert.Equal(t, "edit churn: `main.go` edited 3 times within 10 messages", got[1].Text)
		for _, s := range got {
			assert.NotContains(t, s.Text+s.Evidence, "/home/")
			assert.NotContains(t, s.Text+s.Evidence, `Users`)
		}
	})

	t.Run("mid task compaction", func(t *testing.T) {
		in := PatternInput{
			CompactBoundaries:  []int{7, 20},
			BoundaryTimes:      []time.Time{base.Add(time.Minute), base.Add(8 * time.Minute)},
			MidTaskCompactions: 2,
		}
		got := DetectPatterns("s1", false, in)
		require.Len(t, got, 1)
		assert.Equal(t, "mid-task compaction: 2 compactions during active work", got[0].Text)
		assert.Equal(t, "2 compactions 09:01-09:08", got[0].Evidence)
		assert.Equal(t, ord(7), got[0].Ordinal)
	})

	t.Run("no mid task compaction when count is zero", func(t *testing.T) {
		in := PatternInput{CompactBoundaries: []int{7}, BoundaryTimes: []time.Time{base}}
		assert.Empty(t, DetectPatterns("s1", false, in))
	})

	t.Run("context pressure above threshold only", func(t *testing.T) {
		at := signals.HighContextPressure
		above := 0.934
		assert.Empty(t, DetectPatterns("s1", false, PatternInput{PressureMax: &at}))
		got := DetectPatterns("s1", false, PatternInput{
			PressureMax: &above, PressureAt: base.Add(125 * time.Minute),
		})
		require.Len(t, got, 1)
		assert.Equal(t, "context pressure: peak 93% of the model window", got[0].Text)
		assert.Equal(t, "peak 93% at 11:05", got[0].Evidence)
		assert.Nil(t, got[0].Ordinal)
		assert.Equal(t, base.Add(125*time.Minute), got[0].OccurredAt)
	})

	t.Run("context pressure without a peak time", func(t *testing.T) {
		p := 1.2
		got := DetectPatterns("s1", false, PatternInput{PressureMax: &p})
		require.Len(t, got, 1)
		assert.Equal(t, "peak 120%", got[0].Evidence)
	})

	t.Run("iteration runaway is last and sub-agents are exempt", func(t *testing.T) {
		var calls []signals.ToolCallRow
		for i := range 150 {
			calls = append(calls, signals.ToolCallRow{ToolName: "Read", Category: "Read",
				InputJSON: `{"file_path":"f` + string(rune('0'+i%10)) + `"}`, MessageOrdinal: i + 1})
		}
		p := 0.99
		in := patternInput(calls, base)
		in.PressureMax, in.PressureAt = &p, base
		root := DetectPatterns("s1", false, in)
		require.NotEmpty(t, root)
		assert.Equal(t, "pattern.iteration_runaway", root[len(root)-1].Detector)
		assert.Equal(t, "pattern.context_pressure", root[len(root)-2].Detector)
		for _, s := range DetectPatterns("s1", true, in) {
			assert.NotEqual(t, "pattern.iteration_runaway", s.Detector)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		assert.Empty(t, DetectPatterns("s1", false, PatternInput{}))
	})
}
