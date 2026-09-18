package parser

import (
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
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)
	assert.True(plan.Roots[0].Recursive)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 3)
	for _, source := range discovered {
		assert.Equal(AgentKimiWork, source.Provider)
		assert.Contains(source.DisplayPath, "conv-")
		assert.NotContains(source.DisplayPath, "ctitle-")
		assert.NotContains(source.DisplayPath, "sklsum-")
		assert.NotContains(source.DisplayPath, "dvlt-")
	}
	assert.Equal("agentsview", discovered[0].ProjectHint)
	assert.Equal("agentsview", discovered[1].ProjectHint)
	assert.Equal("legacy", discovered[2].ProjectHint)
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
			found, ok, err := provider.FindSource(t.Context(), tt.req)
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
		_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
			RawSessionID: rawID,
		})
		require.NoError(t, err)
		assert.False(t, ok, "rawID %q must not resolve", rawID)
	}
}

func TestKimiWorkProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fp, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	assert.Equal(sourcePath, fp.Key)
	assert.NotEmpty(fp.Hash)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fp,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal("kimi-work:"+wd+":main:"+sessionDir, result.Session.ID)
	assert.Equal(AgentKimiWork, result.Session.Agent)
	assert.Equal("agentsview", result.Session.Project)
	assert.Equal("devbox", result.Session.Machine)
	assert.Equal(sourcePath, result.Session.File.Path)
	assert.Equal(fp.Hash, result.Session.File.Hash)
	require.Len(result.Messages, 2)
	assert.Equal(RoleUser, result.Messages[0].Role)
	assert.Equal("provider question", result.Messages[0].Content)
	assert.Equal(RoleAssistant, result.Messages[1].Role)
	// Session-level usage events (if any) must carry the rewritten
	// kimi-work identity, not the parser's native kimi: prefix.
	for _, ev := range result.Session.UsageEvents {
		assert.Equal(result.Session.ID, ev.SessionID)
		assert.Contains(ev.DedupKey, "kimi-work:session:")
		assert.NotContains(ev.DedupKey, "kimi:session:")
	}
}

func TestKimiWorkProviderParseRetainsProviderCwd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	wd := "wd_agentsview_e901f41e2366"
	sessionDir := "conv-cwd"
	sourcePath := filepath.Join(
		root, wd, sessionDir, "agents", "main", "wire.jsonl",
	)
	writeSourceFile(t, sourcePath, kimiConfigUpdateCwdLine(t)+"\n"+
		kimiWorkSessionUsageFixture())

	provider, ok := NewProvider(AgentKimiWork, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal("/Users/helix/Code/mcp-hub", result.Session.Cwd)
	require.Len(result.Session.UsageEvents, 1)
	assert.Equal(result.Session.ID, result.Session.UsageEvents[0].SessionID)
	assert.Equal("kimi-work:session:"+wd+":main:"+sessionDir, result.Session.UsageEvents[0].DedupKey)
	assert.Equal(42, result.Session.UsageEvents[0].OutputTokens)
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
			assert := assert.New(t)
			require := require.New(t)

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
			require.True(ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 1)

			outcome, err := provider.Parse(t.Context(), ParseRequest{
				Source: sources[0],
			})
			require.NoError(err)
			require.Len(outcome.Results, 1)
			result := outcome.Results[0].Result
			assert.Equal(tt.wantStart, result.Session.StartedAt.UTC())
			require.Len(result.Messages, 2)
			assert.Equal("daimon-kimi-code", result.Messages[1].Model)
		})
	}
}

func TestKimiWorkProviderAgentByPrefix(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByPrefix("kimi-work:wd_a_b:main:conv-1")
	require.True(ok)
	assert.Equal(AgentKimiWork, def.Type)
	assert.Equal("kimi-work:", def.IDPrefix)

	// The kimi: prefix must not capture kimi-work IDs and vice versa.
	def, ok = AgentByPrefix("kimi:abc123:uuid-1")
	require.True(ok)
	assert.Equal(AgentKimi, def.Type)
}

func TestKimiWorkRegistryEntry(t *testing.T) {
	assert := assert.New(t)

	def, ok := AgentByType(AgentKimiWork)
	require.True(t, ok, "AgentKimiWork missing from Registry")
	assert.Equal("Kimi Work", def.DisplayName)
	assert.Equal("KIMI_WORK_DIR", def.EnvVar)
	assert.Equal("kimi_work_dirs", def.ConfigKey)
	assert.Equal("kimi-work:", def.IDPrefix)
	assert.True(def.FileBased)
	assert.Contains(def.DefaultDirs,
		"Library/Application Support/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/sessions")
}

func TestKimiWorkProviderDiscoversSymlinkedWorkspaceDirectory(t *testing.T) {
	require := require.New(t)

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
	require.True(ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
}
