package parser

import (
	"strings"
	"time"
)

// AgentType identifies the AI agent that produced a session.
type AgentType string

const (
	AgentClaude        AgentType = "claude"
	AgentCodex         AgentType = "codex"
	AgentCopilot       AgentType = "copilot"
	AgentGemini        AgentType = "gemini"
	AgentOpenCode      AgentType = "opencode"
	AgentCursor        AgentType = "cursor"
	AgentAmp           AgentType = "amp"
	AgentVSCodeCopilot AgentType = "vscode-copilot"
	AgentPi            AgentType = "pi"
	AgentOpenClaw      AgentType = "openclaw"
)

// AgentDef describes a supported coding agent's filesystem
// layout, configuration keys, and session ID conventions.
type AgentDef struct {
	Type         AgentType
	DisplayName  string   // "Claude Code", "Codex", etc.
	EnvVar       string   // env var for dir override
	ConfigKey    string   // JSON key in config.json ("" = none)
	DefaultDirs  []string // paths relative to $HOME
	IDPrefix     string   // session ID prefix ("" for Claude)
	WatchSubdirs []string // subdirs to watch (nil = watch root)
	FileBased    bool     // false for DB-backed agents

	// DiscoverFunc finds session files under a root directory.
	// Nil for non-file-based agents.
	DiscoverFunc func(string) []DiscoveredFile

	// FindSourceFunc locates a single session's source file
	// given a root directory and the raw session ID (prefix
	// already stripped). Nil for non-file-based agents.
	FindSourceFunc func(string, string) string
}

// Registry lists all supported agents. Order is stable and
// used for iteration in config, sync, and watcher setup.
var Registry = []AgentDef{
	{
		Type:           AgentClaude,
		DisplayName:    "Claude Code",
		EnvVar:         "CLAUDE_PROJECTS_DIR",
		ConfigKey:      "claude_project_dirs",
		DefaultDirs:    []string{".claude/projects"},
		IDPrefix:       "",
		FileBased:      true,
		DiscoverFunc:   DiscoverClaudeProjects,
		FindSourceFunc: FindClaudeSourceFile,
	},
	{
		Type:           AgentCodex,
		DisplayName:    "Codex",
		EnvVar:         "CODEX_SESSIONS_DIR",
		ConfigKey:      "codex_sessions_dirs",
		DefaultDirs:    []string{".codex/sessions"},
		IDPrefix:       "codex:",
		FileBased:      true,
		DiscoverFunc:   DiscoverCodexSessions,
		FindSourceFunc: FindCodexSourceFile,
	},
	{
		Type:           AgentCopilot,
		DisplayName:    "Copilot",
		EnvVar:         "COPILOT_DIR",
		ConfigKey:      "copilot_dirs",
		DefaultDirs:    []string{".copilot"},
		IDPrefix:       "copilot:",
		WatchSubdirs:   []string{"session-state"},
		FileBased:      true,
		DiscoverFunc:   DiscoverCopilotSessions,
		FindSourceFunc: FindCopilotSourceFile,
	},
	{
		Type:           AgentGemini,
		DisplayName:    "Gemini",
		EnvVar:         "GEMINI_DIR",
		ConfigKey:      "gemini_dirs",
		DefaultDirs:    []string{".gemini"},
		IDPrefix:       "gemini:",
		WatchSubdirs:   []string{"tmp"},
		FileBased:      true,
		DiscoverFunc:   DiscoverGeminiSessions,
		FindSourceFunc: FindGeminiSourceFile,
	},
	{
		Type:        AgentOpenCode,
		DisplayName: "OpenCode",
		EnvVar:      "OPENCODE_DIR",
		ConfigKey:   "opencode_dirs",
		DefaultDirs: []string{".local/share/opencode"},
		IDPrefix:    "opencode:",
		FileBased:   false,
	},
	{
		Type:           AgentCursor,
		DisplayName:    "Cursor",
		EnvVar:         "CURSOR_PROJECTS_DIR",
		DefaultDirs:    []string{".cursor/projects"},
		IDPrefix:       "cursor:",
		FileBased:      true,
		DiscoverFunc:   DiscoverCursorSessions,
		FindSourceFunc: FindCursorSourceFile,
	},
	{
		Type:           AgentAmp,
		DisplayName:    "Amp",
		EnvVar:         "AMP_DIR",
		DefaultDirs:    []string{".local/share/amp/threads"},
		IDPrefix:       "amp:",
		FileBased:      true,
		DiscoverFunc:   DiscoverAmpSessions,
		FindSourceFunc: FindAmpSourceFile,
	},
	{
		Type:        AgentVSCodeCopilot,
		DisplayName: "VSCode Copilot",
		EnvVar:      "VSCODE_COPILOT_DIR",
		ConfigKey:   "vscode_copilot_dirs",
		DefaultDirs: []string{
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
		},
		IDPrefix: "vscode-copilot:",
		WatchSubdirs: []string{
			"workspaceStorage",
			"globalStorage",
		},
		FileBased:      true,
		DiscoverFunc:   DiscoverVSCodeCopilotSessions,
		FindSourceFunc: FindVSCodeCopilotSourceFile,
	},
	{
		Type:           AgentPi,
		DisplayName:    "Pi",
		EnvVar:         "PI_DIR",
		DefaultDirs:    []string{".pi/agent/sessions"},
		IDPrefix:       "pi:",
		FileBased:      true,
		DiscoverFunc:   DiscoverPiSessions,
		FindSourceFunc: FindPiSourceFile,
	},
	{
		Type:           AgentOpenClaw,
		DisplayName:    "OpenClaw",
		EnvVar:         "OPENCLAW_DIR",
		ConfigKey:      "openclaw_dirs",
		DefaultDirs:    []string{".openclaw/agents"},
		IDPrefix:       "openclaw:",
		FileBased:      true,
		DiscoverFunc:   DiscoverOpenClawSessions,
		FindSourceFunc: FindOpenClawSourceFile,
	},
}

