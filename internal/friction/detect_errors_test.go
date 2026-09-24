package friction

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports jilog detectors.rs tests :899-968 (errors) and :970-1209 (the
// expected-noise allowlist, jilog#42fd). Envelopes are the jilog json!
// literals serialized with sorted keys, as jilog's tool() helper does.
func TestDetectErrors(t *testing.T) {
	tests := []struct {
		name     string
		msg      Message
		wantN    int
		wantMsg  string // exact message when non-empty
		wantTool string
	}{
		{"errors_success_false_detected", tool("bash", `{"error":"boom","success":false}`), 1, "boom", "bash"},
		{"errors_success_true_skipped", tool("bash", `{"output":"ok","success":true}`), 0, "", ""},
		{"errors_not_tool_role_skipped", Message{Role: "user", Text: `{"success":false}`}, 0, "", ""},
		{"errors_invalid_json_content_skipped", tool("bash", `not valid json {`), 0, "", ""},
		{"errors_error_as_list_takes_first", tool("validator", `{"error":["first issue","second issue"],"success":false}`), 1, "first issue", "validator"},
		{"errors_error_null_falls_back_to_data", tool("weird", `{"error":null,"info":"see logs","success":false}`), 1, `{"error":null,"info":"see logs","success":false}`, "weird"},
		{"whole_blob_keeps_big_ints_and_serde_floats", tool("validator", `{"success":false,"info":{"negative_zero":-0,"html":"<tag>","float":1.50,"big":9007199254740993},"error":null}`), 1, `{"error":null,"info":{"big":9007199254740993,"float":1.5,"html":"<tag>","negative_zero":-0.0},"success":false}`, "validator"},
		{"errors_no_tool_name_falls_back", Message{Role: "tool", Text: `{"error":"x","success":false}`}, 1, "x", "unknown"},
		// Strictness of success (detectors.rs:222-225).
		{"success_string_false_skipped", tool("bash", `{"error":"x","success":"false"}`), 0, "", ""},
		{"success_missing_skipped", tool("bash", `{"error":"x"}`), 0, "", ""},
		{"success_zero_skipped", tool("bash", `{"error":"x","success":0}`), 0, "", ""},
		{"non_object_json_skipped", tool("bash", `[false]`), 0, "", ""},
		{"empty_text_skipped", tool("bash", ``), 0, "", ""},
		// Message extraction shapes (detectors.rs:252-269).
		{"error_object_is_compact_sorted_json", tool("mode", `{"error":{"message":"no such mode","code":"invalid_transition"},"success":false}`), 1, `{"code":"invalid_transition","message":"no such mode"}`, "mode"},
		{"error_number_is_serialized", tool("x", `{"error":7,"success":false}`), 1, "7", "x"},
		{"error_empty_list_falls_back", tool("x", `{"error":[],"success":false}`), 1, `{"error":[],"success":false}`, "x"},
		{"error_list_first_object", tool("x", `{"error":[{"b":1,"a":"<"}],"success":false}`), 1, `{"a":"<","b":1}`, "x"},
		{
			"golden_fixture_whole_blob", tool("bash", `{"error":null,"output":{"returncode":101,"stderr":"error[E0308]: mismatched types","stdout":""},"success":false}`), 1,
			`{"error":null,"output":{"returncode":101,"stderr":"error[E0308]: mismatched types","stdout":""},"success":false}`, "bash",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectErrors([]Message{tt.msg}, "s1")
			require.Len(t, got, tt.wantN)
			if tt.wantN == 0 {
				return
			}
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
		Text: `{"error":"boom","success":false}`, Timestamp: ts,
	}}, "sess")
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Ordinal)
	require.NotNil(t, got[0].CallIndex)
	assert.Equal(t, 12, *got[0].Ordinal)
	assert.Equal(t, 2, *got[0].CallIndex)
	assert.Equal(t, "sess", got[0].SubjectID)
	assert.Equal(t, ts, got[0].OccurredAt)
	assert.Equal(t, "Bash", got[0].ToolName)
}

// NoiseName selects the allowlist key while the displayed tool name is
// kept (spec §6.3, D12).
func TestDetectErrorsNoiseName(t *testing.T) {
	bare := `{"error":null,"output":{"returncode":1,"stderr":"","stdout":""},"success":false}`
	tests := []struct {
		name      string
		toolName  string
		noiseName string
		wantN     int
	}{
		{"agentsview Bash category maps to bash", "Bash", "bash", 0},
		{"no noise name keeps jilog exact match", "Bash", "", 1},
		{"codex shell mapped to bash", "exec_command", "bash", 0},
		{"non-bash noise name", "Bash", "python_check", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectErrors([]Message{{Role: "tool", ToolName: tt.toolName, NoiseName: tt.noiseName, Text: bare}}, "s1")
			require.Len(t, got, tt.wantN)
			if tt.wantN == 1 {
				assert.Equal(t, tt.toolName, got[0].ToolName)
			}
		})
	}
}

