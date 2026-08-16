package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kimiWorkFixture builds a wire.jsonl transcript in the kimi-code kernel
// format Kimi Work persists (protocol 1.4 metadata header followed by
// top-level turn.prompt / context.append_loop_event records).
func kimiWorkFixture(firstMessage string) string {
	return kimiWorkFixtureAt(firstMessage, 1704067200)
}

func kimiWorkSessionUsageFixture() string {
	return `{"timestamp":1704067200.0,"message":{"type":"TurnBegin","payload":{"user_input":[{"type":"text","text":"usage question"}]}}}` + "\n" +
		`{"timestamp":1704067201.0,"message":{"type":"ContentPart","payload":{"type":"text","text":"Done."}}}` + "\n" +
		`{"timestamp":1704067202.0,"message":{"type":"StatusUpdate","payload":{"token_usage":{"output":42}}}}` + "\n" +
		`{"timestamp":1704067203.0,"message":{"type":"TurnEnd","payload":{}}}` + "\n"
}

func kimiWorkFixtureAt(firstMessage string, timestamp int64) string {
	return fmt.Sprintf(
		`{"type":"metadata","protocol_version":"1.4","created_at":%d}`+"\n"+
			`{"timestamp":%d.0,"type":"turn.prompt","input":[{"type":"text","text":"%s"}]}`+"\n"+
			`{"timestamp":%d.0,"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"Done."}}}`+"\n"+
			`{"timestamp":%d.0,"type":"context.append_loop_event","event":{"type":"step.end","finishReason":"stop","usage":{"output":42}}}`+"\n",
		timestamp*1000, timestamp, firstMessage, timestamp+1, timestamp+2,
	)
}

