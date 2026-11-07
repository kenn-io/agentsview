package gittrailer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sessionUUID = "12345678-1234-1234-1234-1234567890ab"

func TestDiscover(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  map[string]string
		want Session
		bad  bool
	}{
		{name: "none"},
		{name: "claude", env: map[string]string{"CLAUDE_CODE_SESSION_ID": sessionUUID}, want: Session{"claude", sessionUUID}},
		{name: "codex thread", env: map[string]string{"CODEX_THREAD_ID": sessionUUID}, want: Session{"codex", sessionUUID}},
		{name: "codex legacy", env: map[string]string{"CODEX_SESSION_ID": sessionUUID}, want: Session{"codex", sessionUUID}},
		{name: "cursor", env: map[string]string{"CURSOR_CONVERSATION_ID": sessionUUID}, want: Session{"cursor", sessionUUID}},
		{name: "opencode", env: map[string]string{"OPENCODE_SESSION_ID": "ses_8f2bA9"}, want: Session{"opencode", "ses_8f2bA9"}},
		{name: "pi", env: map[string]string{"PI_SESSION_ID": sessionUUID}, want: Session{"pi", sessionUUID}},
		{name: "oh my pi", env: map[string]string{"PI_SESSION_ID": sessionUUID, "OMPCODE": "1"}, want: Session{"omp", sessionUUID}},
		{name: "qwen", env: map[string]string{"QWEN_CODE_SESSION_ID": sessionUUID}, want: Session{"qwen", sessionUUID}},
		{name: "goose", env: map[string]string{"AGENT_SESSION_ID": "20261105_3"}, want: Session{"goose", "20261105_3"}},
		{name: "foreign agent session ignored", env: map[string]string{"AGENT_SESSION_ID": sessionUUID}},
		{name: "foreign agent session leaves others", env: map[string]string{"AGENT_SESSION_ID": "run-7", "HERMES_SESSION_ID": "20261105_142233_a1b2c3"}, want: Session{"hermes", "20261105_142233_a1b2c3"}},
		{name: "hermes", env: map[string]string{"HERMES_SESSION_ID": "20261105_142233_a1b2c3"}, want: Session{"hermes", "20261105_142233_a1b2c3"}},
		{name: "invalid hermes", env: map[string]string{"HERMES_SESSION_ID": "_bad"}, bad: true},
		{name: "agents disagree", env: map[string]string{"CLAUDE_CODE_SESSION_ID": sessionUUID, "CODEX_THREAD_ID": sessionUUID}, bad: true},
		{name: "nested harness", env: map[string]string{"CLAUDE_CODE_SESSION_ID": sessionUUID, "PI_SESSION_ID": sessionUUID}, bad: true},
		{name: "codex subagent", env: map[string]string{"CODEX_SESSION_ID": sessionUUID, "CODEX_THREAD_ID": "abcdefab-1234-1234-1234-1234567890ab"}, want: Session{"codex", "abcdefab-1234-1234-1234-1234567890ab"}},
		{name: "invalid thread takes precedence", env: map[string]string{"CODEX_SESSION_ID": sessionUUID, "CODEX_THREAD_ID": "bad"}, bad: true},
		{name: "case preserves first source", env: map[string]string{"CODEX_SESSION_ID": sessionUUID, "CODEX_THREAD_ID": strings.ToUpper(sessionUUID)}, want: Session{"codex", "12345678-1234-1234-1234-1234567890AB"}},
		{name: "override wins", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "pi", "AGENTSVIEW_SESSION_ID": "run_7.9-A", "CODEX_THREAD_ID": sessionUUID, "CLAUDE_CODE_SESSION_ID": sessionUUID}, want: Session{"pi", "run_7.9-A"}},
		{name: "half pair", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "pi", "CODEX_THREAD_ID": sessionUUID}, bad: true},
		{name: "invalid override", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "codex", "AGENTSVIEW_SESSION_ID": "bad", "CODEX_THREAD_ID": sessionUUID}, bad: true},
		{name: "bad UUID", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "123456781234123412341234567890ab"}, bad: true},
		{name: "bad agent", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "Bad", "AGENTSVIEW_SESSION_ID": "run"}, bad: true},
		{name: "bad token", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "pi", "AGENTSVIEW_SESSION_ID": "run\ntrailer"}, bad: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Discover(func(k string) string { return tt.env[k] })
			if tt.bad {
				require.Error(t, err)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
