package parser

import (
	"encoding/json"
	"strings"
	"time"
)

// AgentType identifies the AI agent that produced a session.
type AgentType string

const (
	AgentClaude         AgentType = "claude"
	AgentOpenClaude     AgentType = "openclaude"
	AgentCowork         AgentType = "cowork"
	AgentCodex          AgentType = "codex"
	AgentCopilot        AgentType = "copilot"
	AgentGemini         AgentType = "gemini"
	AgentMiMoCode       AgentType = "mimocode"
	AgentOpenCode       AgentType = "opencode"
	AgentKilo           AgentType = "kilo"
	AgentOpenHands      AgentType = "openhands"
	AgentCursor         AgentType = "cursor"
	AgentIflow          AgentType = "iflow"
	AgentAmp            AgentType = "amp"
	AgentZencoder       AgentType = "zencoder"
	AgentVSCodeCopilot  AgentType = "vscode-copilot"
	AgentWindsurf       AgentType = "windsurf"
	AgentTrae           AgentType = "trae"
	AgentVSCopilot      AgentType = "visualstudio-copilot"
	AgentPi             AgentType = "pi"
	AgentOMP            AgentType = "omp"
	AgentQwen           AgentType = "qwen"
	AgentCommandCode    AgentType = "commandcode"
	AgentDeepSeekTUI    AgentType = "deepseek-tui"
	AgentOpenClaw       AgentType = "openclaw"
	AgentQClaw          AgentType = "qclaw"
	AgentKimi           AgentType = "kimi"
	AgentClaudeAI       AgentType = "claude-ai"
	AgentChatGPT        AgentType = "chatgpt"
	AgentKiro           AgentType = "kiro"
	AgentKiroIDE        AgentType = "kiro-ide"
	AgentCortex         AgentType = "cortex"
	AgentHermes         AgentType = "hermes"
	AgentGrok           AgentType = "grok"
	AgentWorkBuddy      AgentType = "workbuddy"
	AgentForge          AgentType = "forge"
	AgentDevin          AgentType = "devin"
	AgentPiebald        AgentType = "piebald"
	AgentWarp           AgentType = "warp"
	AgentPositron       AgentType = "positron"
	AgentPositAssistant AgentType = "posit-assistant"
	AgentZCode          AgentType = "zcode"
	AgentAntigravity    AgentType = "antigravity"
	AgentAntigravityCLI AgentType = "antigravity-cli"
	AgentVibe           AgentType = "vibe"
	AgentZed            AgentType = "zed"
	AgentQwenPaw        AgentType = "qwenpaw"
	AgentGptme          AgentType = "gptme"
	AgentQoder          AgentType = "qoder"
	AgentShelley        AgentType = "shelley"
	AgentAider          AgentType = "aider"
	AgentReasonix       AgentType = "reasonix"
	AgentIcodemate      AgentType = "icodemate"
	AgentRooCode        AgentType = "roocode"
)

// AgentDef describes a supported coding agent's filesystem
// layout, configuration keys, and session ID conventions.
type AgentDef struct {
	Type              AgentType
	DisplayName       string   // "Claude Code", "Codex", etc.
	EnvVar            string   // env var for dir override
	DefaultRootEnvVar string   // env var that re-roots DefaultDirs before $HOME fallback
	ConfigKey         string   // TOML key in config.toml ("" = none)
	DefaultDirs       []string // paths relative to $HOME
	IDPrefix          string   // session ID prefix ("" for Claude)
	WatchSubdirs      []string // subdirs to watch (nil = watch root)
	ShallowWatch      bool     // true = watch root only, rely on periodic sync for subdirs
	FileBased         bool     // false for DB-backed agents
	Usage             UsageCapabilities

	// WatchRootsFunc resolves the directories to watch for live
	// updates under a configured root, for agents whose watch
	// targets depend on the on-disk layout rather than a static
	// WatchSubdirs list. When set, it takes precedence over
	// WatchSubdirs. Nil for agents that use WatchSubdirs.
	WatchRootsFunc func(string) []string

	// ShallowWatchRootsFunc resolves directories to watch shallowly
	// (root only) for a configured root, in addition to the agent's
	// normal recursive watch. Used for sibling metadata files that
	// live outside the session tree, such as Codex's
	// session_index.jsonl. Nil for agents with no such files.
	ShallowWatchRootsFunc func(string) []string
}

