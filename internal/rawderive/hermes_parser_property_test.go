//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package rawderive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func deriveHermesProperty(ht *hegel.T, key, entry string, contents []byte) []parser.ParseResult {
	ht.Helper()
	object := objectRefForBytes(ht.T, contents)
	manifest := parserTestManifest(ht.T, parser.AgentHermes, key, []rawsync.Entry{{Path: entry, Type: "file", Length: int64(len(contents)), Objects: []rawsync.ObjectRef{object}}})
	materialized, err := (Materializer{Store: &materializerStore{objects: map[rawsync.ObjectRef][]byte{object: contents}}, BaseDir: ht.TempDir(), MaxTotalBytes: int64(len(contents)) + 1}).Materialize(ht.Context(), manifest)
	require.NoError(ht, err)
	defer func() { require.NoError(ht, materialized.Cleanup()) }()
	dispatcher, err := NewProviderParser(parser.ProviderFactories(), "capture-device")
	require.NoError(ht, err)
	parsed, err := dispatcher.Parse(ht.Context(), manifest, materialized)
	require.NoError(ht, err)
	require.Empty(ht, parsed.Outcome.SourceErrors)
	var results []parser.ParseResult
	for _, result := range parsed.Outcome.Results {
		result.Result.Session.File = parser.FileInfo{}
		results = append(results, result.Result)
	}
	return results
}

// Raw materialization must reproduce local normalization for the full text domain.
func TestHermesPropertyHostedTranscriptAgreesWithLocal(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		content := hegel.Draw(ht, hegel.Text())
		flat := hegel.Draw(ht, hegel.Booleans())
		contents, err := json.Marshal(map[string]string{"role": "user", "content": content})
		require.NoError(ht, err)
		contents = append(contents, '\n')
		home := ht.TempDir()
		dir := home
		if !flat {
			dir = filepath.Join(home, "sessions")
			require.NoError(ht, os.Mkdir(dir, 0o700))
		}
		require.NoError(ht, os.WriteFile(filepath.Join(dir, "example.jsonl"), contents, 0o600))
		p, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{Roots: []string{home}, Machine: "capture-device"})
		require.True(ht, ok)
		ctx := parser.WithoutFilesystemProjectDiscovery(ht.Context())
		sources, err := p.Discover(ctx)
		require.NoError(ht, err)
		require.Len(ht, sources, 1)
		fp, err := p.Fingerprint(ctx, sources[0])
		require.NoError(ht, err)
		local, err := p.Parse(ctx, parser.ParseRequest{Source: sources[0], Fingerprint: fp, Machine: "capture-device", ForceParse: true})
		require.NoError(ht, err)
		require.Empty(ht, local.SourceErrors)
		var want []parser.ParseResult
		for _, result := range local.Results {
			result.Result.Session.File = parser.FileInfo{}
			want = append(want, result.Result)
		}
		assert.Equal(ht, want, deriveHermesProperty(ht, filepath.Join(home, "state.db"), "sessions/example.jsonl", contents))
	}, hegel.WithTestCases(1000))
}

// Native client home spelling determines cron identity on every worker OS.
func TestHermesPropertyHostedCronUsesClientHome(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		windows := hegel.Draw(ht, hegel.Booleans())
		share := hegel.Draw(ht, hegel.Booleans())
		segment := hex.EncodeToString([]byte(name))
		if len(segment) > 80 {
			segment = segment[:80]
		}
		home, separator := "/fixture/home-"+segment, "/"
		if windows {
			home, separator = `C:\fixture\home-`+segment, `\`
			if share {
				home = `\\server\share\home-` + segment
			}
		}
		contents := []byte("{\"role\":\"session_meta\",\"platform\":\"cron\"}\n{\"role\":\"user\",\"content\":\"scheduled question\"}\n")
		results := deriveHermesProperty(ht, home+separator+"state.db", "sessions/cron_job-a_20261007_120000.jsonl", contents)
		require.Len(ht, results, 1)
		digest := sha256.Sum256([]byte(home))
		assert.Equal(ht, fmt.Sprintf("job-a:%x", digest[:4]), results[0].Session.GroupKey)
	}, hegel.WithTestCases(1000))
}
