package parser

import (
	"slices"
	"testing"
)

func TestAgentByType(t *testing.T) {
	tests := []struct {
		input AgentType
		want  bool
	}{
		{AgentClaude, true},
		{AgentCodex, true},
		{AgentCopilot, true},
		{AgentGemini, true},
		{AgentOpenCode, true},
		{AgentCursor, true},
		{AgentAmp, true},
		{AgentVSCodeCopilot, true},
		{AgentPi, true},
		{"unknown", false},
	}
	for _, tt := range tests {
		def, ok := AgentByType(tt.input)
		if ok != tt.want {
			t.Errorf(
				"AgentByType(%q) ok = %v, want %v",
				tt.input, ok, tt.want,
			)
		}
		if ok && def.Type != tt.input {
			t.Errorf(
				"AgentByType(%q).Type = %q",
				tt.input, def.Type,
			)
		}
	}
}

func TestAgentByPrefix(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		wantType  AgentType
		wantOK    bool
	}{
		{
			"claude no prefix",
			"abc-123",
			AgentClaude,
			true,
		},
		{
			"codex prefix",
			"codex:some-uuid",
			AgentCodex,
			true,
		},
		{
			"copilot prefix",
			"copilot:sess-id",
			AgentCopilot,
			true,
		},
		{
			"gemini prefix",
			"gemini:sess-id",
			AgentGemini,
			true,
		},
		{
			"opencode prefix",
			"opencode:sess-id",
			AgentOpenCode,
			true,
		},
		{
			"cursor prefix",
			"cursor:sess-id",
			AgentCursor,
			true,
		},
		{
			"amp prefix",
			"amp:T-019ca26f",
			AgentAmp,
			true,
		},
		{
			"vscode-copilot prefix",
			"vscode-copilot:sess-id",
			AgentVSCodeCopilot,
			true,
		},
		{
			"pi prefix",
			"pi:pi-session-uuid",
			AgentPi,
			true,
		},
		{
			"unknown prefix",
			"future:sess-id",
			"",
			false,
		},
		{
			"empty string",
			"",
			AgentClaude,
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, ok := AgentByPrefix(tt.sessionID)
			if ok != tt.wantOK {
				t.Fatalf(
					"AgentByPrefix(%q) ok = %v, want %v",
					tt.sessionID, ok, tt.wantOK,
				)
			}
			if ok && def.Type != tt.wantType {
				t.Errorf(
					"AgentByPrefix(%q).Type = %q, want %q",
					tt.sessionID, def.Type, tt.wantType,
				)
			}
		})
	}
}

func TestRegistryCompleteness(t *testing.T) {
	allTypes := []AgentType{
		AgentClaude,
		AgentCodex,
		AgentCopilot,
		AgentGemini,
		AgentOpenCode,
		AgentCursor,
		AgentAmp,
		AgentVSCodeCopilot,
		AgentPi,
	}

	registered := make(map[AgentType]bool)
	for _, def := range Registry {
		registered[def.Type] = true
	}

	for _, at := range allTypes {
		if !registered[at] {
			t.Errorf(
				"AgentType %q missing from Registry", at,
			)
		}
	}
}

func TestInferRelationshipTypes(t *testing.T) {
	tests := []struct {
		name   string
		inputs []ParseResult
		want   []RelationshipType
	}{{
		"no parent",
		[]ParseResult{
			{Session: ParsedSession{ID: "abc"}},
		},
		[]RelationshipType{RelNone},
	},
		{
			"agent prefix gets subagent",
			[]ParseResult{
				{Session: ParsedSession{
					ID:              "agent-123",
					ParentSessionID: "parent",
				}},
			},
			[]RelationshipType{RelSubagent},
		},
		{
			"non-agent prefix gets continuation",
			[]ParseResult{
				{Session: ParsedSession{
					ID:              "child-session",
					ParentSessionID: "parent",
				}},
			},
			[]RelationshipType{RelContinuation},
		},
		{
			"pi prefixed session with parent gets continuation",
			[]ParseResult{
				{Session: ParsedSession{
					ID:              "pi:branched-session",
					ParentSessionID: "pi:parent-session",
				}},
			},
			[]RelationshipType{RelContinuation},
		},
		{
			"explicit type preserved",
			[]ParseResult{
				{Session: ParsedSession{
					ID:               "abc-fork",
					ParentSessionID:  "parent",
					RelationshipType: RelFork,
				}},
			},
			[]RelationshipType{RelFork},
		},
		{
			"mixed results",
			[]ParseResult{
				{Session: ParsedSession{ID: "main"}},
				{Session: ParsedSession{
					ID:              "agent-task1",
					ParentSessionID: "main",
				}},
				{Session: ParsedSession{
					ID:               "main-fork-uuid",
					ParentSessionID:  "main",
					RelationshipType: RelFork,
				}},
				{Session: ParsedSession{
					ID:              "child",
					ParentSessionID: "main",
				}},
			},
			[]RelationshipType{
				RelNone, RelSubagent, RelFork, RelContinuation,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			InferRelationshipTypes(tt.inputs)
			if len(tt.inputs) != len(tt.want) {
				t.Fatalf("len(inputs) = %d, want %d", len(tt.inputs), len(tt.want))
			}
			for i, r := range tt.inputs {
				if r.Session.RelationshipType != tt.want[i] {
					t.Errorf(
						"inputs[%d].RelationshipType = %q, want %q",
						i, r.Session.RelationshipType, tt.want[i],
					)
				}
			}
		})
	}
}

func TestVSCodeCopilotDefaultDirs(t *testing.T) {
	def, ok := AgentByType(AgentVSCodeCopilot)
	if !ok {
		t.Fatal("AgentVSCodeCopilot not in Registry")
	}

	required := []string{
		// Windows
		"AppData/Roaming/Code/User",
		"AppData/Roaming/Code - Insiders/User",
		"AppData/Roaming/VSCodium/User",
		// macOS
		"Library/Application Support/Code/User",
		"Library/Application Support/Code - Insiders/User",
		"Library/Application Support/VSCodium/User",
		// Linux
		".config/Code/User",
		".config/Code - Insiders/User",
		".config/VSCodium/User",
	}
	for _, path := range required {
		if !slices.Contains(def.DefaultDirs, path) {
			t.Errorf("missing default dir: %s", path)
		}
	}
}