type UsageCapabilities struct {
	NoPerMessageTokenData bool
}

// Registry lists all supported agents. Order is stable and
// used for iteration in config, sync, and watcher setup.
var Registry = []AgentDef{
	{
		Type:              AgentClaude,
		DisplayName:       "Claude Code",
		EnvVar:            "CLAUDE_PROJECTS_DIR",
		DefaultRootEnvVar: "CLAUDE_CONFIG_DIR",
		ConfigKey:         "claude_project_dirs",
		DefaultDirs:       []string{".claude/projects"},
		IDPrefix:          "",
		FileBased:         true,
	},
	{
		Type:              AgentOpenClaude,
		DisplayName:       "OpenClaude",
		EnvVar:            "OPENCLAUDE_PROJECTS_DIR",
		DefaultRootEnvVar: "OPENCLAUDE_CONFIG_DIR",
		ConfigKey:         "openclaude_project_dirs",
		DefaultDirs:       []string{".openclaude/projects"},
		IDPrefix:          "openclaude:",
		FileBased:         true,
	},
	{
		Type:         AgentCowork,
		DisplayName:  "Claude Cowork",
		EnvVar:       "COWORK_DIR",
		ConfigKey:    "cowork_dirs",
		DefaultDirs:  coworkDefaultDirs(),
		IDPrefix:     "cowork:",
		FileBased:    true,
		ShallowWatch: true,
	},
	{
		Type:        AgentCodex,
		DisplayName: "Codex",
		EnvVar:      "CODEX_SESSIONS_DIR",
		ConfigKey:   "codex_sessions_dirs",
		DefaultDirs: []string{
			".codex/sessions",
			".codex/archived_sessions",
		},
		IDPrefix:              "codex:",
		FileBased:             true,
		ShallowWatchRootsFunc: ResolveCodexShallowWatchRoots,
	},
	{
		Type:         AgentCopilot,
		DisplayName:  "Copilot",
		EnvVar:       "COPILOT_DIR",
		ConfigKey:    "copilot_dirs",
		DefaultDirs:  []string{".copilot"},
		IDPrefix:     "copilot:",
		WatchSubdirs: []string{"session-state"},
		FileBased:    true,
		Usage: UsageCapabilities{
			NoPerMessageTokenData: true,
		},
	},
	{
		Type:         AgentGemini,
		DisplayName:  "Gemini",
		EnvVar:       "GEMINI_DIR",
		ConfigKey:    "gemini_dirs",
		DefaultDirs:  []string{".gemini"},
		IDPrefix:     "gemini:",
		WatchSubdirs: []string{"tmp"},
		FileBased:    true,
	},
	{
		Type:        AgentMiMoCode,
		DisplayName: "MiMoCode",
		EnvVar:      "MIMOCODE_DIR",
		ConfigKey:   "mimocode_dirs",
		DefaultDirs: []string{".local/share/mimocode"},
		IDPrefix:    "mimocode:",
		WatchSubdirs: []string{
			"storage/session_diff",
			"storage/message",
			"storage/part",
		},
		FileBased:      true,
		WatchRootsFunc: ResolveMiMoCodeWatchRoots,
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
		WatchRootsFunc: ResolveOpenCodeWatchRoots,
	},
	{
		Type:        AgentKilo,
		DisplayName: "Kilo",
		EnvVar:      "KILO_DIR",
		ConfigKey:   "kilo_dirs",
		DefaultDirs: []string{".local/share/kilo"},
		IDPrefix:    "kilo:",
		WatchSubdirs: []string{
			"storage/session",
			"storage/message",
			"storage/part",
		},
		FileBased:      true,
		WatchRootsFunc: ResolveKiloWatchRoots,
	},
	{
		Type:         AgentOpenHands,
		DisplayName:  "OpenHands CLI",
		EnvVar:       "OPENHANDS_CONVERSATIONS_DIR",
		ConfigKey:    "openhands_dirs",
		DefaultDirs:  []string{".openhands/conversations"},
		IDPrefix:     "openhands:",
		FileBased:    true,
		ShallowWatch: true,
	},
	{
		Type:        AgentCursor,
		DisplayName: "Cursor",
		EnvVar:      "CURSOR_PROJECTS_DIR",
		ConfigKey:   "cursor_project_dirs",
		DefaultDirs: []string{".cursor/projects"},
		IDPrefix:    "cursor:",
		FileBased:   true,
	},
	{
		Type:        AgentAmp,
		DisplayName: "Amp",
		EnvVar:      "AMP_DIR",
		ConfigKey:   "amp_dirs",
		DefaultDirs: []string{".local/share/amp/threads"},
		IDPrefix:    "amp:",
		FileBased:   true,
	},
	{
		Type:        AgentZencoder,
		DisplayName: "Zencoder",
		EnvVar:      "ZENCODER_DIR",
		ConfigKey:   "zencoder_dirs",
		DefaultDirs: []string{".zencoder/sessions"},
		IDPrefix:    "zencoder:",
		FileBased:   true,
	},
	{
		Type:        AgentIflow,
		DisplayName: "iFlow",
		EnvVar:      "IFLOW_DIR",
		ConfigKey:   "iflow_dirs",
		DefaultDirs: []string{".iflow/projects"},
		IDPrefix:    "iflow:",
		FileBased:   true,
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
		FileBased: true,
		Usage: UsageCapabilities{
			NoPerMessageTokenData: true,
		},
	},
	{
		Type:        AgentWindsurf,
		DisplayName: "Windsurf",
		EnvVar:      "WINDSURF_DIR",
		ConfigKey:   "windsurf_dirs",
		DefaultDirs: []string{
			// Windows
			"AppData/Roaming/Windsurf/User",
			"AppData/Roaming/Windsurf - Next/User",
			// macOS
			"Library/Application Support/Windsurf/User",
			"Library/Application Support/Windsurf - Next/User",
			// Linux
			".config/Windsurf/User",
			".config/Windsurf - Next/User",
		},
		IDPrefix: "windsurf:",
		WatchSubdirs: []string{
			"workspaceStorage",
		},
		FileBased: true,
		Usage: UsageCapabilities{
			NoPerMessageTokenData: true,
		},
	},
	{
		Type:        AgentTrae,
		DisplayName: "Trae",
		EnvVar:      "TRAE_DIR",
		ConfigKey:   "trae_dirs",
		DefaultDirs: []string{
			// Windows
			"AppData/Roaming/Trae/User",
			"AppData/Roaming/Trae CN/User",
			"AppData/Roaming/TRAE SOLO CN/User",
			// macOS
			"Library/Application Support/Trae/User",
			"Library/Application Support/Trae CN/User",
			"Library/Application Support/TRAE SOLO CN/User",
			// Linux
			".config/Trae/User",
			".config/Trae CN/User",
			".config/TRAE SOLO CN/User",
		},
		IDPrefix:     "trae:",
		WatchSubdirs: []string{"workspaceStorage", "globalStorage"},
		FileBased:    true,
		Usage:        UsageCapabilities{NoPerMessageTokenData: true},
	},
	{
		Type:        AgentVSCopilot,
		DisplayName: "Visual Studio Copilot",
		EnvVar:      "VISUALSTUDIO_COPILOT_DIR",
		ConfigKey:   "visualstudio_copilot_dirs",
		DefaultDirs: []string{
			// Windows
			"AppData/Local/Temp/VSGitHubCopilotLogs/traces",
			// macOS
			"Library/Caches/VSGitHubCopilotLogs/traces",
			// Linux
			".cache/VSGitHubCopilotLogs/traces",
		},
		IDPrefix:  "visualstudio-copilot:",
		FileBased: true,
		Usage: UsageCapabilities{
			NoPerMessageTokenData: true,
		},
	},
	{
		Type:        AgentPi,
		DisplayName: "Pi",
		EnvVar:      "PI_DIR",
		ConfigKey:   "pi_dirs",
		DefaultDirs: []string{".pi/agent/sessions"},
		IDPrefix:    "pi:",
		FileBased:   true,
	},
	{
		Type:        AgentOMP,
		DisplayName: "OhMyPi",
		EnvVar:      "OMP_DIR",
		ConfigKey:   "omp_dirs",
		DefaultDirs: []string{".omp/agent/sessions"},
		IDPrefix:    "omp:",
		FileBased:   true,
	},
	{
		Type:        AgentQwen,
		DisplayName: "Qwen Code",
		EnvVar:      "QWEN_PROJECTS_DIR",
		ConfigKey:   "qwen_project_dirs",
		DefaultDirs: []string{".qwen/projects"},
		IDPrefix:    "qwen:",
		// Sessions live under <projectsDir>/<encoded-project>/chats/<id>.jsonl,
		// so the projects root must be watched recursively — pinning the
		// watch to a "chats" subdir of the root catches no events.
		FileBased: true,
	},
	{
		Type:        AgentCommandCode,
		DisplayName: "Command Code",
		EnvVar:      "COMMANDCODE_PROJECTS_DIR",
		ConfigKey:   "commandcode_project_dirs",
		DefaultDirs: []string{".commandcode/projects"},
		IDPrefix:    "commandcode:",
		FileBased:   true,
	},
	{
		Type:        AgentDeepSeekTUI,
		DisplayName: "DeepSeek TUI",
		EnvVar:      "DEEPSEEK_TUI_SESSIONS_DIR",
		ConfigKey:   "deepseek_tui_sessions_dirs",
		DefaultDirs: []string{
			".codewhale/sessions",
			".deepseek/sessions",
		},
		IDPrefix:  "deepseek-tui:",
		FileBased: true,
	},
	{
		Type:        AgentOpenClaw,
		DisplayName: "OpenClaw",
		EnvVar:      "OPENCLAW_DIR",
		ConfigKey:   "openclaw_dirs",
		DefaultDirs: []string{
			".openclaw/agents",
			".kimi_openclaw/agents",
		},
		IDPrefix:  "openclaw:",
		FileBased: true,
	},
	{
		Type:        AgentQClaw,
		DisplayName: "QClaw",
		EnvVar:      "QCLAW_DIR",
		ConfigKey:   "qclaw_dirs",
		DefaultDirs: []string{".qclaw/agents"},
		IDPrefix:    "qclaw:",
		FileBased:   true,
	},
	{
		Type:        AgentKimi,
		DisplayName: "Kimi",
		EnvVar:      "KIMI_DIR",
		ConfigKey:   "kimi_dirs",
		DefaultDirs: []string{
			".kimi/sessions",
			".kimi-code/sessions",
		},
		IDPrefix:  "kimi:",
		FileBased: true,
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
		Type:        AgentKiro,
		DisplayName: "Kiro",
		EnvVar:      "KIRO_SESSIONS_DIR",
		ConfigKey:   "kiro_dirs",
		DefaultDirs: []string{
			".kiro/sessions/cli",
			".local/share/kiro-cli",
		},
		IDPrefix:  "kiro:",
		FileBased: true,
	},
	{
		Type:        AgentKiroIDE,
		DisplayName: "Kiro IDE",
		EnvVar:      "KIRO_IDE_DIR",
		ConfigKey:   "kiro_ide_dirs",
		DefaultDirs: kiroIDEDefaultDirs(),
		IDPrefix:    "kiro-ide:",
		FileBased:   true,
	},
	{
		Type:        AgentCortex,
		DisplayName: "Cortex Code",
		EnvVar:      "CORTEX_DIR",
		ConfigKey:   "cortex_dirs",
		DefaultDirs: []string{
			".snowflake/cortex/conversations",
		},
		IDPrefix:  "cortex:",
		FileBased: true,
	},
	{
		Type:                  AgentHermes,
		DisplayName:           "Hermes Agent",
		EnvVar:                "HERMES_SESSIONS_DIR",
		ConfigKey:             "hermes_sessions_dirs",
		DefaultDirs:           []string{".hermes/sessions"},
		IDPrefix:              "hermes:",
		FileBased:             true,
		WatchRootsFunc:        ResolveHermesWatchRoots,
		ShallowWatchRootsFunc: ResolveHermesShallowWatchRoots,
	},
	{
		Type:        AgentGrok,
		DisplayName: "Grok",
		EnvVar:      "GROK_DIR",
		ConfigKey:   "grok_dirs",
		DefaultDirs: []string{".grok/sessions"},
		IDPrefix:    "grok:",
		FileBased:   true,
	},
	{
		Type:        AgentWorkBuddy,
		DisplayName: "WorkBuddy",
		EnvVar:      "WORKBUDDY_PROJECTS_DIR",
		ConfigKey:   "workbuddy_project_dirs",
		DefaultDirs: []string{".workbuddy/projects"},
		IDPrefix:    "workbuddy:",
		FileBased:   true,
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
		Type:        AgentDevin,
		DisplayName: "Devin",
		EnvVar:      "DEVIN_DIR",
		ConfigKey:   "devin_dirs",
		DefaultDirs: []string{
			"Library/Application Support/devin",
			".local/share/devin",
		},
		IDPrefix:  "devin:",
		FileBased: false,
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
		IDPrefix:     "positron:",
		WatchSubdirs: []string{"workspaceStorage"},
		FileBased:    true,
	},
	{
		// Posit Assistant (posit-dev/assistant) stores one directory per
		// conversation under workspaces/<workspaceId>/<conversationId>/,
		// holding a conversation.json message tree plus an append-only
		// lm-messages.jsonl transcript. Distinct from the Positron IDE's
		// built-in Assistant above, which uses VS Code chatSessions files.
		Type:        AgentPositAssistant,
		DisplayName: "Posit Assistant",
		EnvVar:      "POSIT_ASSISTANT_DIR",
		ConfigKey:   "posit_assistant_dirs",
		DefaultDirs: []string{".posit/assistant/workspaces"},
		IDPrefix:    "posit-assistant:",
		FileBased:   true,
	},
	{
		Type:        AgentZCode,
		DisplayName: "ZCode",
		EnvVar:      "ZCODE_DIR",
		ConfigKey:   "zcode_dirs",
		DefaultDirs: []string{
			".zcode/cli/db",
			".zcode/cli",
		},
		IDPrefix:  "zcode:",
		FileBased: false,
		Usage: UsageCapabilities{
			NoPerMessageTokenData: true,
		},
	},
	{
		Type:         AgentZed,
		DisplayName:  "Zed",
		EnvVar:       "ZED_DIR",
		ConfigKey:    "zed_dirs",
		DefaultDirs:  zedDefaultDirs(),
		IDPrefix:     "zed:",
		FileBased:    true,
		WatchSubdirs: []string{"threads"},
	},
	{
		Type:        AgentAntigravity,
		DisplayName: "Antigravity",
		EnvVar:      "ANTIGRAVITY_DIR",
		ConfigKey:   "antigravity_dirs",
		DefaultDirs: []string{".gemini/antigravity"},
		IDPrefix:    "antigravity:",
		WatchSubdirs: []string{
			"conversations",
			"brain",
			"annotations",
		},
		FileBased: true,
	},
	{
		Type:        AgentAntigravityCLI,
		DisplayName: "Antigravity CLI",
		EnvVar:      "ANTIGRAVITY_CLI_DIR",
		ConfigKey:   "antigravity_cli_dirs",
		DefaultDirs: []string{".gemini/antigravity-cli"},
		IDPrefix:    "antigravity-cli:",
		WatchSubdirs: []string{
			"conversations",
			"implicit",
			"brain",
		},
		FileBased: true,
	},
	{
		Type:        AgentQwenPaw,
		DisplayName: "QwenPaw",
		EnvVar:      "QWENPAW_DIR",
		ConfigKey:   "qwenpaw_dirs",
		DefaultDirs: []string{".copaw/workspaces"},
		IDPrefix:    "qwenpaw:",
		FileBased:   true,
	},
	{
		Type:        AgentGptme,
		DisplayName: "gptme",
		EnvVar:      "GPTME_DIR",
		ConfigKey:   "gptme_dirs",
		DefaultDirs: []string{".local/share/gptme/logs"},
		IDPrefix:    "gptme:",
		FileBased:   true,
	},
	{
		Type:        AgentQoder,
		DisplayName: "Qoder",
		EnvVar:      "QODER_PROJECTS_DIR",
		ConfigKey:   "qoder_project_dirs",
		DefaultDirs: []string{
			".qoder/projects",
			".qoderwork/projects",
		},
		IDPrefix:  "qoder:",
		FileBased: true,
	},
	{
		// Shelley (exe.dev) stores all conversations in a single
		// SQLite DB at ~/.config/shelley/shelley.db. Like Zed, each
		// conversation is addressed by a virtual path (dbPath#id).
		Type:        AgentShelley,
		DisplayName: "Shelley",
		EnvVar:      "SHELLEY_DIR",
		ConfigKey:   "shelley_dirs",
		DefaultDirs: []string{".config/shelley"},
		IDPrefix:    "shelley:",
		FileBased:   true,
	},
	{
		Type:        AgentVibe,
		DisplayName: "Mistral Vibe",
		EnvVar:      "VIBE_SESSIONS_DIR",
		ConfigKey:   "vibe_session_dirs",
		DefaultDirs: []string{".vibe/logs/session"},
		IDPrefix:    "vibe:",
		FileBased:   true,
	},
	{
		// Aider has no central session store. It writes one Markdown
		// chat log per repo at <repo>/.aider.chat.history.md. There is
		// no safe canonical root: an always-on $HOME walk is prone to
		// macOS privacy prompts (Documents/Downloads/Music/Photos) during
		// passive background refreshes, and to surprising work. Users must
		// opt in by setting AIDER_DIR or the aider_dirs config key to a
		// code root they want scanned. A configured broad root such as
		// $HOME still gets the bounded, symlink-safe, depth-capped,
		// time-budgeted walk with protected-folder pruning.
		//
		// ShallowWatch is true because users can still opt into broad
		// roots; watch those roots shallowly and rely on the 15-minute
		// periodic sync to pick up new repos' history files. Aider history
		// is append-mostly, so this is an acceptable latency tradeoff.
		Type:         AgentAider,
		DisplayName:  "Aider",
		EnvVar:       "AIDER_DIR",
		ConfigKey:    "aider_dirs",
		IDPrefix:     "aider:",
		FileBased:    true,
		ShallowWatch: true,
	},
	{
		Type:         AgentReasonix,
		DisplayName:  "Reasonix",
		EnvVar:       "REASONIX_DIR",
		ConfigKey:    "reasonix_dirs",
		DefaultDirs:  []string{".reasonix", "AppData/Roaming/reasonix"},
		IDPrefix:     "reasonix:",
		WatchSubdirs: []string{"sessions", "archive", "projects"},
		FileBased:    true,
	},
	{
		Type:           AgentIcodemate,
		DisplayName:    "IcodeMate",
		EnvVar:         "ICODEMATE_DIR",
		ConfigKey:      "icodemate_dirs",
		DefaultDirs:    []string{".local/share/icodemate"},
		IDPrefix:       "icodemate:",
		WatchSubdirs:   []string{"storage/session_diff"},
		FileBased:      true,
		WatchRootsFunc: ResolveIcodemateWatchRoots,
	},
	{
		// RooCode (rooveterinaryinc.roo-cline) is a VSCode extension that
		// stores sessions in VSCode's globalStorage directory under
		// tasks/<taskId>/. Each task directory holds history_item.json
		// (metadata) and ui_messages.json (transcript). VSCode
		// canonicalizes globalStorage directory names to lowercase, so
		// the default paths must use the lowercase extension ID to be
		// discoverable on case-sensitive Linux filesystems. RooCode was
		// shut down on May 15, 2026; ZooCode is the active community fork.
		Type:        AgentRooCode,
		DisplayName: "RooCode",
		EnvVar:      "ROOCODE_DIR",
		ConfigKey:   "roocode_dirs",
		DefaultDirs: []string{
			// macOS
			"Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline",
			// Linux
			".config/Code/User/globalStorage/rooveterinaryinc.roo-cline",
			// Windows
			"AppData/Roaming/Code/User/globalStorage/rooveterinaryinc.roo-cline",
		},
		IDPrefix:  "roocode:",
		FileBased: true,
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

// AgentNameLacksPerMessageTokenData reports whether the named agent
// records no per-message token data. Names match registry types
// exactly and unknown names fail closed; CSV filter parsing trims its
// parts before calling.
func AgentNameLacksPerMessageTokenData(agent string) bool {
	def, ok := AgentByType(AgentType(agent))
	return ok && def.Usage.NoPerMessageTokenData
}

// AgentFilterLacksPerMessageTokenData reports whether a (possibly
// comma-separated) agent filter selects only agents without
// per-message token data, with at least one entry.
func AgentFilterLacksPerMessageTokenData(agentFilter string) bool {
	return agentFilterMatches(agentFilter, AgentNameLacksPerMessageTokenData)
}

func agentFilterMatches(agentFilter string, match func(string) bool) bool {
	matched := false
	for part := range strings.SplitSeq(agentFilter, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !match(part) {
			return false
		}
		matched = true
	}
	return matched
}

// AgentIsCopilot reports whether t is one of the GitHub Copilot
// family agents. Copilot-specific user-facing wording keys on this
// identity; the usage capabilities above intentionally do not imply
// it, so a future agent can adopt NoPerMessageTokenData without
// inheriting Copilot messaging.
func AgentIsCopilot(t AgentType) bool {
	switch t {
	case AgentCopilot, AgentVSCodeCopilot, AgentVSCopilot:
		return true
	}
	return false
}

// AgentNameIsCopilot reports whether the agent name identifies a
// Copilot-family agent. Names match exactly, like the capability
// helpers above.
func AgentNameIsCopilot(agent string) bool {
	return AgentIsCopilot(AgentType(agent))
}

// AgentFilterIsCopilot reports whether a (possibly comma-separated)
// agent filter selects only Copilot-family agents, with at least one
// entry.
func AgentFilterIsCopilot(agentFilter string) bool {
	return agentFilterMatches(agentFilter, AgentNameIsCopilot)
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
	// RoleSystem and RoleTool are emitted by several parsers (for
	// system-injected notices and standalone tool-result records) and
	// persist to the messages table, so they are part of the known
	// role enum even though the user/assistant pair carries the common
	// case.
	RoleSystem RoleType = "system"
	RoleTool   RoleType = "tool"
)

// Transcript fidelity values for ParsedSession.TranscriptFidelity. Empty
// is treated as full (no degradation signalled).
const (
	TranscriptFidelityFull    = "full"
	TranscriptFidelitySummary = "summary"
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
	AgentLabel       string
	Entrypoint       string
	ParentSessionID  string
	RelationshipType RelationshipType
	Cwd              string
	GitBranch        string
	SourceSessionID  string
	SourceVersion    string
	// TranscriptFidelity classifies how complete a stored transcript is
	// relative to the agent's full session data: "full" when the
	// high-resolution source was used, "summary" for a degraded/fallback
	// decode. Empty means full (parser did not classify). Currently set
	// only by the Antigravity CLI parser.
	TranscriptFidelity string
	// GenMetadataWithoutUsage reports whether this Antigravity session's steps
	// table carried gen_metadata rows but none decoded into a usage event --
	// an early warning that a newer agy build changed the gen_metadata wire
	// format the token-block heuristic depends on. Set by both Antigravity
	// parsers; false for every other agent.
	GenMetadataWithoutUsage bool
	MalformedLines          int
	IsTruncated             bool
	FirstMessage            string
	SessionName             string
	StartedAt               time.Time
	EndedAt                 time.Time
	MessageCount            int
	UserMessageCount        int
	File                    FileInfo

	// TerminationStatus describes how the session appears to have
	// ended. Empty string = unknown (parser did not classify, or
	// agent format does not yet support classification).
	TerminationStatus TerminationStatus

	TotalOutputTokens    int
	PeakContextTokens    int
	HasTotalOutputTokens bool
	HasPeakContextTokens bool

	// UsageEvents carries parser-emitted aggregate usage rows for
	// agents whose session-level accounting is computed inline
	// (e.g. VSCode Copilot). The sync engine forwards these into
	// the usage_events table for catalog-based cost pricing.
	UsageEvents []ParsedUsageEvent

	// CountsAuthoritative marks parsers that own MessageCount and
	// UserMessageCount even when they intentionally emit no transcript rows.
	CountsAuthoritative bool

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
	FilePath          string // resolved edit/write target path, when known natively
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

// ParsedUsageEvent records session-level usage emitted by parsers
// when an agent exposes aggregate accounting instead of per-message
// token_usage rows.
type ParsedUsageEvent struct {
	SessionID                string
	MessageOrdinal           *int
	Source                   string
	Model                    string
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	ReasoningTokens          int
	CostUSD                  *float64
	CostStatus               string
	CostSource               string
	OccurredAt               string
	DedupKey                 string
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

// applyUsageEventTokenTotals recomputes session token totals from the
// usage-event set whenever events exist. Callers must only use it when
// events are a superset of per-message token metadata — true for the
// Antigravity gen_metadata parsers, where every token-bearing message
// derives from a gen row that also emits an event and undecodable
// steps emit events with no message. Deriving totals from events
// therefore covers transcripts that dropped steps (sidecar wins,
// undecodable rows) without double counting. Message-derived totals
// are kept where the events are silent.
//
// Peak context counts the full context window per event: fresh input
// plus cache-creation and cache-read tokens. That keeps event-derived
// session totals consistent with per-message ContextTokens attribution
// (input + cacheRead) from parsers whose events carry cache fields,
// such as the Antigravity CLI sidecar parser.
func applyUsageEventTokenTotals(
	sess *ParsedSession,
	events []ParsedUsageEvent,
) {
	totalOutput, hasOutput, peakContext, hasContext :=
		UsageEventTokenAggregate(events)
	if hasOutput {
		sess.HasTotalOutputTokens = true
		sess.TotalOutputTokens = totalOutput
	}
	if hasContext {
		sess.HasPeakContextTokens = true
		sess.PeakContextTokens = peakContext
	}
}

// UsageEventTokenAggregate is the canonical event-derived token rollup:
// the sum of POSITIVE per-event output tokens and the peak per-event full
// context (input + cache-creation + cache-read) where that context is
// positive, each with a presence flag. It is the single source of truth
// shared by applyUsageEventTokenTotals (parser side) and the sync layer's
// post-sanitize aggregate reconciliation, so the two never drift: a value
// that did not contribute to the stored aggregate (zero or negative) is
// excluded on both sides, before and after clamping.
func UsageEventTokenAggregate(
	events []ParsedUsageEvent,
) (totalOut int, hasOut bool, peakCtx int, hasCtx bool) {
	for _, ev := range events {
		if ev.OutputTokens > 0 {
			hasOut = true
			totalOut += ev.OutputTokens
		}
		context := ev.InputTokens +
			ev.CacheCreationInputTokens +
			ev.CacheReadInputTokens
		if context > 0 {
			hasCtx = true
			if context > peakCtx {
				peakCtx = context
			}
		}
	}
	return totalOut, hasOut, peakCtx, hasCtx
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
	Session     ParsedSession
	Messages    []ParsedMessage
	UsageEvents []ParsedUsageEvent
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
