package rawderive

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func writePiDeriveFixture(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func derivePiManifest(t *testing.T, manifest rawsync.CanonicalManifest, objects map[rawsync.ObjectRef][]byte) ParsedManifest {
	t.Helper()
	materialized, err := (Materializer{Store: &materializerStore{objects: objects}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)
	hosted, err := dispatch.Parse(t.Context(), manifest, materialized)
	require.NoError(t, err)
	return hosted
}

func TestProviderParserHostedPiFamilyParents(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentPi, parser.AgentOMP, parser.AgentOMO, parser.AgentStepCode, parser.AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			dir := root
			if agent != parser.AgentPrimeAgent {
				dir = filepath.Join(root, "project")
			}
			child := filepath.Join(dir, "child.jsonl")
			parent := filepath.Join(dir, "parent-file.jsonl")
			if agent != parser.AgentPrimeAgent {
				parent = filepath.Join(root, "elsewhere/parent-file.jsonl")
			}
			// Reciprocal parents have identical manifest membership; the matcher must emit only the child.
			writePiDeriveFixture(t, parent, `{"type":"session","id":"parent-header","parentSession":`+strconv.Quote(child)+"}\n")
			body := `{"type":"session","id":"child-header","cwd":"/work/project","parentSession":` + strconv.Quote(parent) + "}\n" +
				`{"type":"message","id":"u1","message":{"role":"user","content":"capture this"}}` + "\n" +
				`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[{"type":"text","text":"running"},{"type":"toolCall","id":"call1","name":"bash","arguments":{"command":"echo hello"}}]}}` + "\n" +
				`{"type":"message","id":"r1","parentId":"a1","message":{"role":"toolResult","toolCallId":"call1","content":[{"type":"text","text":"hello"}]}}` + "\n"
			writePiDeriveFixture(t, child, body)
			provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}, Machine: "collector"})
			require.True(t, ok)
			sources, err := provider.SourcesForChangedPath(t.Context(), parser.ChangedPathRequest{Path: child})
			require.NoError(t, err)
			require.Len(t, sources, 1)
			local, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(t.Context()), parser.ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, local.Results, 1)
			require.Len(t, local.Results[0].Result.Messages, 3)
			assert.Equal(t, "capture this", local.Results[0].Result.Messages[0].Content)
			require.Len(t, local.Results[0].Result.Messages[1].ToolCalls, 1)
			assert.Equal(t, "bash", local.Results[0].Result.Messages[1].ToolCalls[0].ToolName)
			require.Len(t, local.Results[0].Result.Messages[2].ToolResults, 1)
			assert.Equal(t, `[{"type":"text","text":"hello"}]`, local.Results[0].Result.Messages[2].ToolResults[0].ContentRaw)
			manifest, objects := manifestFromCapturePlan(t, agent, provider, sources[0])
			require.NoError(t, os.RemoveAll(root))
			hosted := derivePiManifest(t, manifest, objects)
			require.Len(t, hosted.Outcome.Results, 1)
			got := hosted.Outcome.Results[0].Result
			assert.Equal(t, local.Results[0].Result.Messages, got.Messages)
			assert.Equal(t, provider.Definition().IDPrefix+"child-header", got.Session.ID)
			assert.Equal(t, provider.Definition().IDPrefix+"parent-header", got.Session.ParentSessionID)
			expected := local.Results[0].Result.Session
			expected.Machine = got.Session.Machine
			expected.File = got.Session.File
			assert.Equal(t, expected, got.Session)
			assert.Equal(t, child, got.Session.File.Path)
		})
	}
}

