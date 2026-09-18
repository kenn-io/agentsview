package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInferTokenPresence(t *testing.T) {
	tests := []struct {
		name        string
		tokenUsage  []byte
		contextToks int
		outputToks  int
		hasContext  bool
		hasOutput   bool
		wantCtx     bool
		wantOut     bool
	}{
		{
			name:       "explicit flags preserved, no data",
			hasContext: true,
			hasOutput:  true,
			wantCtx:    true,
			wantOut:    true,
		},
		{
			name:        "non-zero contextTokens infers presence",
			contextToks: 1000,
			wantCtx:     true,
			wantOut:     false,
		},
		{
			name:       "non-zero outputTokens infers presence",
			outputToks: 42,
			wantCtx:    false,
			wantOut:    true,
		},
		{
			name:    "zero numerics, no flags -> false/false",
			wantCtx: false,
			wantOut: false,
		},
		{
			name:       "json input_tokens key",
			tokenUsage: []byte(`{"input_tokens": 100}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "json output_tokens key",
			tokenUsage: []byte(`{"output_tokens": 50}`),
			wantCtx:    false,
			wantOut:    true,
		},
		{
			name:       "json cache_read_input_tokens key",
			tokenUsage: []byte(`{"cache_read_input_tokens": 200}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "json cache_creation_input_tokens key",
			tokenUsage: []byte(`{"cache_creation_input_tokens": 10}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "json both sides",
			tokenUsage: []byte(`{"input_tokens": 100, "output_tokens": 50}`),
			wantCtx:    true,
			wantOut:    true,
		},
		{
			name:       "malformed json ignored",
			tokenUsage: []byte(`not-json`),
			wantCtx:    false,
			wantOut:    false,
		},
		{
			name:       "empty json object",
			tokenUsage: []byte(`{}`),
			wantCtx:    false,
			wantOut:    false,
		},
		{
			name:       "gemini style input key",
			tokenUsage: []byte(`{"input": 300}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "gemini style output key",
			tokenUsage: []byte(`{"output": 75}`),
			wantCtx:    false,
			wantOut:    true,
		},
		{
			name:       "context_tokens json key",
			tokenUsage: []byte(`{"context_tokens": 500}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "cached json key",
			tokenUsage: []byte(`{"cached": 30}`),
			wantCtx:    true,
			wantOut:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCtx, gotOut := InferTokenPresence(
				tt.tokenUsage,
				tt.contextToks,
				tt.outputToks,
				tt.hasContext,
				tt.hasOutput,
			)
			assert.Equal(t, tt.wantCtx, gotCtx, "InferTokenPresence context")
			assert.Equal(t, tt.wantOut, gotOut, "InferTokenPresence output")
		})
	}
}

func TestAgentUsageCapabilities(t *testing.T) {
	tests := []struct {
		name        string
		agent       AgentType
		wantNoToken bool
		wantCredits bool
	}{
		{
			name:        "copilot",
			agent:       AgentCopilot,
			wantNoToken: true,
			wantCredits: true,
		},
		{
			name:        "vscode copilot",
			agent:       AgentVSCodeCopilot,
			wantNoToken: true,
			wantCredits: true,
		},
		{
			name:        "visual studio copilot",
			agent:       AgentVSCopilot,
			wantNoToken: true,
			wantCredits: true,
		},
		{
			name:  "claude",
			agent: AgentClaude,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantNoToken,
				AgentNameLacksPerMessageTokenData(string(tc.agent)))
			assert.Equal(t, tc.wantCredits,
				AgentNameUsesAICredits(string(tc.agent)))
		})
	}
}

func TestAgentCopilotIdentity(t *testing.T) {
	tests := []struct {
		name  string
		agent AgentType
		want  bool
	}{
		{"copilot", AgentCopilot, true},
		{"vscode copilot", AgentVSCodeCopilot, true},
		{"visual studio copilot", AgentVSCopilot, true},
		{"claude", AgentClaude, false},
		{"unknown", AgentType("unknown-agent"), false},
		{"empty", AgentType(""), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AgentIsCopilot(tc.agent))
			assert.Equal(t, tc.want, AgentNameIsCopilot(string(tc.agent)))
		})
	}
}

func TestAgentFilterIsCopilot(t *testing.T) {
	tests := []struct {
		name   string
		filter string
		want   bool
	}{
		{"empty", "", false},
		{"single copilot", "copilot", true},
		{"all-copilot CSV with spaces", " copilot , visualstudio-copilot ", true},
		{"trailing comma", "copilot,vscode-copilot,", true},
		{"mixed CSV", "copilot,claude", false},
		{"only commas", ",", false},
		{"single non-copilot", "claude", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AgentFilterIsCopilot(tc.filter))
		})
	}
}

func TestAgentByType(t *testing.T) {
	tests := []struct {
		input AgentType
		want  bool
	}{
		{AgentClaude, true},
		{AgentOpenClaude, true},
		{AgentCodex, true},
		{AgentCopilot, true},
		{AgentGemini, true},
		{AgentMiMoCode, true},
		{AgentOpenCode, true},
		{AgentOpenCodeReview, true},
		{AgentOpenHands, true},
		{AgentCursor, true},
		{AgentAmp, true},
		{AgentVSCodeCopilot, true},
		{AgentPi, true},
		{AgentPrimeAgent, true},
		{AgentOMP, true},
		{AgentDevin, true},
		{AgentDeepSeekTUI, true},
		{AgentDeepSeekHarness, true},
		{AgentCline, true},
		{"unknown", false},
	}
	for _, tt := range tests {
		def, ok := AgentByType(tt.input)
		assert.Equalf(t, tt.want, ok, "AgentByType(%q) ok", tt.input)
		if ok {
			assert.Equalf(t, tt.input, def.Type, "AgentByType(%q).Type", tt.input)
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
			"openclaude prefix",
			"openclaude:session-id",
			AgentOpenClaude,
			true,
		},
		{
			"codex prefix",
			"codex:some-uuid",
			AgentCodex,
			true,
		},
		{
			"traex prefix",
			"traex:some-uuid",
			AgentTraeX,
			true,
		},
		{
			"augure-code prefix",
			"augure-code:some-uuid",
			AgentAugureCode,
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
			"gemini apps prefix",
			"gemini-apps:sess-id",
			AgentGeminiApps,
			true,
		},
		{
			"mimocode prefix",
			"mimocode:sess-id",
			AgentMiMoCode,
			true,
		},
		{
			"opencode prefix",
			"opencode:sess-id",
			AgentOpenCode,
			true,
		},
		{
			"openhands prefix",
			"openhands:sess-id",
			AgentOpenHands,
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
			"visualstudio-copilot prefix",
			"visualstudio-copilot:sess-id",
			AgentVSCopilot,
			true,
		},
		{
			"pi prefix",
			"pi:pi-session-uuid",
			AgentPi,
			true,
		},
		{
			"prime agent prefix",
			"prime-agent:019c1234-session",
			AgentPrimeAgent,
			true,
		},
		{
			"omp prefix",
			"omp:omp-session-uuid",
			AgentOMP,
			true,
		},
		{
			"devin prefix",
			"devin:session-id",
			AgentDevin,
			true,
		},
		{
			"zed prefix",
			"zed:sess-id",
			AgentZed,
			true,
		},
		{
			"qwenpaw prefix",
			"qwenpaw:default:sess-id",
			AgentQwenPaw,
			true,
		},
		{
			// Lock in the disjoint prefix: "qwenpaw:" must NOT be
			// swallowed by the "qwen:" rule (no shared stem), so
			// QwenPaw IDs never route to the Qwen agent.
			"qwen prefix does not capture qwenpaw",
			"qwen:sess-id",
			AgentQwen,
			true,
		},
		{
			"deepseek tui prefix",
			"deepseek-tui:sess-id",
			AgentDeepSeekTUI,
			true,
		},
		{
			"deepseek harness prefix",
			"deepseek-harness:sess-id",
			AgentDeepSeekHarness,
			true,
		},
		{
			"qoder prefix",
			"qoder:sess-id",
			AgentQoder,
			true,
		},
		{
			"cline prefix",
			"cline:sess-id",
			AgentCline,
			true,
		},
		{
			"remote deepseek tui prefix",
			"devbox~deepseek-tui:sess-id",
			AgentDeepSeekTUI,
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
			require.Equalf(t, tt.wantOK, ok, "AgentByPrefix(%q) ok", tt.sessionID)
			if ok {
				assert.Equalf(t, tt.wantType, def.Type,
					"AgentByPrefix(%q).Type", tt.sessionID)
			}
		})
	}
}

func TestRegistryCompleteness(t *testing.T) {
	assert := assert.New(t)

	// allTypes is the canonical list of every supported agent. It must match
	// Registry exactly in both directions: the assertions below fail if an
	// agent is registered without being listed here (or vice versa), so a new
	// AgentDef cannot silently bypass this check the way several agents
	// previously did.
	allTypes := []AgentType{
		AgentClaude,
		AgentOpenClaude,
		AgentCowork,
		AgentCodex,
		AgentTraeX,
		AgentAugureCode,
		AgentCopilot,
		AgentGemini,
		AgentGeminiApps,
		AgentMiMoCode,
		AgentOpenCode,
		AgentOpenCodeReview,
		AgentKilo,
		AgentKiloLegacy,
		AgentOpenHands,
		AgentCursor,
		AgentCursorIDE,
		AgentAmp,
		AgentVSCodeCopilot,
		AgentWindsurf,
		AgentTrae,
		AgentVSCopilot,
		AgentPi,
		AgentTau,
		AgentPrimeAgent,
		AgentOMP,
		AgentQwen,
		AgentCommandCode,
		AgentDeepSeekTUI,
		AgentDeepSeekHarness,
		AgentOpenClaw,
		AgentQClaw,
		AgentKimi,
		AgentKimiWork,
		AgentClaudeAI,
		AgentChatGPT,
		AgentKiro,
		AgentKiroIDE,
		AgentCortex,
		AgentHermes,
		AgentGrok,
		AgentGoose,
		AgentForge,
		AgentDevin,
		AgentPiebald,
		AgentWarp,
		AgentPositron,
		AgentPositAssistant,
		AgentZCode,
		AgentZed,
		AgentAntigravity,
		AgentAntigravityCLI,
		AgentIflow,
		AgentIcodemate,
		AgentWorkBuddy,
		AgentCodeBuddy,
		AgentZencoder,
		AgentGptme,
		AgentQoder,
		AgentQwenPaw,
		AgentShelley,
		AgentVibe,
		AgentAider,
		AgentEvener,
		AgentReasonix,
		AgentRooCode,
		AgentCline,
		AgentPoolside,
		AgentOmnigent,
		AgentCodebuff,
		AgentCrush,
	}

	expected := make(map[AgentType]bool, len(allTypes))
	for _, at := range allTypes {
		assert.Falsef(expected[at], "AgentType %q listed more than once in allTypes", at)
		expected[at] = true
	}

	registered := make(map[AgentType]bool, len(Registry))
	for _, def := range Registry {
		assert.Falsef(registered[def.Type],
			"AgentType %q registered more than once in Registry", def.Type)
		registered[def.Type] = true
	}

	// Every listed agent must be registered.
	for at := range expected {
		assert.Truef(registered[at], "AgentType %q missing from Registry", at)
	}
	// Every registered agent must be listed, so additions to Registry cannot
	// silently skip this completeness check.
	for at := range registered {
		assert.Truef(expected[at],
			"AgentType %q registered but not listed in allTypes (add it to TestRegistryCompleteness)", at)
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
			require.Len(t, tt.inputs, len(tt.want), "inputs len")
			for i, r := range tt.inputs {
				assert.Equalf(t, tt.want[i], r.Session.RelationshipType,
					"inputs[%d].RelationshipType", i)
			}
		})
	}
}

func TestZedRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentZed)
	require.True(ok, "AgentZed missing from Registry")
	require.True(def.FileBased, "Zed FileBased")
	assert.Equal("ZED_DIR", def.EnvVar)
	assert.Equal("zed_dirs", def.ConfigKey)
	assert.Equal("zed:", def.IDPrefix)
}

func TestZCodeRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentZCode)
	require.True(ok, "AgentZCode missing from Registry")
	require.False(def.FileBased, "ZCode FileBased")
	assert.Equal("ZCODE_DIR", def.EnvVar)
	assert.Equal("zcode_dirs", def.ConfigKey)
	assert.Equal([]string{".zcode/cli/db", ".zcode/cli"}, def.DefaultDirs)
	assert.Equal("zcode:", def.IDPrefix)
	assert.True(def.Usage.NoPerMessageTokenData)
}

func TestShelleyRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentShelley)
	require.True(ok, "AgentShelley missing from Registry")
	require.True(def.FileBased, "Shelley FileBased")
	assert.Equal("SHELLEY_DIR", def.EnvVar)
	assert.Equal("shelley_dirs", def.ConfigKey)
	assert.Equal("shelley:", def.IDPrefix)
}

func TestOmnigentRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentOmnigent)
	require.True(ok, "AgentOmnigent missing from Registry")
	require.True(def.FileBased, "Omnigent FileBased")
	assert.Equal("OMNIGENT_DIR", def.EnvVar)
	assert.Equal("omnigent_dirs", def.ConfigKey)
	assert.Equal("omnigent:", def.IDPrefix)
	require.Equal([]string{".omnigent"}, def.DefaultDirs)
}

func TestOpenCodeRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentOpenCode)
	require.True(t, ok, "AgentOpenCode missing from Registry")
	require.True(t, def.FileBased, "OpenCode FileBased")
	want := []string{
		"storage/session",
		"storage/message",
		"storage/part",
	}
	require.Truef(t, slices.Equal(def.WatchSubdirs, want),
		"OpenCode WatchSubdirs = %v, want %v", def.WatchSubdirs, want)
}

func TestOpenCodeReviewRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentOpenCodeReview)
	require.True(ok, "AgentOpenCodeReview missing from Registry")
	require.True(def.FileBased, "Open Code Review FileBased")
	assert.Equal("Open Code Review", def.DisplayName)
	assert.Equal("OPENCODEREVIEW_DIR", def.EnvVar)
	assert.Equal("opencodereview_dirs", def.ConfigKey)
	assert.Equal([]string{".opencodereview/sessions"}, def.DefaultDirs)
	assert.Equal("opencodereview:", def.IDPrefix)
}

func TestOpenClaudeRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentOpenClaude)
	require.True(ok, "AgentOpenClaude missing from Registry")
	require.True(def.FileBased, "OpenClaude FileBased")
	assert.Equal("OPENCLAUDE_PROJECTS_DIR", def.EnvVar)
	assert.Equal("openclaude_project_dirs", def.ConfigKey)
	assert.Equal([]string{".openclaude/projects"}, def.DefaultDirs)
	assert.Equal("openclaude:", def.IDPrefix)
}

func TestCoworkRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentCowork)
	require.True(ok, "AgentCowork missing from Registry")
	require.True(def.FileBased, "Cowork FileBased")
	assert.Equal("COWORK_DIR", def.EnvVar)
	assert.Equal("cowork_dirs", def.ConfigKey)
	assert.Equal("cowork:", def.IDPrefix)
	assert.Equal(coworkDefaultDirs(), def.DefaultDirs)
	assert.True(def.ShallowWatch,
		"Cowork root contains large local_* working trees that discovery skips")
}

func TestPeriodicReconcileCapability(t *testing.T) {
	assert := assert.New(t)

	optedIn := map[AgentType]bool{}
	for _, def := range Registry {
		optedIn[def.Type] = def.PeriodicReconcile
	}
	// Shallow-watched providers rely on scheduled reconciliation because
	// subdirectory changes are invisible to their shallow watch coverage.
	assert.True(optedIn[AgentOpenHands])
	assert.True(optedIn[AgentAider])
	// Omnigent's watcher scans only members at or past the stored
	// updated_at floor, so metadata-only edits and deletions rely on the
	// scheduled fingerprint-gated container reparse.
	assert.True(optedIn[AgentOmnigent])
	// Codebuff's recursive per-project watch covers existing projects;
	// scheduled reconciliation picks up newly created project
	// directories under the root (see codebuffWatchRoots).
	assert.True(optedIn[AgentCodebuff])
	// Cowork's provider WatchPlan registers its root recursively
	// (coworkWatchRoots Recursive:true overrides legacy ShallowWatch), so
	// scheduled reconciliation would redundantly rescan the whole archive.
	assert.False(optedIn[AgentCowork])
	// Recursive session roots must NOT opt in: their shallow roots are
	// supplemental (codex_provider.go WatchPlan registers Recursive:true),
	// so scheduled reconciliation would rescan the whole session tree.
	assert.False(optedIn[AgentCodex])
	assert.False(optedIn[AgentHermes])
	assert.False(optedIn[AgentClaude])
	assert.False(optedIn[AgentGemini])
}

func TestRemoteSyncExcludedCapability(t *testing.T) {
	assert := assert.New(t)

	excluded := map[AgentType]bool{}
	for _, def := range Registry {
		excluded[def.Type] = def.RemoteSyncExcluded
	}
	// Trae's modern layout stores sessions as encrypted state that a remote
	// machine cannot read, so it opts out of every remote sync artifact.
	assert.True(excluded[AgentTrae])
	// Omnigent's chat.db co-locates transcripts with authentication
	// secrets, so its source tree never leaves the machine.
	assert.True(excluded[AgentOmnigent])
	assert.False(excluded[AgentClaude])
	assert.False(excluded[AgentCodex])
}

func TestRemoteSyncExcludedAgent(t *testing.T) {
	assert := assert.New(t)

	assert.True(RemoteSyncExcludedAgent(AgentTrae))
	assert.True(RemoteSyncExcludedAgent(AgentOmnigent))
	assert.False(RemoteSyncExcludedAgent(AgentClaude))
	assert.False(RemoteSyncExcludedAgent(AgentType("unknown-agent")))
}

func TestAgentByPrefixCowork(t *testing.T) {
	def, ok := AgentByPrefix("cowork:c0000000-0000-4000-8000-000000000001")
	require.True(t, ok, "cowork-prefixed ID should resolve")
	assert.Equal(t, AgentCowork, def.Type)
}

func TestMiMoCodeRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentMiMoCode)
	require.True(ok, "AgentMiMoCode missing from Registry")
	require.True(def.FileBased, "MiMoCode FileBased")
	assert.Equal("MIMOCODE_DIR", def.EnvVar)
	assert.Equal("mimocode_dirs", def.ConfigKey)
	assert.Equal([]string{".local/share/mimocode"}, def.DefaultDirs)
	assert.Equal("mimocode:", def.IDPrefix)
	want := []string{
		"storage/session_diff",
		"storage/message",
		"storage/part",
	}
	require.Truef(slices.Equal(def.WatchSubdirs, want),
		"MiMoCode WatchSubdirs = %v, want %v", def.WatchSubdirs, want)
}

func TestCommandCodeRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentCommandCode)
	require.True(ok, "AgentCommandCode missing from Registry")
	require.True(def.FileBased, "Command Code FileBased")
	assert.Equal([]string{".commandcode/projects"}, def.DefaultDirs)
	assert.Equal("commandcode:", def.IDPrefix)
}

func TestDevinRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentDevin)
	require.True(ok, "AgentDevin missing from Registry")
	require.False(def.FileBased, "Devin FileBased")
	assert.Equal("Devin", def.DisplayName)
	assert.Equal("DEVIN_DIR", def.EnvVar)
	assert.Equal("devin_dirs", def.ConfigKey)
	assert.Equal([]string{
		"Library/Application Support/devin",
		".local/share/devin",
	}, def.DefaultDirs)
	assert.Equal("devin:", def.IDPrefix)
}

func TestDeepSeekTUIRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentDeepSeekTUI)
	require.True(ok, "AgentDeepSeekTUI missing from Registry")
	require.True(def.FileBased, "DeepSeek TUI FileBased")
	assert.Equal("DeepSeek TUI", def.DisplayName)
	assert.Equal("DEEPSEEK_TUI_SESSIONS_DIR", def.EnvVar)
	assert.Equal("deepseek_tui_sessions_dirs", def.ConfigKey)
	assert.Equal([]string{".codewhale/sessions", ".deepseek/sessions"}, def.DefaultDirs)
	assert.Equal("deepseek-tui:", def.IDPrefix)
}

func TestDeepSeekHarnessRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentDeepSeekHarness)
	require.True(ok, "AgentDeepSeekHarness missing from Registry")
	require.True(def.FileBased, "DeepSeek Harness FileBased")
	assert.Equal("DeepSeek Harness", def.DisplayName)
	assert.Equal("DEEPSEEK_HARNESS_SESSIONS_DIR", def.EnvVar)
	assert.Equal("DSH_HOME", def.DefaultRootEnvVar)
	assert.Equal("deepseek_harness_sessions_dirs", def.ConfigKey)
	assert.Equal([]string{".dsh/sessions"}, def.DefaultDirs)
	assert.Equal("deepseek-harness:", def.IDPrefix)
}

func TestResolveOpenCodeSourcePrefersStorage(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	sessionDir := filepath.Join(root, "storage", "session", "global")
	require.NoError(os.MkdirAll(sessionDir, 0o755), "mkdir session dir")
	dbPath := filepath.Join(root, "opencode.db")
	require.NoError(os.WriteFile(dbPath, []byte("x"), 0o644), "write db marker")

	got := ResolveOpenCodeSource(root)
	require.Equal(OpenCodeSourceStorage, got.Mode, "Mode")
	require.Equal(filepath.Join(root, "storage", "session"), got.SessionRoot, "SessionRoot")
}

func TestResolveMiMoCodeSourcePrefersStorage(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	dir := filepath.Join(root, "storage", "session_diff", "global")
	require.NoError(os.MkdirAll(dir, 0o755), "mkdir")
	dbPath := filepath.Join(root, "mimocode.db")
	require.NoError(os.WriteFile(dbPath, []byte("x"), 0o644), "write db marker")

	src := ResolveMiMoCodeSource(root)
	require.Equal(OpenCodeSourceStorage, src.Mode, "Mode")
	require.Equal(filepath.Join(root, "storage", "session_diff"), src.SessionRoot)

	path := filepath.Join(dir, "ses_test.json")
	require.NoError(os.WriteFile(path,
		[]byte(`{"id":"ses_test","directory":"/home/user/code/my-app"}`),
		0o644))

	discovered := discoverOpenCodeFormatSessions(mimoFmt, root)
	require.Len(discovered, 1)
	require.Equal(AgentMiMoCode, discovered[0].Agent)

	require.Equal(path, findOpenCodeFormatSourceFile(t.Context(), mimoFmt, root, "ses_test"))
}

func TestResolveOpenCodeSourceFallsBackToSQLiteOnBrokenStoragePath(
	t *testing.T,
) {
	require := require.New(t)

	root := t.TempDir()
	storagePath := filepath.Join(root, "storage")
	require.NoError(os.WriteFile(storagePath, []byte("x"), 0o644), "write storage marker")
	dbPath := filepath.Join(root, "opencode.db")
	require.NoError(os.WriteFile(dbPath, []byte("x"), 0o644), "write db marker")

	got := ResolveOpenCodeSource(root)
	require.Equal(OpenCodeSourceSQLite, got.Mode, "Mode")
	require.Equal(dbPath, got.DBPath, "DBPath")
}

func TestResolveOpenCodeSourceKeepsStorageAuthoritativeWhenUnreadable(
	t *testing.T,
) {
	require := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on Windows")
	}
	root := t.TempDir()
	sessionDir := filepath.Join(root, "storage", "session", "global")
	require.NoError(os.MkdirAll(sessionDir, 0o755), "mkdir session dir")
	storageRoot := filepath.Join(root, "storage")
	require.NoError(os.Chmod(storageRoot, 0o000), "chmod storage root")
	defer func() {
		_ = os.Chmod(storageRoot, 0o755)
	}()
	dbPath := filepath.Join(root, "opencode.db")
	require.NoError(os.WriteFile(dbPath, []byte("x"), 0o644), "write db marker")

	got := ResolveOpenCodeSource(root)
	require.Equal(OpenCodeSourceStorage, got.Mode, "Mode")
	require.Equal(filepath.Join(root, "storage", "session"), got.SessionRoot, "SessionRoot")
}

func TestDiscoverOpenCodeSessions(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	dir := filepath.Join(root, "storage", "session", "global")
	require.NoError(os.MkdirAll(dir, 0o755), "mkdir")
	path := filepath.Join(dir, "ses_test.json")
	data := []byte(`{"id":"ses_test","directory":"/home/user/code/my-app"}`)
	require.NoError(os.WriteFile(path, data, 0o644), "write session")

	got := discoverOpenCodeFormatSessions(openCodeFmt, root)
	require.Len(got, 1, "len")
	require.Equal(path, got[0].Path, "Path")
	require.Equal("my_app", got[0].Project, "Project")
	require.Equal(AgentOpenCode, got[0].Agent, "Agent")
}

func TestDiscoverOpenCodeSessionsIgnoresNestedJSON(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	dir := filepath.Join(root, "storage", "session", "global")
	require.NoError(os.MkdirAll(filepath.Join(dir, "nested"), 0o755), "mkdir")
	path := filepath.Join(dir, "ses_test.json")
	require.NoError(os.WriteFile(path, []byte(`{"id":"ses_test"}`), 0o644), "write session")
	require.NoError(os.WriteFile(filepath.Join(dir, "nested", "meta.json"), []byte(`{"id":"meta"}`), 0o644), "write nested json")

	got := discoverOpenCodeFormatSessions(openCodeFmt, root)
	require.Len(got, 1, "len")
	require.Equal(path, got[0].Path, "Path")
}

func TestFindOpenCodeSourceFilePrefersStorage(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "storage", "session", "global", "ses_123.json")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755), "mkdir")
	require.NoError(os.WriteFile(path, []byte(`{"id":"ses_123"}`), 0o644), "write session")
	require.NoError(os.WriteFile(filepath.Join(root, "opencode.db"), []byte("x"), 0o644), "write db marker")

	got := findOpenCodeFormatSourceFile(t.Context(), openCodeFmt, root, "ses_123")
	require.Equal(path, got, "FindOpenCodeSourceFile()")
}

func TestFindOpenCodeSourceFileFallsBackToSQLiteInHybridRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session", "global"),
		0o755,
	), "mkdir session dir")
	dbPath := filepath.Join(root, "opencode.db")
	seedHybridSQLiteDB(t, dbPath, "ses_456")

	got := findOpenCodeFormatSourceFile(t.Context(), openCodeFmt, root, "ses_456")
	want := OpenCodeSQLiteVirtualPath(dbPath, "ses_456")
	require.Equal(t, want, got, "FindOpenCodeSourceFile()")
}

// TestFindOpenCodeSourceFileReturnsEmptyWhenSessionMissing covers
// the multi-root shadowing case: an early hybrid root with an
// opencode.db file that does NOT contain the session must return
// "" so the engine's FindSourceFile loop continues to later roots
// where the session actually lives.
func TestFindOpenCodeSourceFileReturnsEmptyWhenSessionMissing(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session", "global"),
		0o755,
	), "mkdir session dir")
	dbPath := filepath.Join(root, "opencode.db")
	seedHybridSQLiteDB(t, dbPath, "ses_unrelated")

	got := findOpenCodeFormatSourceFile(t.Context(), openCodeFmt, root, "ses_missing")
	assert.Empty(t, got, "FindOpenCodeSourceFile()")
}

func TestFindOpenCodeSourceFilePureSQLiteOnlyForExistingSession(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	seedHybridSQLiteDB(t, dbPath, "ses_present")

	got := findOpenCodeFormatSourceFile(t.Context(), openCodeFmt, root, "ses_present")
	assert.Equal(t,
		OpenCodeSQLiteVirtualPath(dbPath, "ses_present"),
		got, "FindOpenCodeSourceFile(present)")
	got = findOpenCodeFormatSourceFile(t.Context(), openCodeFmt, root, "ses_absent")
	assert.Empty(t, got, "FindOpenCodeSourceFile(absent)")
}

func TestResolveCodexShallowWatchRoots(t *testing.T) {
	tests := []struct {
		name string
		root string
		want []string
	}{
		{
			name: "sessions dir",
			root: filepath.Join("home", ".codex", "sessions"),
			want: []string{filepath.Join("home", ".codex")},
		},
		{
			name: "archived sessions dir",
			root: filepath.Join("home", ".codex", "archived_sessions"),
			want: []string{filepath.Join("home", ".codex")},
		},
		{
			name: "empty root",
			root: "",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveCodexShallowWatchRoots(tt.root)
			assert.Truef(t, slices.Equal(got, tt.want),
				"ResolveCodexShallowWatchRoots(%q) = %v, want %v",
				tt.root, got, tt.want)
		})
	}
}

func TestCodexDefShallowWatchesIndexParent(t *testing.T) {
	var def AgentDef
	found := false
	for _, d := range Registry {
		if d.Type == AgentCodex {
			def = d
			found = true
			break
		}
	}
	require.True(t, found, "Codex agent def must exist")
	require.NotNil(t, def.ShallowWatchRootsFunc,
		"Codex must watch its index parent shallowly")
	got := def.ShallowWatchRootsFunc(
		filepath.Join("home", ".codex", "sessions"),
	)
	want := []string{filepath.Join("home", ".codex")}
	assert.Truef(t, slices.Equal(got, want),
		"Codex ShallowWatchRootsFunc = %v, want %v", got, want)
}

func TestResolveOpenCodeWatchRootsStorage(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session", "global"),
		0o755,
	), "mkdir session dir")

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{filepath.Join(root, "storage")}
	assert.Truef(t, slices.Equal(got, want),
		"ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
}

func TestResolveOpenCodeWatchRootsHybrid(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session", "global"),
		0o755,
	), "mkdir session dir")
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("x"), 0o644,
	), "write db marker")

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{root}
	assert.Truef(t, slices.Equal(got, want),
		"ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
}

// A fresh opencode install may only have storage/session at startup;
// message/ and part/ get created lazily when the first message is
// written. Returning storage/ as the watch root ensures the watcher's
// Create handler picks up those lazy subdirs without a restart.
func TestResolveOpenCodeWatchRootsStorageMissingSubdirs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session"),
		0o755,
	), "mkdir session dir")

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{filepath.Join(root, "storage")}
	assert.Truef(t, slices.Equal(got, want),
		"ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
}

func TestResolveOpenCodeWatchRootsSQLite(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("x"), 0o644,
	), "write db marker")

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{root}
	assert.Truef(t, slices.Equal(got, want),
		"ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
}

func TestResolveOpenCodeWatchRootsMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name    string
		resolve func(string) []string
	}{
		{name: "opencode", resolve: ResolveOpenCodeWatchRoots},
		{name: "kilo", resolve: ResolveKiloWatchRoots},
		{name: "mimocode", resolve: ResolveMiMoCodeWatchRoots},
		{name: "icodemate", resolve: ResolveIcodemateWatchRoots},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, []string{root}, tc.resolve(root),
				"missing roots need a deterministic lifecycle watch plan")
		})
	}
}

func TestParseOpenCodeSQLiteVirtualPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dbPath := filepath.Join("/tmp", "opencode.db")
	virtual := OpenCodeSQLiteVirtualPath(dbPath, "ses_123")
	gotDB, gotSessionID, ok := parseOpenCodeFormatVirtualPath(openCodeFmt.dbName, virtual)
	require.True(ok, "expected virtual path to parse")
	assert.Equal(dbPath, gotDB, "db path")
	assert.Equal("ses_123", gotSessionID, "session ID")
	hashDBPath := filepath.Join("/tmp", "opencode#dev", "opencode.db")
	hashVirtual := OpenCodeSQLiteVirtualPath(hashDBPath, "ses_456")
	gotDB, gotSessionID, ok = parseOpenCodeFormatVirtualPath(openCodeFmt.dbName, hashVirtual)
	require.True(ok, "expected virtual path with # in db path to parse")
	assert.Equal(hashDBPath, gotDB, "db path with #")
	assert.Equal("ses_456", gotSessionID, "session ID with #")
	_, _, ok = parseOpenCodeFormatVirtualPath(
		openCodeFmt.dbName,
		"/tmp/project#dir/storage/session/global/ses_123.json",
	)
	assert.False(ok, "expected real storage path with # to be rejected")
}

func TestStripHostPrefix(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		wantHost string
		wantRaw  string
	}{
		{
			"local claude id",
			"abc-123-def",
			"",
			"abc-123-def",
		},
		{
			"local codex id",
			"codex:some-uuid",
			"",
			"codex:some-uuid",
		},
		{
			"host-prefixed claude",
			"devbox1~abc-123-def",
			"devbox1",
			"abc-123-def",
		},
		{
			"host-prefixed codex",
			"devbox1~codex:some-uuid",
			"devbox1",
			"codex:some-uuid",
		},
		{
			"host-prefixed copilot",
			"server2~copilot:sess-id",
			"server2",
			"copilot:sess-id",
		},
		{
			"fqdn host",
			"dev.example.com~abc-123",
			"dev.example.com",
			"abc-123",
		},
		{
			"empty string",
			"",
			"",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, raw := StripHostPrefix(tt.id)
			assert.Equalf(t, tt.wantHost, host, "StripHostPrefix(%q) host", tt.id)
			assert.Equalf(t, tt.wantRaw, raw, "StripHostPrefix(%q) raw", tt.id)
		})
	}
}

func TestAgentByPrefixRemote(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		wantType  AgentType
		wantOK    bool
	}{
		{
			"remote claude",
			"devbox1~abc-123",
			AgentClaude,
			true,
		},
		{
			"remote openclaude",
			"devbox1~openclaude:session-id",
			AgentOpenClaude,
			true,
		},
		{
			"remote codex",
			"devbox1~codex:some-uuid",
			AgentCodex,
			true,
		},
		{
			"remote copilot",
			"server2~copilot:sess-id",
			AgentCopilot,
			true,
		},
		{
			"remote devin",
			"devbox1~devin:session-id",
			AgentDevin,
			true,
		},
		{
			"remote gemini",
			"myhost~gemini:sess-id",
			AgentGemini,
			true,
		},
		{
			"fqdn host with claude",
			"dev.example.com~abc-123",
			AgentClaude,
			true,
		},
		{
			"fqdn host with codex",
			"prod.example.com~codex:sess-id",
			AgentCodex,
			true,
		},
		{
			"remote unknown agent",
			"host1~future:sess-id",
			"",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, ok := AgentByPrefix(tt.sessionID)
			require.Equalf(t, tt.wantOK, ok, "AgentByPrefix(%q) ok", tt.sessionID)
			if ok {
				assert.Equalf(t, tt.wantType, def.Type,
					"AgentByPrefix(%q).Type", tt.sessionID)
			}
		})
	}
}

func TestVSCodeCopilotDefaultDirs(t *testing.T) {
	def, ok := AgentByType(AgentVSCodeCopilot)
	require.True(t, ok, "AgentVSCodeCopilot not in Registry")

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
		assert.Truef(t, slices.Contains(def.DefaultDirs, path),
			"missing default dir: %s", path)
	}
}

func TestWindsurfRegistryEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentWindsurf)
	require.True(ok, "AgentWindsurf not in Registry")

	assert.Equal("Windsurf", def.DisplayName)
	assert.Equal("WINDSURF_DIR", def.EnvVar)
	assert.Equal("windsurf_dirs", def.ConfigKey)
	assert.Equal("windsurf:", def.IDPrefix)
	assert.True(def.FileBased)
	assert.Contains(def.WatchSubdirs, "workspaceStorage")

	required := []string{
		"AppData/Roaming/Windsurf/User",
		"AppData/Roaming/Windsurf - Next/User",
		"Library/Application Support/Windsurf/User",
		"Library/Application Support/Windsurf - Next/User",
		".config/Windsurf/User",
		".config/Windsurf - Next/User",
	}
	for _, path := range required {
		assert.Truef(slices.Contains(def.DefaultDirs, path),
			"missing default dir: %s", path)
	}

	byPrefix, ok := AgentByPrefix("windsurf:session-a")
	require.True(ok)
	assert.Equal(AgentWindsurf, byPrefix.Type)
}

func TestApplyUsageEventTokenTotals(t *testing.T) {
	assert := assert.New(t)

	// Verify that applyUsageEventTokenTotals computes PeakContextTokens
	// correctly including cache-creation and cache-read tokens.
	sess := &ParsedSession{}
	events := []ParsedUsageEvent{
		{
			InputTokens:              1000,
			OutputTokens:             200,
			CacheReadInputTokens:     500,
			CacheCreationInputTokens: 300,
		},
		{
			InputTokens:              800,
			OutputTokens:             150,
			CacheReadInputTokens:     1200,
			CacheCreationInputTokens: 100,
		},
	}

	applyUsageEventTokenTotals(sess, events)

	assert.True(sess.HasTotalOutputTokens)
	assert.Equal(350, sess.TotalOutputTokens)

	assert.True(sess.HasPeakContextTokens)
	// Peak context should be max of context window (InputTokens + CacheRead + CacheCreation)
	// Event 1 context = 1000 + 500 + 300 = 1800
	// Event 2 context = 800 + 1200 + 100 = 2100
	assert.Equal(2100, sess.PeakContextTokens)
}

func TestReasonixRegistryEntry(t *testing.T) {
	assert := assert.New(t)

	// Find Reasonix in the registry
	var reasonixDef *AgentDef
	for _, def := range Registry {
		if def.Type == AgentReasonix {
			reasonixDef = &def
			break
		}
	}
	require.NotNil(t, reasonixDef, "AgentReasonix must be in Registry")

	// Verify basic properties
	assert.Equal(AgentReasonix, reasonixDef.Type)
	assert.Equal("Reasonix", reasonixDef.DisplayName)
	assert.Equal("REASONIX_DIR", reasonixDef.EnvVar)
	assert.Equal("reasonix_dirs", reasonixDef.ConfigKey)
	assert.Equal("reasonix:", reasonixDef.IDPrefix)
	assert.True(reasonixDef.FileBased)

	// Verify watch subdirs
	assert.Contains(reasonixDef.WatchSubdirs, "sessions")
	assert.Contains(reasonixDef.WatchSubdirs, "archive")

	// Verify default dirs contain .reasonix and Windows path
	assert.True(len(reasonixDef.DefaultDirs) > 0)
	hasUnix := false
	hasWindows := false
	for _, dir := range reasonixDef.DefaultDirs {
		if dir == ".reasonix" {
			hasUnix = true
		}
		if dir == "AppData/Roaming/reasonix" {
			hasWindows = true
		}
	}
	assert.True(hasUnix, "DefaultDirs should contain .reasonix")
	assert.True(hasWindows, "DefaultDirs should contain AppData/Roaming/reasonix")
}

func TestFreebuffNotRegistered(t *testing.T) {
	// Freebuff intentionally shares the Codebuff provider and is NOT
	// registered in Registry. Freebuff sessions do carry agent =
	// AgentFreebuff with freebuff:-prefixed IDs (set by
	// parseCodebuffSession when run-state.json agentType contains
	// "free"), but there is no separate registry entry or factory:
	// sync canonicalizes freebuff onto the Codebuff provider def
	// (AgentByPrefix maps freebuff: IDs to the Codebuff def), so
	// lifecycle operations over the shared roots run once.
	for _, def := range Registry {
		assert.NotEqualf(t, AgentFreebuff, def.Type,
			"AgentFreebuff must not be registered — it shares the Codebuff provider")
	}
}