func TestKimiWorkProviderDiscoveryFiltersAuxSessions(t *testing.T) {
	root := t.TempDir()
	wd := "wd_agentsview_e901f41e2366"
	mainPath := filepath.Join(
		root, wd, "conv-3fac68340656963a67a35ba9",
		"agents", "main", "wire.jsonl",
	)
	subPath := filepath.Join(
		root, wd, "conv-026a61fc8f451ee44ecd6df8",
		"agents", "agent-0", "wire.jsonl",
	)
	legacyPath := filepath.Join(
		root, "wd_legacy_1234567890ab", "conv-a2a93747284e099415320d57",
		"wire.jsonl",
	)
	writeSourceFile(t, mainPath, kimiWorkFixture("main question"))
	writeSourceFile(t, subPath, kimiWorkFixture("subagent question"))
	writeSourceFile(t, legacyPath, kimiWorkFixture("legacy layout question"))

	// Auxiliary internal daimon sessions must never be discovered, in
	// either layout.
	for _, aux := range []string{
		"ctitle-019f85a8-bd77-7f02-ad95-ce249ffdc5c5",
		"sklsum-019e98fd-eaad-7943-aab5-8ffa54a0ef2f",
		"dvlt-019f6bae-4e80-7248-9f8b-4ca8c0e481db",
	} {
		writeSourceFile(t, filepath.Join(
			root, wd, aux, "agents", "main", "wire.jsonl",
		), kimiWorkFixture("aux"))
		writeSourceFile(t, filepath.Join(
			root, wd, aux, "wire.jsonl",
		), kimiWorkFixture("aux"))
	}
	// Non-matching shapes are ignored as for the Kimi provider.
	writeSourceFile(t, filepath.Join(
		root, wd, "conv-3fac68340656963a67a35ba9", "other.jsonl",
	), "{}\n")
	writeSourceFile(t, filepath.Join(
		root, wd, "conv-3fac68340656963a67a35ba9",
		"agents", "sub agent", "wire.jsonl",
	), kimiWorkFixture("bad agent"))
	writeSourceFile(t, filepath.Join(root, "wire.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentKimiWork, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 3)
	for _, source := range discovered {
		assert.Equal(t, AgentKimiWork, source.Provider)
		assert.Contains(t, source.DisplayPath, "conv-")
		assert.NotContains(t, source.DisplayPath, "ctitle-")
		assert.NotContains(t, source.DisplayPath, "sklsum-")
		assert.NotContains(t, source.DisplayPath, "dvlt-")
	}
	assert.Equal(t, "agentsview", discovered[0].ProjectHint)
	assert.Equal(t, "agentsview", discovered[1].ProjectHint)
	assert.Equal(t, "legacy", discovered[2].ProjectHint)
}

func TestKimiWorkProviderFindSourceRoundTrip(t *testing.T) {
	root := t.TempDir()
	wd := "wd_workspace_3dc191b7d233"
	sessionDir := "conv-8d3c0d67139455f66adebb1e"
	mainPath := filepath.Join(
		root, wd, sessionDir, "agents", "main", "wire.jsonl",
	)
	legacyPath := filepath.Join(
		root, "wd_legacy_1234567890ab", "conv-a2a93747284e099415320d57",
		"wire.jsonl",
	)
	writeSourceFile(t, mainPath, kimiWorkFixture("round trip"))
	writeSourceFile(t, legacyPath, kimiWorkFixture("legacy round trip"))

	provider, ok := NewProvider(AgentKimiWork, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	tests := []struct {
		name string
		req  FindSourceRequest
		want string
	}{
		{
			name: "five-part raw ID",
			req:  FindSourceRequest{RawSessionID: wd + ":main:" + sessionDir},
			want: mainPath,
		},
		{
			name: "full session ID with host prefix",
			req: FindSourceRequest{
				FullSessionID: "host~kimi-work:" + wd + ":main:" + sessionDir,
			},
			want: mainPath,
		},
		{
			name: "three-part raw ID",
			req: FindSourceRequest{
				RawSessionID: "wd_legacy_1234567890ab:conv-a2a93747284e099415320d57",
			},
			want: legacyPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, ok, err := provider.FindSource(context.Background(), tt.req)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, tt.want, found.DisplayPath)
		})
	}

	// Aux session IDs must not resolve even though their files exist.
	auxDir := "ctitle-019f85a8-bd77-7f02-ad95-ce249ffdc5c5"
	writeSourceFile(t, filepath.Join(
		root, wd, auxDir, "agents", "main", "wire.jsonl",
	), kimiWorkFixture("aux"))
	for _, rawID := range []string{
		wd + ":main:" + auxDir,
		wd + ":" + auxDir,
		wd + ":main:conv-000000000000000000000000",
		"invalid",
	} {
		_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			RawSessionID: rawID,
		})
		require.NoError(t, err)
		assert.False(t, ok, "rawID %q must not resolve", rawID)
	}
}

func TestKimiWorkProviderParse(t *testing.T) {
	root := t.TempDir()
	wd := "wd_agentsview_e901f41e2366"
	sessionDir := "conv-3fac68340656963a67a35ba9"
	sourcePath := filepath.Join(
		root, wd, sessionDir, "agents", "main", "wire.jsonl",
	)
	writeSourceFile(t, sourcePath, kimiWorkFixture("provider question"))

	provider, ok := NewProvider(AgentKimiWork, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fp, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fp.Key)
	assert.NotEmpty(t, fp.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fp,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, "kimi-work:"+wd+":main:"+sessionDir, result.Session.ID)
	assert.Equal(t, AgentKimiWork, result.Session.Agent)
	assert.Equal(t, "agentsview", result.Session.Project)
	assert.Equal(t, "devbox", result.Session.Machine)
	assert.Equal(t, sourcePath, result.Session.File.Path)
	assert.Equal(t, fp.Hash, result.Session.File.Hash)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "provider question", result.Messages[0].Content)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	// Session-level usage events (if any) must carry the rewritten
	// kimi-work identity, not the parser's native kimi: prefix.
	for _, ev := range result.Session.UsageEvents {
		assert.Equal(t, result.Session.ID, ev.SessionID)
		assert.Contains(t, ev.DedupKey, "kimi-work:session:")
		assert.NotContains(t, ev.DedupKey, "kimi:session:")
	}
}

