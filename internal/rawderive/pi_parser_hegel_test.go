//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package rawderive

import (
	"encoding/json/v2"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/stringutil"
)

// The capture contract preserves transcript-derived identity and content across
// local and hosted parsing, including arbitrary JSON string values and parents
// whose filename differs from their authoritative header identity.
func TestPiRawCaptureHostedRoundTripProperty(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentPi, parser.AgentOMP, parser.AgentOMO, parser.AgentStepCode, parser.AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			base := t.TempDir()
			dispatch, err := NewProviderParser(parser.ProviderFactories(), "worker")
			require.NoError(t, err)
			hegel.Test(t, func(ht *hegel.T) {
				quote := func(value string) string {
					encoded, err := json.Marshal(value)
					require.NoError(ht, err)
					return string(encoded)
				}
				payload := hegel.Draw(ht, hegel.Text())
				parentID := hegel.Draw(ht, hegel.Text())
				childNumber := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))
				absoluteParent := hegel.Draw(ht, hegel.Booleans())
				repeatedRoot := hegel.Draw(ht, hegel.Booleans())
				companionLineage := hegel.Draw(ht, hegel.Booleans())
				sameSuffix := hegel.Draw(ht, hegel.Booleans())
				if sameSuffix {
					absoluteParent = true
				}
				// Bound filesystem materialization, not the generated text domains.
				payload = stringutil.SafeTruncate(payload, 8192)
				parentID = stringutil.SafeTruncate(parentID, 1024)
				childID := "child-" + strconv.FormatUint(childNumber, 16)
				root := t.TempDir()
				dir := root
				if agent != parser.AgentPrimeAgent {
					dir = filepath.Join(root, "project")
				}
				require.NoError(ht, os.MkdirAll(dir, 0o700))
				child := filepath.Join(dir, "child.jsonl")
				parent := filepath.Join(dir, "parent-file.jsonl")
				if absoluteParent {
					require.NoError(ht, os.MkdirAll(filepath.Join(root, "project"), 0o700))
					require.NoError(ht, os.MkdirAll(filepath.Join(root, "other"), 0o700))
					parent = filepath.Join(root, "other/parent-file.jsonl")
					if sameSuffix {
						childRel, err := filepath.Rel(root, child)
						require.NoError(ht, err)
						parent = filepath.Join(root, "other", childRel)
						require.NoError(ht, os.MkdirAll(filepath.Dir(parent), 0o700))
					}
				}
				parentRef := "parent-file.jsonl"
				if absoluteParent {
					parentRel, err := filepath.Rel(root, parent)
					require.NoError(ht, err)
					spelling := hegel.Draw(ht, hegel.Integers(0, 2))
					switch spelling {
					case 0:
						parentRef = parent
					case 1:
						parentRef = root + string(filepath.Separator) + "project" + string(filepath.Separator) + ".." + string(filepath.Separator) + parentRel
					case 2:
						parentRef = root + string(filepath.Separator) + strings.ReplaceAll(parentRel, string(filepath.Separator), string(filepath.Separator)+string(filepath.Separator))
					}
				}
				parentHeader := `{"type":"session","id":` + quote(parentID)
				if companionLineage {
					parentHeader += `,"parentSession":"./"`
				}
				require.NoError(ht, os.WriteFile(parent, []byte(parentHeader+"}\n"), 0o600))
				contents := `{"type":"session","id":` + quote(childID) + `,"cwd":"/work/project","parentSession":` + quote(parentRef) + "}\n" +
					`{"type":"message","id":"u1","message":{"role":"user","content":` + quote(payload) + "}}\n" +
					`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[{"type":"toolCall","id":"call1","name":"bash","arguments":{"command":` + quote("echo "+payload) + "}}]}}\n" +
					`{"type":"message","id":"r1","parentId":"a1","message":{"role":"toolResult","toolCallId":"call1","content":` + quote(payload) + "}}\n"
				require.NoError(ht, os.WriteFile(child, []byte(contents), 0o600))
				roots := []string{root}
				if repeatedRoot {
					roots = append(roots, root)
				}
				provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: roots})
				require.True(ht, ok)
				discovery, err := parser.DiscoverRawCaptureSources(ht.Context(), provider)
				require.NoError(ht, err)
				require.True(ht, discovery.Complete)
				expectedSources := 2
				if agent == parser.AgentPrimeAgent && absoluteParent {
					expectedSources = 1
				}
				require.Len(ht, discovery.Sources, expectedSources)
				var source parser.SourceRef
				for _, candidate := range discovery.Sources {
					if candidate.DisplayPath == child {
						source = candidate
					}
				}
				require.NotEmpty(ht, source.Key)
				local, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(ht.Context()), parser.ParseRequest{Source: source})
				require.NoError(ht, err)
				require.Len(ht, local.Results, 1)
				plan, supported, err := parser.ResolveRawCapturePlan(ht.Context(), provider, source)
				require.NoError(ht, err)
				require.True(ht, supported)
				objects := map[rawsync.ObjectRef][]byte{}
				var entries []rawsync.Entry
				for _, entry := range plan.Entries {
					body, err := os.ReadFile(entry.LocalPath)
					require.NoError(ht, err)
					info, err := os.Stat(entry.LocalPath)
					require.NoError(ht, err)
					ref := objectRefForBytes(ht.T, body)
					objects[ref] = body
					entries = append(entries, rawsync.Entry{Path: entry.Path, Type: "file", Length: int64(len(body)), ModTimeNS: info.ModTime().UnixNano(), Objects: []rawsync.ObjectRef{ref}})
				}
				manifest, err := rawsync.ValidateAndCanonicalize(rawsync.AuthIdentity{TenantID: "tenant-property", DeviceID: "device-property"}, rawsync.Manifest{
					SchemaVersion: rawsync.ManifestSchemaVersion, Provider: agent, ConfiguredRootID: "root-property", SourceKey: source.Key, CaptureID: "capture-property", CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Kind: rawsync.ManifestSnapshot, Entries: entries,
				}, rawsync.DefaultManifestLimits())
				require.NoError(ht, err)
				require.NoError(ht, os.RemoveAll(root))
				materialized, err := (Materializer{Store: &materializerStore{objects: objects}, BaseDir: base, MaxTotalBytes: 1 << 20}).Materialize(ht.Context(), manifest)
				require.NoError(ht, err)
				defer materialized.Cleanup()
				hosted, err := dispatch.Parse(ht.Context(), manifest, materialized)
				require.NoError(ht, err)
				require.Len(ht, hosted.Outcome.Results, 1)
				got := hosted.Outcome.Results[0].Result
				assert.Equal(ht, provider.Definition().IDPrefix+childID, got.Session.ID)
				expectedParent := parentID
				if expectedParent == "" {
					expectedParent = "parent-file"
					if sameSuffix {
						expectedParent = "child"
					}
				}
				assert.Equal(ht, provider.Definition().IDPrefix+expectedParent, got.Session.ParentSessionID)
				require.Len(ht, got.Messages, 3)
				assert.Equal(ht, payload, got.Messages[0].Content)
				require.Len(ht, got.Messages[1].ToolCalls, 1)
				assert.JSONEq(ht, `{"command":`+quote("echo "+payload)+"}", got.Messages[1].ToolCalls[0].InputJSON)
				require.Len(ht, got.Messages[2].ToolResults, 1)
				assert.Equal(ht, payload, parser.DecodeContent(got.Messages[2].ToolResults[0].ContentRaw))
				assert.Equal(ht, local.Results[0].Result.Messages, got.Messages)
			}, hegel.WithTestCases(1000))
		})
	}
}
