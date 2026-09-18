package parser

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInferPiSkillNameReadSKILLMD(t *testing.T) {
	tests := []struct {
		name       string
		toolName   string
		inputJSON  string
		sessionCwd string
		want       string
	}{
		{
			name:      "absolute SKILL.md read yields parent dir",
			toolName:  "read",
			inputJSON: `{"path":"/skills/engineering/implement-plan/SKILL.md"}`,
			want:      "implement-plan",
		},
		{
			name:      "file_path key also matches",
			toolName:  "read",
			inputJSON: `{"file_path":"/skills/review/SKILL.md"}`,
			want:      "review",
		},
		{
			name:      "tilde path resolves to parent dir",
			toolName:  "read",
			inputJSON: `{"path":"~/.pi/agent/skills/create-plan/SKILL.md"}`,
			want:      "create-plan",
		},
		{
			name:       "relative path resolves against session cwd",
			toolName:   "read",
			inputJSON:  `{"path":"skills/engineering/create-plan/SKILL.md"}`,
			sessionCwd: "/worktrees/demo",
			want:       "create-plan",
		},
		{
			name:      "plain source file read is not a skill",
			toolName:  "read",
			inputJSON: `{"path":"/repo/auth.go"}`,
			want:      "",
		},
		{
			name:      "glob path is not a skill",
			toolName:  "read",
			inputJSON: `{"path":"/skills/*/SKILL.md"}`,
			want:      "",
		},
		{
			name:      "non-read tools stay unattributed",
			toolName:  "edit",
			inputJSON: `{"path":"/skills/foo/SKILL.md"}`,
			want:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferPiSkillName(tt.toolName, tt.inputJSON, tt.sessionCwd)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestInferPiSkillNameReadFrontmatter(t *testing.T) {
	skillDir := filepath.Join(t.TempDir(), "deploy-docs")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	skillFile := filepath.Join(skillDir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte(
		"---\nname: canonical-deploy-docs\ndescription: Deployment guides\n---\n\nbody\n",
	), 0o644))

	got := inferPiSkillName(
		"read", `{"path":`+strconv.Quote(skillFile)+`}`, "",
	)
	assert.Equal(t, "canonical-deploy-docs", got)
}

func TestInferPiSkillNameSkillURI(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		inputJSON string
		want      string
	}{
		{
			name:      "read with skill path URI",
			toolName:  "read",
			inputJSON: `{"path":"skill://create-plan/SKILL.md"}`,
			want:      "create-plan",
		},
		{
			name:      "bare name is attributed",
			toolName:  "read",
			inputJSON: `{"path":"skill://coding-standards"}`,
			want:      "coding-standards",
		},
		{
			name:      "percent-encoded namespace is decoded",
			toolName:  "read",
			inputJSON: `{"path":"skill://superpowers%3Abrainstorming"}`,
			want:      "superpowers:brainstorming",
		},
		{
			name:      "URI mention in a non-read tool is not a skill load",
			toolName:  "write",
			inputJSON: `{"path":"notes.md","content":"See skill://deploy-docs"}`,
			want:      "",
		},
		{
			name:      "query suffix is stripped",
			toolName:  "read",
			inputJSON: `{"path":"skill://create-plan?section=1"}`,
			want:      "create-plan",
		},
		{
			name:      "empty name falls through",
			toolName:  "read",
			inputJSON: `{"path":"skill:///"}`,
			want:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferPiSkillName(tt.toolName, tt.inputJSON, "")
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestInferPiSkillNameShellCommand(t *testing.T) {
	tests := []struct {
		name       string
		toolName   string
		inputJSON  string
		sessionCwd string
		want       string
	}{
		{
			name:       "cat of a SKILL.md",
			toolName:   "bash",
			inputJSON:  `{"command":"cat skills/review/SKILL.md"}`,
			sessionCwd: "/worktrees/demo",
			want:       "review",
		},
		{
			name:       "grep of a SKILL.md",
			toolName:   "bash",
			inputJSON:  `{"command":"grep -n frontmatter /skills/review/SKILL.md"}`,
			sessionCwd: "",
			want:       "review",
		},
		{
			name:       "sed without -i is a read",
			toolName:   "bash",
			inputJSON:  `{"command":"sed -n 1,10p /skills/review/SKILL.md"}`,
			sessionCwd: "",
			want:       "review",
		},
		{
			name:       "relative path resolves against session cwd",
			toolName:   "bash",
			inputJSON:  `{"command":"cat ./skills/create-plan/SKILL.md"}`,
			sessionCwd: "/worktrees/demo",
			want:       "create-plan",
		},
		{
			name:       "unrelated command is untouched",
			toolName:   "bash",
			inputJSON:  `{"command":"go test ./..."}`,
			sessionCwd: "/worktrees/demo",
			want:       "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferPiSkillName(tt.toolName, tt.inputJSON, tt.sessionCwd)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPiProviderAttributesSkillNames(t *testing.T) {
	_, msgs := parsePiLikeTestSession(t, AgentPi, strings.Join([]string{
		`{"type":"session","version":3,"id":"skill-test","timestamp":"2026-09-03T10:00:00Z","cwd":"/worktrees/demo"}`,
		`{"type":"message","id":"entry-1","parentId":null,"timestamp":"2026-09-03T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Review this change"}]}}`,
		`{"type":"message","id":"entry-2","parentId":"entry-1","timestamp":"2026-09-03T10:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tool-1","name":"read","arguments":{"path":"skills/code-review/SKILL.md"}},{"type":"toolCall","id":"tool-2","name":"read","arguments":{"path":"auth.go"}}]}}`,
	}, "\n"))
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 2)
	assert.Equal(t, "code-review", msgs[1].ToolCalls[0].SkillName)
	assert.Empty(t, msgs[1].ToolCalls[1].SkillName)
}
