package sync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

type recordingProviderFactory struct {
	inner   parser.ProviderFactory
	configs *[]parser.ProviderConfig
}

func (f recordingProviderFactory) Definition() parser.AgentDef {
	return f.inner.Definition()
}

func (f recordingProviderFactory) Capabilities() parser.Capabilities {
	return f.inner.Capabilities()
}

func (f recordingProviderFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	*f.configs = append(*f.configs, cfg)
	return f.inner.NewProvider(cfg)
}

func TestAiderPathRewriterSurvivesChangedAndParseDiffRoutes(t *testing.T) {
	path := writeAiderHistory(t)
	root := filepath.Dir(filepath.Dir(path))
	rewriter := func(candidate string) string {
		return strings.Replace(candidate, root, "/remote", 1)
	}

	var aiderFactory parser.ProviderFactory
	for _, candidate := range parser.ProviderFactories() {
		if candidate.Definition().Type == parser.AgentAider {
			aiderFactory = candidate
			break
		}
	}
	require.NotNil(t, aiderFactory)
	var configs []parser.ProviderConfig

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs:    map[parser.AgentType][]string{parser.AgentAider: {root}},
		Machine:      "remote-host",
		PathRewriter: rewriter,
		ProviderFactories: []parser.ProviderFactory{
			recordingProviderFactory{
				inner: aiderFactory, configs: &configs,
			},
		},
	})
	t.Cleanup(engine.Close)

	configs = nil
	changed, err := engine.classifyProviderChangedPath(
		context.Background(), path,
	)
	require.NoError(t, err)
	require.NotEmpty(t, changed)
	require.NotEmpty(t, configs)
	assert.NotNil(t, configs[0].PathRewriter,
		"changed-path provider must receive remote identity")

	configs = nil
	diffFiles, err := engine.parseDiffProviderSources(
		context.Background(), parser.AgentAider,
	)
	require.NoError(t, err)
	require.NotEmpty(t, diffFiles)
	require.NotEmpty(t, configs)
	assert.NotNil(t, configs[0].PathRewriter,
		"parse-diff provider must receive remote identity")
}
