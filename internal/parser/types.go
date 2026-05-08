package parser

import (
	"encoding/json"
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
	AgentOpenHands     AgentType = "openhands"
	AgentCursor        AgentType = "cursor"
	AgentIflow         AgentType = "iflow"
	AgentAmp           AgentType = "amp"
	AgentZencoder      AgentType = "zencoder"
	AgentVSCodeCopilot AgentType = "vscode-copilot"
	AgentPi            AgentType = "pi"
	AgentOpenClaw      AgentType = "openclaw"
	AgentKimi          AgentType = "kimi"
	AgentClaudeAI      AgentType = "claude-ai"
	AgentChatGPT       AgentType = "chatgpt"
	AgentKiro          AgentType = "kiro"
	AgentKiroIDE       AgentType = "kiro-ide"
	AgentCortex        AgentType = "cortex"
	AgentHermes        AgentType = "hermes"
	AgentForge         AgentType = "forge"
	AgentPiebald       AgentType = "piebald"
	AgentWarp          AgentType = "warp"
	AgentPositron      AgentType = "positron"
)

// AgentDef describes a supported coding agent's filesystem
// layout, configuration keys, and session ID conventions.
type AgentDef struct {
	Type         AgentType
	DisplayName  string   // "Claude Code", "Codex", etc.
	EnvVar       string   // env var for dir override
	ConfigKey    string   // TOML key in config.toml ("" = none)
	DefaultDirs  []string // paths relative to $HOME
	IDPrefix     string   // session ID prefix ("" for Claude)
	WatchSubdirs []string // subdirs to watch (nil = watch root)
	ShallowWatch bool     // true = watch root only, rely on periodic sync for subdirs
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
		WatchSubdirs: []string{
			"storage/session",
			"storage/message",
			"storage/part",
		},
		FileBased:      true,
		DiscoverFunc:   DiscoverOpenCodeSessions,
		FindSourceFunc: FindOpenCodeSourceFile,
	},
	{
		Type:           AgentOpenHands,
		DisplayName:    "OpenHands CLI",
		EnvVar:         "OPENHANDS_CONVERSATIONS_DIR",
		ConfigKey:      "openhands_dirs",
		DefaultDirs:    []string{".openhands/conversations"},
		IDPrefix:       "openhands:",
		FileBased:      true,
		ShallowWatch:   true,
		DiscoverFunc:   DiscoverOpenHandsSessions,
		FindSourceFunc: FindOpenHandsSourceFile,
	},
	{
		Type:           AgentCursor,
		DisplayName:    "Cursor",
		EnvVar:         "CURSOR_PROJECTS_DIR",
		ConfigKey:      "cursor_project_dirs",
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
		ConfigKey:      "amp_dirs",
		DefaultDirs:    []string{".local/share/amp/threads"},
		IDPrefix:       "amp:",
		FileBased:      true,
		DiscoverFunc:   DiscoverAmpSessions,
		FindSourceFunc: FindAmpSourceFile,
	},
	{
		Type:           AgentZencoder,
		DisplayName:    "Zencoder",
		EnvVar:         "ZENCODER_DIR",
		ConfigKey:      "zencoder_dirs",
		DefaultDirs:    []string{".zencoder/sessions"},
		IDPrefix:       "zencoder:",
		FileBased:      true,
		DiscoverFunc:   DiscoverZencoderSessions,
		FindSourceFunc: FindZencoderSourceFile,
	},
	{
		Type:           AgentIflow,
		DisplayName:    "iFlow",
		EnvVar:         "IFLOW_DIR",
		ConfigKey:      "iflow_dirs",
		DefaultDirs:    []string{".iflow/projects"},
		IDPrefix:       "iflow:",
		FileBased:      true,
		DiscoverFunc:   DiscoverIflowProjects,
		FindSourceFunc: FindIflowSourceFile,
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
		ConfigKey:      "pi_dirs",
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
	{
		Type:           AgentKimi,
		DisplayName:    "Kimi",
		EnvVar:         "KIMI_DIR",
		ConfigKey:      "kimi_dirs",
		DefaultDirs:    []string{".kimi/sessions"},
		IDPrefix:       "kimi:",
		FileBased:      true,
		DiscoverFunc:   DiscoverKimiSessions,
		FindSourceFunc: FindKimiSourceFile,
	},
	{
		Type:        AgentClaudeAI,
		DisplayName: "Claude.ai",
		IDPrefix:    "claude-ai:",
		FileBased:   false,
	},
	{
		Type:        AgentChatGPT,
		DisplayName: "ChatGPT",
		IDPrefix:    "chatgpt:",
		FileBased:   false,
	},
	{
		Type:           AgentKiro,
		DisplayName:    "Kiro",
		EnvVar:         "KIRO_SESSIONS_DIR",
		ConfigKey:      "kiro_dirs",
		DefaultDirs:    []string{".kiro/sessions/cli"},
		IDPrefix:       "kiro:",
		FileBased:      true,
		DiscoverFunc:   DiscoverKiroSessions,
		FindSourceFunc: FindKiroSourceFile,
	},
	{
		Type:           AgentKiroIDE,
		DisplayName:    "Kiro IDE",
		EnvVar:         "KIRO_IDE_DIR",
		ConfigKey:      "kiro_ide_dirs",
		DefaultDirs:    kiroIDEDefaultDirs(),
		IDPrefix:       "kiro-ide:",
		FileBased:      true,
		DiscoverFunc:   DiscoverKiroIDESessions,
		FindSourceFunc: FindKiroIDESourceFile,
	},
	{
		Type:        AgentCortex,
		DisplayName: "Cortex Code",
		EnvVar:      "CORTEX_DIR",
		ConfigKey:   "cortex_dirs",
		DefaultDirs: []string{
			".snowflake/cortex/conversations",
		},
		IDPrefix:       "cortex:",
		FileBased:      true,
		DiscoverFunc:   DiscoverCortexSessions,
		FindSourceFunc: FindCortexSourceFile,
	},
	{
		Type:           AgentHermes,
		DisplayName:    "Hermes Agent",
		EnvVar:         "HERMES_SESSIONS_DIR",
		ConfigKey:      "hermes_sessions_dirs",
		DefaultDirs:    []string{".hermes/sessions"},
		IDPrefix:       "hermes:",
		FileBased:      true,
		DiscoverFunc:   DiscoverHermesSessions,
		FindSourceFunc: FindHermesSourceFile,
	},
	{
		Type:        AgentForge,
		DisplayName: "Forge",
		EnvVar:      "FORGE_DIR",
		ConfigKey:   "forge_dirs",
		DefaultDirs: []string{".forge"},
		IDPrefix:    "forge:",
		FileBased:   false,
	},
	{
		Type:        AgentPiebald,
		DisplayName: "Piebald",
		EnvVar:      "PIEBALD_DIR",
		ConfigKey:   "piebald_dirs",
		DefaultDirs: []string{
			// Linux
			".local/share/piebald",
			// macOS
			"Library/Application Support/piebald",
			// Windows
			"AppData/Roaming/piebald",
		},
		IDPrefix:  "piebald:",
		FileBased: false,
	},
	{
		Type:        AgentWarp,
		DisplayName: "Warp",
		EnvVar:      "WARP_DIR",
		ConfigKey:   "warp_dirs",
		DefaultDirs: warpDefaultDirs(),
		IDPrefix:    "warp:",
		FileBased:   false,
	},
	{
		Type:        AgentPositron,
		DisplayName: "Positron Assistant",
		EnvVar:      "POSITRON_DIR",
		ConfigKey:   "positron_dirs",
		DefaultDirs: []string{
			"Library/Application Support/Positron/User",
		},
		IDPrefix:       "positron:",
		WatchSubdirs:   []string{"workspaceStorage"},
		FileBased:      true,
		DiscoverFunc:   DiscoverPositronSessions,
		FindSourceFunc: FindPositronSourceFile,
	},
}