// AgentByType returns the AgentDef for the given type.
func AgentByType(t AgentType) (AgentDef, bool) {
	for _, def := range Registry {
		if def.Type == t {
			return def, true
		}
	}
	return AgentDef{}, false
}

// AgentByPrefix returns the AgentDef whose IDPrefix matches
// the session ID. For Claude (empty prefix), the match
// succeeds only when no other prefix matches and the ID
// does not contain a colon.
func AgentByPrefix(sessionID string) (AgentDef, bool) {
	for _, def := range Registry {
		if def.IDPrefix != "" &&
			strings.HasPrefix(sessionID, def.IDPrefix) {
			return def, true
		}
	}
	// No prefixed agent matched. Fall back to Claude only
	// if the ID has no colon (unprefixed).
	if !strings.Contains(sessionID, ":") {
		if def, ok := AgentByType(AgentClaude); ok {
			return def, true
		}
	}
	return AgentDef{}, false
}

// RelationshipType describes how a session relates to its parent.
type RelationshipType string

const (
	RelNone         RelationshipType = ""
	RelContinuation RelationshipType = "continuation"
	RelSubagent     RelationshipType = "subagent"
	RelFork         RelationshipType = "fork"
)

// RoleType identifies the role of a message sender.
type RoleType string

const (
	RoleUser      RoleType = "user"
	RoleAssistant RoleType = "assistant"
)

// FileInfo holds file system metadata for a session source file.
type FileInfo struct {
	Path  string
	Size  int64
	Mtime int64
	Hash  string
}

// ParsedSession holds session metadata extracted from a JSONL file.
type ParsedSession struct {
	ID               string
	Project          string
	Machine          string
	Agent            AgentType
	ParentSessionID  string
	RelationshipType RelationshipType
	FirstMessage     string
	StartedAt        time.Time
	EndedAt          time.Time
	MessageCount     int
	UserMessageCount int
	File             FileInfo
}

// ParsedToolCall holds a single tool invocation extracted from
// a message.
type ParsedToolCall struct {
	ToolUseID         string // tool_use block id from session data
	ToolName          string // raw name from session data
	Category          string // normalized: Read, Edit, Write, Bash, etc.
	InputJSON         string // raw JSON of the input object
	SkillName         string // skill name when ToolName is "Skill"
	SubagentSessionID string // linked subagent session file (e.g. "agent-{task_id}")
}

// ParsedToolResult holds metadata about a tool result block in a
// user message (the response to a prior tool_use).
type ParsedToolResult struct {
	ToolUseID     string
	ContentLength int
	ContentRaw    string // raw JSON of the content field; decode with DecodeContent
}

// ParsedMessage holds a single extracted message.
type ParsedMessage struct {
	Ordinal       int
	Role          RoleType
	Content       string
	Timestamp     time.Time
	HasThinking   bool
	HasToolUse    bool
	ContentLength int
	ToolCalls     []ParsedToolCall
	ToolResults   []ParsedToolResult
}

// ParseResult pairs a parsed session with its messages.
type ParseResult struct {
	Session  ParsedSession
	Messages []ParsedMessage
}

// InferRelationshipTypes sets RelationshipType on results that have
// a ParentSessionID but no explicit type. Sessions with an "agent-"
// prefix are subagents; others are continuations.
func InferRelationshipTypes(results []ParseResult) {
	for i := range results {
		if results[i].Session.ParentSessionID == "" {
			continue
		}
		if results[i].Session.RelationshipType != RelNone {
			continue
		}
		if strings.HasPrefix(results[i].Session.ID, "agent-") {
			results[i].Session.RelationshipType = RelSubagent
		} else {
			results[i].Session.RelationshipType = RelContinuation
		}
	}
}
