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
			assert := assert.New(t)
			require := require.New(t)

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
			require.NoError(err)
			if tt.want == "" {
				assert.Empty(cfg.ResolveDirs(parser.AgentPi))
				return
			}
			root := filepath.Join(home, filepath.FromSlash(tt.want))
			assert.Equal([]string{root}, cfg.ResolveDirs(parser.AgentPi))

			// A real transcript must be discoverable through the resolved roots.
			sessionPath := filepath.Join(root, "session-a.jsonl")
			if tt.sessionDir == "" && tt.piDir == "" {
				sessionPath = filepath.Join(root, "--project-a--", "session-a.jsonl")
			}
			require.NoError(os.MkdirAll(filepath.Dir(sessionPath), 0o755))
			require.NoError(os.WriteFile(sessionPath, []byte(`{"type":"session","version":3,"id":"session-a","timestamp":"2026-09-01T12:00:00Z","cwd":"/project-a"}`+"\n"), 0o600))
			provider, ok := parser.NewProvider(parser.AgentPi, parser.ProviderConfig{
				Roots: cfg.ResolveDirs(parser.AgentPi), Machine: "host-a",
			})
			require.True(ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 1)
			assert.Equal(sessionPath, sources[0].DisplayPath)
		})
	}
}
