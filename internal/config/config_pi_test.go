package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestPiDirectoryOverrides(t *testing.T) {
	for _, tt := range []struct {
		name       string
		agentDir   string
		sessionDir string
		piDir      string
		configDirs []string
		want       string
	}{
		{name: "default", want: ".pi/agent/sessions"},
		{name: "agent home", agentDir: "profile", want: "profile/sessions"},
		{name: "session directory", sessionDir: "transcripts", want: "transcripts"},
		{name: "sessions override home", agentDir: "profile", sessionDir: "transcripts", want: "transcripts"},
		{name: "PI_DIR overrides native variables", agentDir: "profile", sessionDir: "transcripts", piDir: "explicit", want: "explicit"},
		{name: "config overrides home", agentDir: "profile", configDirs: []string{"configured"}, want: "configured"},
		{name: "config clears home", agentDir: "profile", configDirs: []string{}},
		{name: "sessions override config", sessionDir: "transcripts", configDirs: []string{"configured"}, want: "transcripts"},
		{name: "tilde home", agentDir: "~/profile", want: "profile/sessions"},
		{name: "tilde sessions", sessionDir: "~/transcripts", want: "transcripts"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			home := canonicalTempDir(t)
			setTestHome(t, home)
			t.Chdir(home)
			t.Setenv("PI_CODING_AGENT_DIR", tt.agentDir)
			t.Setenv("PI_CODING_AGENT_SESSION_DIR", tt.sessionDir)
			t.Setenv("PI_DIR", tt.piDir)
			settings := map[string]any{}
			if tt.configDirs != nil {
				settings["pi_dirs"] = tt.configDirs
			}
			writeConfig(t, dir, settings)

			cfg, err := LoadMinimal()
			require.NoError(t, err)
			if tt.want == "" {
				assert.Empty(t, cfg.ResolveDirs(parser.AgentPi))
				return
			}
			root := filepath.Join(home, filepath.FromSlash(tt.want))
			assert.Equal(t, []string{root}, cfg.ResolveDirs(parser.AgentPi))

			// A real transcript must be discoverable through the resolved roots.
			sessionPath := filepath.Join(root, "session-a.jsonl")
			if tt.sessionDir == "" && tt.piDir == "" {
				sessionPath = filepath.Join(root, "--project-a--", "session-a.jsonl")
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
			require.NoError(t, os.WriteFile(sessionPath, []byte(`{"type":"session","version":3,"id":"session-a","timestamp":"2026-09-01T12:00:00Z","cwd":"/project-a"}`+"\n"), 0o600))
			provider, ok := parser.NewProvider(parser.AgentPi, parser.ProviderConfig{
				Roots: cfg.ResolveDirs(parser.AgentPi), Machine: "host-a",
			})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			assert.Equal(t, sessionPath, sources[0].DisplayPath)
		})
	}
}

// TestStepCodeDirectoryOverrides covers StepCode's root resolution. StepCode
// ships the Pi harness under its own agent directory, so it honours the same
// pair of native overrides Pi does: STEP_CODING_AGENT_DIR re-roots the default
// sessions path and STEP_CODING_AGENT_SESSION_DIR replaces it outright, while
// STEPCODE_DIR beats both the way PI_DIR does.
func TestStepCodeDirectoryOverrides(t *testing.T) {
	for _, tt := range []struct {
		name       string
		agentDir   string
		sessionDir string
		stepDir    string
		configDirs []string
		want       string
	}{
		{name: "default", want: ".stepcode/agent/sessions"},
		{name: "agent home", agentDir: "profile", want: "profile/sessions"},
		{name: "session directory", sessionDir: "transcripts", want: "transcripts"},
		{name: "sessions override home", agentDir: "profile", sessionDir: "transcripts", want: "transcripts"},
		{name: "STEPCODE_DIR overrides native variables", agentDir: "profile", sessionDir: "transcripts", stepDir: "explicit", want: "explicit"},
		{name: "config overrides home", agentDir: "profile", configDirs: []string{"configured"}, want: "configured"},
		{name: "config clears home", agentDir: "profile", configDirs: []string{}},
		{name: "tilde home", agentDir: "~/profile", want: "profile/sessions"},
		{name: "tilde sessions", sessionDir: "~/transcripts", want: "transcripts"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			home := canonicalTempDir(t)
			setTestHome(t, home)
			t.Chdir(home)
			t.Setenv("STEP_CODING_AGENT_DIR", tt.agentDir)
			t.Setenv("STEP_CODING_AGENT_SESSION_DIR", tt.sessionDir)
			t.Setenv("STEPCODE_DIR", tt.stepDir)
			settings := map[string]any{}
			if tt.configDirs != nil {
				settings["stepcode_dirs"] = tt.configDirs
			}
			writeConfig(t, dir, settings)

			cfg, err := LoadMinimal()
			require.NoError(t, err)
			if tt.want == "" {
				assert.Empty(t, cfg.ResolveDirs(parser.AgentStepCode))
				return
			}
			root := filepath.Join(home, filepath.FromSlash(tt.want))
			assert.Equal(t, []string{root}, cfg.ResolveDirs(parser.AgentStepCode))

			// A real transcript must be discoverable through the resolved
			// roots, under StepCode's project-encoded directory and its
			// timestamp-prefixed filename.
			sessionPath := filepath.Join(root, "--project-a--",
				"2026-09-01T12-00-00-000Z_0199e4c2-session-a.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
			require.NoError(t, os.WriteFile(sessionPath, []byte(
				`{"type":"session","version":3,"id":"0199e4c2-session-a",`+
					`"timestamp":"2026-09-01T12:00:00.000Z","cwd":"/project-a"}`+"\n"), 0o600))
			provider, ok := parser.NewProvider(parser.AgentStepCode, parser.ProviderConfig{
				Roots: cfg.ResolveDirs(parser.AgentStepCode), Machine: "host-a",
			})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			assert.Equal(t, sessionPath, sources[0].DisplayPath)
		})
	}
}
