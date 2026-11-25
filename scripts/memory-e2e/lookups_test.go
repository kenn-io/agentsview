package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestLookupClientVersionGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable shims require Unix")
	}
	for _, tc := range []struct {
		name     string
		resolved bool
	}{
		{name: "unsupported version"},
		{name: "resolved supported version", resolved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			client := filepath.Join(dir, "codex")
			if tc.resolved {
				resolved := filepath.Join(dir, "real-codex")
				require.NoError(t, os.WriteFile(resolved, []byte("#!/bin/sh\necho 'codex-cli 0.172.0'\n"), 0o700))
				mise := filepath.Join(dir, "mise")
				require.NoError(t, os.WriteFile(mise, []byte("#!/bin/sh\nif [ \"$1\" = which ]; then echo '"+resolved+"'; else echo 'codex-cli 0.0.0'; fi\n"), 0o700))
				require.NoError(t, os.Symlink(mise, client))
			} else {
				require.NoError(t, os.WriteFile(client, []byte("#!/bin/sh\necho 'codex-cli 0.0.0'\n"), 0o700))
			}
			t.Setenv("PATH", dir)
			cfg := config{outputDir: filepath.Join(dir, "output"), repository: dir}
			_, err := runLookupClient(t.Context(), cfg, "missing-agentsview", clientCodex, nil, nil)
			if tc.resolved {
				require.Error(t, err)
				require.NotErrorIs(t, err, errLookupCaptureUnsupported)
			} else {
				require.ErrorIs(t, err, errLookupCaptureUnsupported)
				_, err = os.Stat(cfg.outputDir)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestLookupCorpusFixtures(t *testing.T) {
	var fixtures []lookupFixture
	require.NoError(t, readLookupJSON("testdata/lookups/corpus.json", &fixtures))
	root := t.TempDir()
	owners := 0
	for _, fixture := range fixtures {
		t.Run(fixture.ID, func(t *testing.T) {
			_, err := writeLookupFixtureTranscript(root, root, fixture)
			require.NoError(t, err)
			provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}, Machine: "local"})
			require.True(t, ok)
			source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{FullSessionID: "codex:" + fixture.ID})
			require.NoError(t, err)
			require.True(t, found)
			parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, parsed.Results, 1)
			require.GreaterOrEqual(t, parsed.Results[0].Result.Session.UserMessageCount, 2)
			if fixture.ID == "33333333-3333-4333-8333-333333333333" || fixture.ID == "44444444-4444-4444-8444-444444444444" {
				owners++
				links, err := parser.PromptPRLinksContext(t.Context(), fixture.Title)
				require.NoError(t, err)
				require.Len(t, links, 1)
				require.Equal(t, "example-org/project-a", links[0].Repository)
				require.Equal(t, 123, links[0].Number)
			}
		})
	}
	require.Equal(t, 2, owners)
}
