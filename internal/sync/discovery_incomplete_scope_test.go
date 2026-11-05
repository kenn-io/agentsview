package sync

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestDiscoverProviderSourcesDiscardsNonHermesIncompleteDiscovery proves
// this PR's "use the partial sources on DiscoveryIncompleteError" branch in
// discoverProviderSources stays scoped to Hermes and Augure Desktop. Every
// other provider -- including Aider and the raw-capture-backed providers,
// which already return DiscoveryIncompleteError from real discovery on
// main -- must keep the prior behavior: any discovery error, including an
// incomplete one, discards that pass's partial sources for the agent
// entirely instead of writing them.
//
// Reuses changedPathPlanProvider/changedPathPlanFactory (changed_path_plan_test.go),
// which already returns configurable sources alongside a discovery error,
// instead of a second discovery-fixture provider.
func TestDiscoverProviderSourcesDiscardsNonHermesIncompleteDiscovery(t *testing.T) {
	root := t.TempDir()
	path, fingerprint := writeProcessProviderSource(t, root, "fixture.jsonl")
	source := processFixtureSource(path)
	outcome := parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{
			Result: processFixtureResult(
				"cowork:fixture", parser.AgentCowork, "fixture-project",
				path, fingerprint,
			),
		}},
		ResultSetComplete: true,
	}
	factory := changedPathPlanFactory{
		agent: parser.AgentCowork,
		caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources: parser.CapabilitySupported,
		}},
	}
	factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			discovered: []parser.SourceRef{source},
			discoverErr: parser.DiscoveryIncompleteError{
				Provider: parser.AgentCowork, Reason: "test budget exceeded",
			},
			outcomes: map[string]parser.ParseOutcome{source.Key: outcome},
			fingerprints: map[string]parser.SourceFingerprint{
				source.Key: fingerprint,
			},
		}
	}

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs:         map[parser.AgentType][]string{parser.AgentCowork: {root}},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncAll(t.Context(), nil)

	assert.Positive(t, stats.Failed,
		"an incomplete discovery for a non-Hermes agent must still count as a failure")
	assert.Zero(t, stats.Synced,
		"the partial source must not be written for a non-Hermes agent")
	stored, err := database.GetSession(t.Context(), "cowork:fixture")
	require.NoError(t, err)
	assert.Nil(t, stored,
		"the partial source's session must be discarded, not written")
}

func TestFullSyncRetainsHealthyLocalHistoryWithDeniedS3(t *testing.T) {
	const uuid = "11111111-1111-4111-8111-111111111111"
	const timestamp = "2026-11-05T10:00:00Z"
	const message = "hello from healthy local"
	claude := `{"type":"user","sessionId":"morning","timestamp":"2026-11-05T10:00:00Z","message":{"role":"user","content":"hello from healthy local"}}` + "\n"
	for _, tc := range []struct {
		agent             parser.AgentType
		file, content, id string
	}{
		{parser.AgentClaude, "project/morning.jsonl", claude, "morning"},
		{parser.AgentIcodemate, "project/morning.jsonl", claude, "icodemate:morning"},
		{parser.AgentCodex, "2026/11/05/rollout-2026-11-05T10-00-00-" + uuid + ".jsonl", testjsonl.CodexSessionMetaJSON(uuid, "/work/project", "codex_cli_rs", timestamp) + "\n" + testjsonl.CodexMsgJSON("user", message, timestamp) + "\n", "codex:" + uuid},
		{parser.AgentCursor, "project/agent-transcripts/morning.jsonl", `{"role":"user","message":{"content":"<user_query>hello from healthy local</user_query>"}}` + "\n", "cursor:morning"},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
			}))
			t.Cleanup(remote.Close)
			t.Setenv("AWS_S3_ENDPOINT", remote.URL)
			t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
			t.Setenv("AWS_REGION", "us-east-1")
			root := t.TempDir()
			path := filepath.Join(root, filepath.FromSlash(tc.file))
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{tc.agent: {"s3://bucket/sessions", root}}, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			assert.False(t, stats.ProcessingComplete(), "a denied root keeps discovery incomplete")
			messages, err := database.GetAllMessages(t.Context(), tc.id)
			require.NoError(t, err)
			require.Len(t, messages, 1, "healthy local history survives incomplete discovery")
			assert.Equal(t, message, messages[0].Content)
			stats = engine.SyncRootsSince(t.Context(), []string{"s3://bucket/sessions"}, time.Time{}, nil)
			assert.False(t, stats.ProcessingComplete(), "the scoped remote pass also reports the failed source")
		})
	}
}