// NonFileBackedAgents returns agent types where FileBased is false.
func NonFileBackedAgents() []AgentType {
	var agents []AgentType
	for _, def := range Registry {
		if !def.FileBased {
			agents = append(agents, def.Type)
		}
	}
	return agents
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

// StripHostPrefix splits a remote session ID into its host
// and raw ID parts. Remote IDs use the form "host~rawID"
// where the "~" separator avoids conflict with both agent
// prefixes (":") and URL path segments ("/"). For local
// session IDs (no "~" present), host is empty and rawID is
// the original ID.
func StripHostPrefix(id string) (host, rawID string) {
	if before, after, ok := strings.Cut(id, "~"); ok {
		return before, after
	}
	return "", id
}

// AgentByPrefix returns the AgentDef whose IDPrefix matches
// the session ID. For Claude (empty prefix), the match
// succeeds only when no other prefix matches and the ID
// does not contain a colon. Host prefixes ("host~...") are
// stripped before matching.
func AgentByPrefix(sessionID string) (AgentDef, bool) {
	_, rawID := StripHostPrefix(sessionID)
	for _, def := range Registry {
		if def.IDPrefix != "" &&
			strings.HasPrefix(rawID, def.IDPrefix) {
			return def, true
		}
	}
	// No prefixed agent matched. Fall back to Claude only
	// if the raw ID has no colon (unprefixed).
	if !strings.Contains(rawID, ":") {
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
	Path   string
	Size   int64
	Mtime  int64
	Inode  int64
	Device int64
	Hash   string
}

// ParsedSession holds session metadata extracted from a JSONL file.
type ParsedSession struct {
	ID               string
	Project          string
	Machine          string
	Agent            AgentType
	ParentSessionID  string
	RelationshipType RelationshipType
	Cwd              string
	GitBranch        string
	SourceSessionID  string
	SourceVersion    string
	MalformedLines   int
	IsTruncated      bool
	FirstMessage     string
	DisplayName      string
	StartedAt        time.Time
	EndedAt          time.Time
	MessageCount     int
	UserMessageCount int
	File             FileInfo

	// TerminationStatus describes how the session appears to have
	// ended. Empty string = unknown (parser did not classify, or
	// agent format does not yet support classification).
	TerminationStatus TerminationStatus

	TotalOutputTokens    int
	PeakContextTokens    int
	HasTotalOutputTokens bool
	HasPeakContextTokens bool

	// aggregateTokenPresenceKnown marks session aggregate token
	// coverage as parser-owned and authoritative.
	aggregateTokenPresenceKnown bool
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
	ResultEvents      []ParsedToolResultEvent
}

// ParsedToolResult holds metadata about a tool result block in a
// user message (the response to a prior tool_use).
type ParsedToolResult struct {
	ToolUseID     string
	ContentLength int
	ContentRaw    string // raw JSON of the content field; decode with DecodeContent
}

// ParsedToolResultEvent is a canonical chronological update attached
// to a tool call. Used for Codex subagent terminal status updates.
type ParsedToolResultEvent struct {
	ToolUseID         string
	AgentID           string
	SubagentSessionID string
	Source            string
	Status            string
	Content           string
	Timestamp         time.Time
}

// ParsedMessage holds a single extracted message.
type ParsedMessage struct {
	Ordinal       int
	Role          RoleType
	Content       string
	ThinkingText  string // concatenated text of all thinking blocks; "" if none
	Timestamp     time.Time
	HasThinking   bool
	HasToolUse    bool
	IsSystem      bool
	ContentLength int
	ToolCalls     []ParsedToolCall
	ToolResults   []ParsedToolResult

	Model            string
	TokenUsage       json.RawMessage
	ContextTokens    int
	OutputTokens     int
	HasContextTokens bool
	HasOutputTokens  bool

	// ClaudeMessageID and ClaudeRequestID hold the provider's
	// per-response identifiers. Used for cross-file / cross-session
	// deduplication when summing token usage, matching ccusage's
	// `${messageId}:${requestId}` hash. Only populated by the
	// Claude parser; empty for all other agents.
	ClaudeMessageID string
	ClaudeRequestID string

	SourceType        string
	SourceSubtype     string
	SourceUUID        string
	SourceParentUUID  string
	IsSidechain       bool
	IsCompactBoundary bool

	// StopReason is the reason the assistant stopped generating
	// (Claude: "end_turn", "tool_use", "max_tokens", "stop_sequence";
	// other agents may use their own vocabulary or leave it empty).
	// Only populated for assistant messages where the parser sees
	// the field. Empty when unknown.
	StopReason string

	// tokenPresenceKnown marks per-message token coverage as
	// parser-owned and authoritative.
	tokenPresenceKnown bool
}

// accumulateMessageTokenUsage rolls up explicit per-message token
// metadata into session totals without inferring presence from raw
// numeric values alone.
func accumulateMessageTokenUsage(
	sess *ParsedSession,
	messages []ParsedMessage,
) {
	sess.aggregateTokenPresenceKnown = true
	for _, m := range messages {
		if m.HasOutputTokens {
			sess.HasTotalOutputTokens = true
			sess.TotalOutputTokens += m.OutputTokens
		}
		if m.HasContextTokens {
			sess.HasPeakContextTokens = true
			if m.ContextTokens > sess.PeakContextTokens {
				sess.PeakContextTokens = m.ContextTokens
			}
		}
	}
}

// InferTokenPresence determines whether context/output tokens were
// present in a provider payload. It starts from explicit boolean
// flags (and non-zero numeric values), then inspects tokenUsage JSON
// keys when available. This is the single source of truth for token
// presence inference across all storage backends.
func InferTokenPresence(
	tokenUsage []byte,
	contextTokens, outputTokens int,
	hasContext, hasOutput bool,
) (bool, bool) {
	hasContext = hasContext || contextTokens != 0
	hasOutput = hasOutput || outputTokens != 0

	if len(tokenUsage) == 0 {
		return hasContext, hasOutput
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(tokenUsage, &payload); err != nil {
		return hasContext, hasOutput
	}

	for key := range payload {
		switch key {
		case "input_tokens", "cache_creation_input_tokens",
			"cache_read_input_tokens", "input",
			"cached", "context_tokens":
			hasContext = true
		case "output_tokens", "output":
			hasOutput = true
		}
	}
	return hasContext, hasOutput
}

// TokenPresence reports whether context/output token fields were
// present in the provider payload. Falls back to raw token_usage
// key inspection when parser-specific flags were not populated.
func (m ParsedMessage) TokenPresence() (bool, bool) {
	if m.tokenPresenceKnown {
		return m.HasContextTokens, m.HasOutputTokens
	}
	return InferTokenPresence(
		m.TokenUsage, m.ContextTokens, m.OutputTokens,
		m.HasContextTokens, m.HasOutputTokens,
	)
}

// AggregateTokenPresence reports whether aggregate session token
// metrics were present. This preserves explicit flags and falls
// back to non-zero aggregates for providers like Kimi that only
// expose truthful session-level totals in current Task 1 paths.
func (s ParsedSession) AggregateTokenPresence() (bool, bool) {
	if s.aggregateTokenPresenceKnown {
		return s.HasTotalOutputTokens, s.HasPeakContextTokens
	}

	return s.HasTotalOutputTokens || s.TotalOutputTokens > 0,
		s.HasPeakContextTokens || s.PeakContextTokens > 0
}

// TokenCoverage reports the truthful aggregate/session coverage
// after combining session-level aggregate presence with per-message
// token presence.
func (s ParsedSession) TokenCoverage(
	msgs []ParsedMessage,
) (bool, bool) {
	hasTotal, hasPeak := s.AggregateTokenPresence()
	for _, m := range msgs {
		msgHasCtx, msgHasOut := m.TokenPresence()
		hasTotal = hasTotal || msgHasOut
		hasPeak = hasPeak || msgHasCtx
	}
	return hasTotal, hasPeak
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
