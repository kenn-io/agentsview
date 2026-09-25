package friction

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Adapts jilog detectors.rs tests :899-968 (errors) and the bash cases of
// :970-1209 (the expected-noise allowlist, jilog#42fd) to plain-text results.
func TestDetectErrors(t *testing.T) {
	const timeout30 = "Command timed out after 30 seconds"
	tests := []struct {
		name     string
		msg      Message
		wantMsg  string
		wantTool string
		emitted  bool
	}{
		{"failed call detected", failedTool("Edit", "old_string not found"), "old_string not found", "Edit", true},
		{"successful call skipped", tool("Edit", "old_string not found"), "", "", false},
		{"non-tool role skipped", Message{Role: "user", Text: "boom", Failed: true}, "", "", false},
		{"empty tool name falls back", Message{Role: "tool", Text: "x", Failed: true}, "x", "unknown", true},
		{"non-bash empty text kept", failedTool("Edit", ""), "", "Edit", true},
		{"bash empty text skipped", failedTool("bash", ""), "", "", false},
		{"bash whitespace skipped", failedTool("bash", " \n\t"), "", "", false},
		{"bash bare timeout skipped", failedTool("bash", timeout30), "", "", false},
		{"bash singular timeout skipped", failedTool("bash", "command timed out after 1 second."), "", "", false},
		{"bash padded timeout skipped", failedTool("bash", "  "+timeout30+"\n"), "", "", false},
		{"bash timeout with output kept", failedTool("bash", timeout30+"\npartial build log"), timeout30 + "\npartial build log", "bash", true},
		{"bash timeout with extra words kept", failedTool("bash", timeout30+" while running cargo"), timeout30 + " while running cargo", "bash", true},
		{"bash stderr kept", failedTool("bash", "deploy-tool: status check failed"), "deploy-tool: status check failed", "bash", true},
		{"bare timeout from other tool kept", failedTool("python_check", timeout30), timeout30, "python_check", true},
		{"noise match is case-sensitive on tool", failedTool("Bash", ""), "", "Bash", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectErrors([]Message{tt.msg}, "s1")
			if !tt.emitted {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, tt.wantTool, got[0].ToolName)
			assert.Equal(t, tt.wantMsg, got[0].Text)
			assert.Equal(t, KindError, got[0].Kind)
			assert.Equal(t, SubjectSession, got[0].SubjectKind)
			assert.Equal(t, DetectorError, got[0].Detector)
		})
	}
}

func TestDetectErrorsFields(t *testing.T) {
	ts := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	got := DetectErrors([]Message{{
		Ordinal: 12, CallIndex: 2, Role: "tool", ToolName: "Bash",
		Text: "boom", Failed: true, Timestamp: ts,
	}}, "sess")
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Ordinal)
	require.NotNil(t, got[0].CallIndex)
	assert.Equal(t, 12, *got[0].Ordinal)
	assert.Equal(t, 2, *got[0].CallIndex)
	assert.Equal(t, "sess", got[0].SubjectID)
	assert.Equal(t, ts, got[0].OccurredAt)
	assert.Equal(t, "boom", got[0].Text)
}

// NoiseName selects the noise-rule key while the displayed tool name is
// kept.
func TestDetectErrorsNoiseName(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		noiseName string
		emitted   bool
	}{
		{"Bash category maps to bash", "Bash", "bash", false},
		{"no noise name keeps exact match", "Bash", "", true},
		{"codex shell mapped to bash", "exec_command", "bash", false},
		{"non-bash noise name", "Bash", "python_check", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := failedTool(tt.toolName, "")
			msg.NoiseName = tt.noiseName
			got := DetectErrors([]Message{msg}, "s1")
			if !tt.emitted {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, tt.toolName, got[0].ToolName)
		})
	}
}