func TestProviderParserHostedPiWindowsParent(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentPi, parser.AgentOMP, parser.AgentOMO, parser.AgentStepCode, parser.AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			primary := "project/child.jsonl"
			parent := "other/parent-file.jsonl"
			if agent == parser.AgentPrimeAgent {
				primary = "child.jsonl"
				parent = "parent-file.jsonl"
			}
			storedParent := strings.ToUpper(`C:\captures\` + strings.ReplaceAll(parent, "/", `\`))
			bodies := map[string][]byte{
				primary: []byte(`{"type":"session","id":"child","parentSession":` + strconv.Quote(storedParent) + "}\n"),
				parent:  []byte(`{"type":"session","id":"stored-parent-header"}` + "\n"),
			}
			var entries []rawsync.Entry
			objects := map[rawsync.ObjectRef][]byte{}
			for name, body := range bodies {
				ref := objectRefForBytes(t, body)
				objects[ref] = body
				entries = append(entries, rawsync.Entry{Path: name, Type: "file", Length: int64(len(body)), Objects: []rawsync.ObjectRef{ref}})
			}
			sourceKey := `C:\captures\` + strings.ReplaceAll(primary, "/", `\`)
			manifest := parserTestManifest(t, agent, sourceKey, entries)
			hosted := derivePiManifest(t, manifest, objects)
			require.Len(t, hosted.Outcome.Results, 1)
			provider, _ := parser.NewProvider(agent, parser.ProviderConfig{})
			assert.Equal(t, provider.Definition().IDPrefix+"stored-parent-header", hosted.Outcome.Results[0].Result.Session.ParentSessionID)
		})
	}
}

func TestProviderParserHostedPiMetadataStaysLexical(t *testing.T) {
	worker := t.TempDir()
	cwd := filepath.Join(worker, "visible-folder")
	require.NoError(t, os.MkdirAll(filepath.Join(worker, ".git"), 0o700))
	writePiDeriveFixture(t, filepath.Join(worker, ".git/config"), "[remote \"origin\"]\n url = https://example.invalid/worker-secret.git\n")
	writePiDeriveFixture(t, filepath.Join(cwd, "SKILL.md"), "---\nname: worker-only-skill\n---\n")
	stale := filepath.Join(worker, "stale-parent.jsonl")
	root := t.TempDir()
	child := filepath.Join(root, "child.jsonl")
	body := `{"type":"session","id":"child","cwd":` + strconv.Quote(cwd) + `,"parentSession":` + strconv.Quote(stale) + "}\n" +
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"read1","name":"read","arguments":{"path":"SKILL.md"}}]}}` + "\n"
	writePiDeriveFixture(t, child, body)
	provider, _ := parser.NewProvider(parser.AgentPi, parser.ProviderConfig{Roots: []string{root}})
	sources, err := provider.SourcesForChangedPath(t.Context(), parser.ChangedPathRequest{Path: child})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	local, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, local.Results, 1)
	assert.Equal(t, "worker-only-skill", local.Results[0].Result.Messages[0].ToolCalls[0].SkillName)
	manifest, objects := manifestFromCapturePlan(t, parser.AgentPi, provider, sources[0])
	require.NoError(t, os.RemoveAll(root))
	// The worker happens to have a file at an uncaptured client reference.
	writePiDeriveFixture(t, stale, `{"type":"session","id":"worker-only-parent"}`+"\n")
	hosted := derivePiManifest(t, manifest, objects)
	require.Len(t, hosted.Outcome.Results, 1)
	got := hosted.Outcome.Results[0].Result
	assert.Equal(t, "visible_folder", got.Session.Project)
	assert.Equal(t, "pi:stale-parent", got.Session.ParentSessionID)
	assert.Empty(t, got.Messages[0].ToolCalls[0].SkillName)
}

func TestProviderParserHostedPiIgnoresCompanionLineage(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "project/child.jsonl")
	writePiDeriveFixture(t, filepath.Join(root, "parent.jsonl"), `{"type":"session","id":"parent-header","parentSession":"project"}`+"\n")
	writePiDeriveFixture(t, child, `{"type":"session","id":"child","parentSession":`+strconv.Quote(filepath.Join(root, "parent.jsonl"))+"}\n")
	provider, _ := parser.NewProvider(parser.AgentPi, parser.ProviderConfig{Roots: []string{root}})
	sources, err := parser.RawCaptureSourcesForChangedPath(t.Context(), provider, parser.ChangedPathRequest{Path: child})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	manifest, objects := manifestFromCapturePlan(t, parser.AgentPi, provider, sources[0])
	hosted := derivePiManifest(t, manifest, objects)
	require.Len(t, hosted.Outcome.Results, 1)
	assert.Equal(t, "pi:child", hosted.Outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "pi:parent-header", hosted.Outcome.Results[0].Result.Session.ParentSessionID)
}

func TestProviderParserHostedPiNormalizedAbsoluteParent(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "project/child.jsonl")
	parent := filepath.Join(root, "other/parent.jsonl")
	reference := root + string(filepath.Separator) + "project" + string(filepath.Separator) + ".." + string(filepath.Separator) + "other" + string(filepath.Separator) + string(filepath.Separator) + "parent.jsonl"
	writePiDeriveFixture(t, parent, `{"type":"session","id":"parent-header"}`+"\n")
	writePiDeriveFixture(t, child, `{"type":"session","id":"child","parentSession":`+strconv.Quote(reference)+"}\n")
	provider, _ := parser.NewProvider(parser.AgentPi, parser.ProviderConfig{Roots: []string{root}})
	sources, err := parser.RawCaptureSourcesForChangedPath(t.Context(), provider, parser.ChangedPathRequest{Path: child})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	manifest, objects := manifestFromCapturePlan(t, parser.AgentPi, provider, sources[0])
	require.NoError(t, os.RemoveAll(root))
	hosted := derivePiManifest(t, manifest, objects)
	require.Len(t, hosted.Outcome.Results, 1)
	assert.Equal(t, "pi:parent-header", hosted.Outcome.Results[0].Result.Session.ParentSessionID)
}

func TestProviderParserHostedPiIgnoresSameSuffixCompanion(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentPi, parser.AgentOMP, parser.AgentOMO, parser.AgentStepCode, parser.AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			primary := "project/session.jsonl"
			if agent == parser.AgentPrimeAgent {
				primary = "session.jsonl"
			}
			child := filepath.Join(root, filepath.FromSlash(primary))
			parent := filepath.Join(root, "other", filepath.FromSlash(primary))
			writePiDeriveFixture(t, parent, `{"type":"session","id":"parent-header","parentSession":"./"}`+"\n")
			writePiDeriveFixture(t, child, `{"type":"session","id":"child","parentSession":`+strconv.Quote(parent)+"}\n")
			provider, _ := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}})
			sources, err := parser.RawCaptureSourcesForChangedPath(t.Context(), provider, parser.ChangedPathRequest{Path: child})
			require.NoError(t, err)
			require.Len(t, sources, 1)
			manifest, objects := manifestFromCapturePlan(t, agent, provider, sources[0])
			require.NoError(t, os.RemoveAll(root))
			hosted := derivePiManifest(t, manifest, objects)
			require.Len(t, hosted.Outcome.Results, 1)
			assert.Equal(t, provider.Definition().IDPrefix+"child", hosted.Outcome.Results[0].Result.Session.ID)
			assert.Equal(t, provider.Definition().IDPrefix+"parent-header", hosted.Outcome.Results[0].Result.Session.ParentSessionID)
		})
	}
}
