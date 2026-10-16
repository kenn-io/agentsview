//go:build linux && (amd64 || arm64) && !race

package rawderive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSandboxCodexCompressedRolloutsAndReplayParents(t *testing.T) {
	p, err := NewSubprocessParser(20 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.NoError(t, err)
		}
		t.Skip("kernel isolation unavailable")
	}
	for _, tc := range []struct {
		name             string
		compressedChild  bool
		compressedParent bool
	}{
		{name: "compressed source", compressedChild: true},
		{name: "compressed replay parent", compressedParent: true},
		{name: "both compressed", compressedChild: true, compressedParent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const parentID = "11111111-1111-4111-8111-111111111111"
			const childID = "22222222-2222-4222-8222-222222222222"
			root := t.TempDir()
			enc, err := zstd.NewWriter(nil)
			require.NoError(t, err)
			defer enc.Close()
			for _, file := range []struct {
				id         string
				compressed bool
				lines      []string
			}{
				{id: parentID, compressed: tc.compressedParent, lines: []string{
					testjsonl.CodexSessionMetaJSON(parentID, "/work/project", "codex_cli_rs", "2026-09-08T10:00:00Z"),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "parent-turn", "2026-09-08T10:00:01Z"),
				}},
				{id: childID, compressed: tc.compressedChild, lines: []string{
					testjsonl.CodexForkedSessionMetaJSON(childID, parentID, "/work/project", "codex_cli_rs", "2026-09-09T10:00:00Z"),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "parent-turn", "2026-09-09T10:00:01Z"),
					testjsonl.CodexMsgJSON("user", "replayed parent task", "2026-09-09T10:00:02Z"),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "child-turn", "2026-09-09T10:01:00Z"),
					testjsonl.CodexMsgJSON("user", "child task", "2026-09-09T10:01:01Z"),
					testjsonl.CodexMsgJSON("assistant", "child answer", "2026-09-09T10:01:02Z"),
				}},
			} {
				data := []byte(testjsonl.JoinJSONL(file.lines...))
				name := "rollout-2026-09-09T10-00-00-" + file.id + ".jsonl"
				if file.compressed {
					data = enc.EncodeAll(data, nil)
					name += ".zst"
				}
				require.NoError(t, os.WriteFile(filepath.Join(root, name), data, 0o600))
			}
			provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{FullSessionID: "codex:" + childID})
			require.NoError(t, err)
			require.True(t, found)
			manifest, objects := manifestFromCapturePlan(t, parser.AgentCodex, provider, source)
			materialized, err := (Materializer{
				Store: &materializerStore{objects: objects}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
			}).Materialize(t.Context(), manifest)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })

			parsed, err := p.Parse(t.Context(), manifest, materialized)

			require.NoError(t, err)
			require.Len(t, parsed.Outcome.Results, 1)
			result := parsed.Outcome.Results[0]
			assert.Equal(t, parser.DataVersionCurrent, result.DataVersion)
			assert.Empty(t, result.RetryReason)
			assert.Equal(t, "codex:"+parentID, result.Result.Session.ParentSessionID)
			require.Len(t, result.Result.Messages, 2)
			assert.Equal(t, "child task", result.Result.Messages[0].Content)
			assert.Equal(t, "child answer", result.Result.Messages[1].Content)
		})
	}
}