func TestKimiWorkProviderParseRetainsProviderCwd(t *testing.T) {
	root := t.TempDir()
	wd := "wd_agentsview_e901f41e2366"
	sessionDir := "conv-cwd"
	sourcePath := filepath.Join(
		root, wd, sessionDir, "agents", "main", "wire.jsonl",
	)
	writeSourceFile(t, sourcePath, kimiConfigUpdateCwdLine(t)+"\n"+
		kimiWorkSessionUsageFixture())

	provider, ok := NewProvider(AgentKimiWork, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(t, "/Users/helix/Code/mcp-hub", result.Session.Cwd)
	require.Len(t, result.Session.UsageEvents, 1)
	assert.Equal(t, result.Session.ID, result.Session.UsageEvents[0].SessionID)
	assert.Equal(t, "kimi-work:session:"+wd+":main:"+sessionDir, result.Session.UsageEvents[0].DedupKey)
	assert.Equal(t, 42, result.Session.UsageEvents[0].OutputTokens)
}

func TestKimiWorkProviderMissingModelUsesDateAmbiguousAlias(t *testing.T) {
	tests := []struct {
		name      string
		timestamp int64
		wantStart time.Time
	}{
		{
			name:      "before K3 cutoff",
			timestamp: 1784376000,
			wantStart: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
		},
		{
			name:      "after K3 cutoff",
			timestamp: 1784548800,
			wantStart: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(
				root, "wd_agentsview_e901f41e2366",
				"conv-3fac68340656963a67a35ba9",
				"agents", "main", "wire.jsonl",
			)
			writeSourceFile(t, sourcePath,
				kimiWorkFixtureAt("missing model", tt.timestamp))

			provider, ok := NewProvider(AgentKimiWork, ProviderConfig{
				Roots: []string{root},
			})
			require.True(t, ok)
			sources, err := provider.Discover(context.Background())
			require.NoError(t, err)
			require.Len(t, sources, 1)

			outcome, err := provider.Parse(context.Background(), ParseRequest{
				Source: sources[0],
			})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			result := outcome.Results[0].Result
			assert.Equal(t, tt.wantStart, result.Session.StartedAt.UTC())
			require.Len(t, result.Messages, 2)
			assert.Equal(t, "daimon-kimi-code", result.Messages[1].Model)
		})
	}
}

func TestKimiWorkProviderAgentByPrefix(t *testing.T) {
	def, ok := AgentByPrefix("kimi-work:wd_a_b:main:conv-1")
	require.True(t, ok)
	assert.Equal(t, AgentKimiWork, def.Type)
	assert.Equal(t, "kimi-work:", def.IDPrefix)

	// The kimi: prefix must not capture kimi-work IDs and vice versa.
	def, ok = AgentByPrefix("kimi:abc123:uuid-1")
	require.True(t, ok)
	assert.Equal(t, AgentKimi, def.Type)
}

func TestKimiWorkRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentKimiWork)
	require.True(t, ok, "AgentKimiWork missing from Registry")
	assert.Equal(t, "Kimi Work", def.DisplayName)
	assert.Equal(t, "KIMI_WORK_DIR", def.EnvVar)
	assert.Equal(t, "kimi_work_dirs", def.ConfigKey)
	assert.Equal(t, "kimi-work:", def.IDPrefix)
	assert.True(t, def.FileBased)
	assert.Contains(t, def.DefaultDirs,
		"Library/Application Support/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/sessions")
}

func TestKimiWorkProviderDiscoversSymlinkedWorkspaceDirectory(t *testing.T) {
	root := t.TempDir()
	targetRoot := t.TempDir()
	targetWorkspace := filepath.Join(targetRoot, "wd_agentsview_e901f41e2366")
	sourceWorkspace := filepath.Join(root, "wd_agentsview_e901f41e2366")
	sourcePath := filepath.Join(
		sourceWorkspace, "conv-3fac68340656963a67a35ba9",
		"agents", "main", "wire.jsonl",
	)
	writeSourceFile(t, filepath.Join(
		targetWorkspace, "conv-3fac68340656963a67a35ba9",
		"agents", "main", "wire.jsonl",
	), kimiWorkFixture("from symlink"))
	if err := os.Symlink(targetWorkspace, sourceWorkspace); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentKimiWork, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
}