// Every pinned case in jilog-inventory §2.4, grouped by jilog test name.
func TestDetectErrorsNoise(t *testing.T) {
	const timeout30 = "Command timed out after 30 seconds"
	tests := []struct {
		name     string
		tool     string
		envelope string
		emitted  bool
	}{
		{"errors_skip_mode_denial_status", "mode", `{"error":null,"output":{"status":"denied","user_instruction":"Inform the user: I'd like to clear the current mode"},"success":false}`, false},
		{"errors_skip_mode_denied_mode", "mode", `{"error":null,"output":{"denied_mode":"debug","status":"denied"},"success":false}`, false},
		{"errors_skip_mode_clear_denied_code", "mode", `{"error":{"code":"clear_denied","message":"Cannot clear mode while in 'debug'."},"success":false}`, false},
		{"errors_skip_mode_flat_denied_code", "mode", `{"code":"switch_denied","success":false}`, false},
		{"errors_skip_mode_denied_mode_null_value", "mode", `{"output":{"denied_mode":null},"success":false}`, false},
		{"errors_keep_mode_non_denial", "mode", `{"error":{"code":"invalid_transition","message":"no such mode"},"success":false}`, true},
		{"errors_keep_denied_status_from_other_tool", "delegate", `{"error":null,"output":{"status":"denied"},"success":false}`, true},
		{"errors_skip_bash_bare_timeout/object", "bash", `{"error":{"message":"` + timeout30 + `"},"success":false}`, false},
		{"errors_skip_bash_bare_timeout/string", "bash", `{"error":"Command timed out after 120 seconds.","success":false}`, false},
		{"errors_skip_bash_bare_timeout/top_message", "bash", `{"message":"Command timed out after 25 seconds","success":false}`, false},
		{"errors_skip_bash_bare_nonzero_exit/empty", "bash", `{"error":null,"output":{"returncode":1,"stderr":"","stdout":""},"success":false}`, false},
		{"errors_skip_bash_bare_nonzero_exit/whitespace", "bash", `{"error":null,"output":{"returncode":2,"stderr":" \n","stdout":"\n"},"success":false}`, false},
		{"errors_skip_bash_bare_nonzero_exit/absent_streams", "bash", `{"output":{"returncode":127},"success":false}`, false},
		{"errors_keep_bash_banner_stdout", "bash", `{"error":null,"output":{"returncode":1,"stderr":"","stdout":"=== today's health report ===\n"},"success":false}`, true},
		{"errors_keep_bash_stderr", "bash", `{"error":null,"output":{"returncode":1,"stderr":"deploy-tool: status check failed","stdout":""},"success":false}`, true},
		{"errors_keep_bash_marker_free_stdout", "bash", `{"error":null,"output":{"returncode":1,"stderr":"","stdout":"checksum mismatch: expected X, got Y"},"success":false}`, true},
		{"errors_keep_bash_structured_stdout_and_stderr/object_stdout", "bash", `{"error":null,"output":{"returncode":1,"stderr":"","stdout":{"failed":["a"]}},"success":false}`, true},
		{"errors_keep_bash_structured_stdout_and_stderr/array_stderr", "bash", `{"error":null,"output":{"returncode":1,"stderr":["boom"],"stdout":""},"success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/in_output", "bash", `{"output":{"message":"disk full","returncode":1},"success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/beside_timeout", "bash", `{"error":{"message":"` + timeout30 + `","partial":"tail of log"},"success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/top_level", "bash", `{"diagnostics":"oom","error":null,"output":{"returncode":1,"stderr":"","stdout":""},"success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/timeout_top", "bash", `{"error":"` + timeout30 + `","stderr":"killed","success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/msg_beside_error", "bash", `{"error":"` + timeout30 + `","message":"Traceback (most recent call last): ...","success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/msg_beside_error_obj", "bash", `{"error":{"message":"` + timeout30 + `"},"message":"disk full","success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/msg_struct", "bash", `{"error":"` + timeout30 + `","message":{"detail":"disk full"},"success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/rc_text", "bash", `{"error":{"message":"` + timeout30 + `"},"output":{"returncode":"see log: disk full","stderr":"","stdout":""},"success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/rc_obj", "bash", `{"error":{"message":"` + timeout30 + `"},"output":{"returncode":{"signal":"SIGKILL"}},"success":false}`, true},
		{"errors_keep_bash_extra_fields_anywhere/rc_null", "bash", `{"error":{"message":"` + timeout30 + `"},"output":{"returncode":null,"stderr":"","stdout":""},"success":false}`, true},
		{"errors_skip_bash_production_timeout_envelope/real", "bash", `{"error":{"message":"` + timeout30 + `"},"output":"` + timeout30 + `","success":false}`, false},
		{"errors_skip_bash_production_timeout_envelope/empty", "bash", `{"error":"` + timeout30 + `","output":"","success":false}`, false},
		{"errors_skip_bash_production_timeout_envelope/whitespace", "bash", `{"error":"` + timeout30 + `","output":"  \n","success":false}`, false},
		{"errors_skip_bash_production_timeout_envelope/padded", "bash", `{"error":{"message":"` + timeout30 + `"},"output":" ` + timeout30 + `\n","success":false}`, false},
		{"errors_keep_bash_non_object_output_with_content/string", "bash", `{"error":"` + timeout30 + `","output":"partial build log","success":false}`, true},
		{"errors_keep_bash_non_object_output_with_content/other_sentence", "bash", `{"error":"` + timeout30 + `","output":"` + timeout30 + `\nTraceback (most recent call last):","success":false}`, true},
		{"errors_keep_bash_non_object_output_with_content/array", "bash", `{"output":["returncode",1],"success":false}`, true},
		{"errors_keep_bash_non_object_output_with_content/number", "bash", `{"error":"` + timeout30 + `","output":1,"success":false}`, true},
		{"errors_keep_bash_non_object_output_with_content/exit_status_string", "bash", `{"output":"exit status 1","success":false}`, true},
		{"errors_keep_bash_structured_error/object", "bash", `{"error":{"code":"E1"},"output":{"returncode":1,"stderr":"","stdout":""},"success":false}`, true},
		{"errors_keep_bash_structured_error/array", "bash", `{"error":["x"],"output":{"returncode":1,"stderr":"","stdout":""},"success":false}`, true},
		{"errors_keep_bash_timeout_with_output/stdout", "bash", `{"error":{"message":"` + timeout30 + `"},"output":{"stderr":"","stdout":"partial build log"},"success":false}`, true},
		{"errors_keep_bash_timeout_with_output/stderr", "bash", `{"error":"` + timeout30 + `","output":{"stderr":"warning: slow","stdout":""},"success":false}`, true},
		{"errors_keep_bash_other_error_text/spawn", "bash", `{"error":"spawn failed","success":false}`, true},
		{"errors_keep_bash_other_error_text/extra_words", "bash", `{"error":"Command timed out after 30 seconds while running cargo","success":false}`, true},
		{"errors_keep_bash_unknown_envelope", "bash", `{"success":false,"weird":1}`, true},
		{"errors_keep_bash_zero_or_non_integer_returncode/zero", "bash", `{"error":null,"output":{"returncode":0,"stderr":"","stdout":""},"success":false}`, true},
		{"errors_keep_bash_zero_or_non_integer_returncode/string", "bash", `{"error":null,"output":{"returncode":"1","stderr":"","stdout":""},"success":false}`, true},
		{"errors_keep_bare_shapes_from_other_tools", "python_check", `{"error":null,"output":{"returncode":1,"stderr":"","stdout":""},"success":false}`, true},
		{"error_text_classification/null_message_nonzero_exit", "bash", `{"error":null,"message":null,"output":{"returncode":1,"stderr":"","stdout":""},"success":false}`, false},
		{"error_text_classification/null_message_timeout", "bash", `{"error":{"message":"` + timeout30 + `"},"message":null,"output":"","success":false}`, false},
		// agentsview additions: serde_json as_i64 edges.
		{"returncode_float_is_not_integer", "bash", `{"output":{"returncode":1.0},"success":false}`, true},
		{"returncode_above_i64_is_not_integer", "bash", `{"output":{"returncode":9223372036854775808},"success":false}`, true},
		{"returncode_negative_integer_is_nonzero", "bash", `{"output":{"returncode":-9},"success":false}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectErrors([]Message{tool(tt.tool, tt.envelope)}, "s1")
			if tt.emitted {
				assert.Len(t, got, 1)
			} else {
				assert.Empty(t, got)
			}
		})
	}
	// errors_keep_mode_non_denial also pins the message.
	got := DetectErrors([]Message{tool("mode", `{"error":{"code":"invalid_transition","message":"no such mode"},"success":false}`)}, "s1")
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Text, "invalid_transition")
}
