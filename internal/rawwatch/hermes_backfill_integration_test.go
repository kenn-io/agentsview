package rawwatch

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest/hermesfixture"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawupload"
)

func hermesRawWrite(t *testing.T, dir, name, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	file := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
	return file
}

func hermesRawSeedState(t *testing.T, home string) *sql.DB {
	t.Helper()
	writer := hermesfixture.OpenState(t, home)
	_, err := writer.ExecContext(t.Context(), `
INSERT INTO sessions(id,message_count,input_tokens,output_tokens,title) VALUES('database',3,23,7,'Database session'),('shared',1,40,11,'Shared session'),('usage',0,91,13,'Usage only');
INSERT INTO messages(session_id,role,content) VALUES('database','user','database question'),('shared','user','database fallback');
INSERT INTO messages(session_id,role,content,tool_calls,timestamp) VALUES('database','assistant','Checking','[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"example.txt\"}"}}]',1778767220);
INSERT INTO messages(session_id,role,content,tool_call_id,timestamp) VALUES('database','tool','file contents','call-1',1778767230);`)
	require.NoError(t, err)
	return writer
}

func hermesRawLocalResults(t *testing.T, p parser.Provider) []parser.ParseResult {
	t.Helper()
	ctx := parser.WithoutFilesystemProjectDiscovery(t.Context())
	sources, err := p.Discover(ctx)
	require.NoError(t, err)
	var results []parser.ParseResult
	for _, source := range sources {
		fp, err := p.Fingerprint(ctx, source)
		require.NoError(t, err)
		out, err := p.Parse(ctx, parser.ParseRequest{Source: source, Fingerprint: fp, Machine: "capture-device", ForceParse: true})
		require.NoError(t, err)
		require.Empty(t, out.SourceErrors)
		for _, r := range out.Results {
			results = append(results, r.Result)
		}
	}
	return hermesRawComparable(results)
}

func hermesRawComparable(results []parser.ParseResult) []parser.ParseResult {
	results = slices.Clone(results)
	for i := range results {
		results[i].Session.File = parser.FileInfo{}
	}
	slices.SortFunc(results, func(a, b parser.ParseResult) int {
		if a.Session.ID < b.Session.ID {
			return -1
		}
		if a.Session.ID > b.Session.ID {
			return 1
		}
		return 0
	})
	return results
}

type hermesBackfillHarness struct {
	store     *rawcheckpoint.Store
	provider  parser.Provider
	transport *backfillCustodyTransport
	roots     []rawcheckpoint.BackfillSelection
	runs      int
}

func newHermesBackfillHarness(t *testing.T, roots ...string) *hermesBackfillHarness {
	t.Helper()
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	p, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{Roots: roots, Machine: "capture-device"})
	require.True(t, ok)
	h := &hermesBackfillHarness{store: store, provider: p, transport: newBackfillCustody(t)}
	for _, path := range p.(interface{ ConfiguredRoots() []string }).ConfiguredRoots() {
		root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentHermes, path)
		require.NoError(t, err)
		h.roots = append(h.roots, rawcheckpoint.BackfillSelection{Provider: parser.AgentHermes, ConfiguredRootID: root.ID})
	}
	return h
}

func (h *hermesBackfillHarness) run(t *testing.T) rawcheckpoint.BackfillProgress {
	t.Helper()
	h.runs++
	spec := rawcheckpoint.BackfillRunSpec{RunID: fmt.Sprintf("hermes-run-%d", h.runs), DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentHermes}, Roots: h.roots}
	progress, err := RunBackfill(t.Context(), h.store, rawcapture.New(h.store), rawupload.New(h.store, h.transport, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{h.provider}, BatchSize: 1})
	require.NoError(t, err, "progress: %+v", progress)
	require.True(t, progress.Complete)
	return progress
}

func (h *hermesBackfillHarness) derive(t *testing.T) map[string]rawderive.ParsedManifest {
	t.Helper()
	latest := make(map[string]rawsync.CanonicalManifest)
	for _, m := range h.transport.metadata.manifests {
		prior, ok := latest[m.Manifest.SourceKey]
		if !ok || h.transport.metadata.commits[m.ManifestID].Generation > h.transport.metadata.commits[prior.ManifestID].Generation {
			latest[m.Manifest.SourceKey] = m
		}
	}
	dispatcher, err := rawderive.NewProviderParser(parser.ProviderFactories(), "capture-device")
	require.NoError(t, err)
	materializer := rawderive.Materializer{Store: h.transport.objects, BaseDir: t.TempDir(), MaxTotalBytes: 8 << 20}
	out := make(map[string]rawderive.ParsedManifest)
	for key, m := range latest {
		materialized, err := materializer.Materialize(t.Context(), m)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
		parsed, err := dispatcher.Parse(t.Context(), m, materialized)
		require.NoError(t, err)
		require.NoError(t, materialized.Cleanup())
		require.Empty(t, parsed.Outcome.SourceErrors)
		assert.False(t, parsed.Outcome.ForceReplace, "omission must not erase archived Hermes sessions")
		out[key] = parsed
	}
	return out
}

func TestHermesRawBackfillRoundtrip(t *testing.T) {
	for _, mode := range []string{"database only", "database and transcripts", "transcripts only", "flat transcripts", "sessions root"} {
		t.Run(mode, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			hasDB := mode == "database only" || mode == "database and transcripts" || mode == "sessions root"
			if hasDB {
				hermesRawSeedState(t, home)
			}
			if mode != "database only" {
				transcripts := filepath.Join(home, "sessions")
				if mode == "flat transcripts" {
					transcripts = home
				}
				hermesRawWrite(t, transcripts, "shared.jsonl", `{"role":"session_meta","platform":"cli","timestamp":"2026-05-14T10:00:00Z"}`+"\n"+`{"role":"user","content":"transcript wins","timestamp":"2026-05-14T10:01:00Z"}`+"\n"+`{"role":"assistant","content":"transcript answer","timestamp":"2026-05-14T10:02:00Z"}`+"\n")
				hermesRawWrite(t, transcripts, "session_shared.json", `{"messages":[{"role":"user","content":"shadowed legacy"}]}`)
				hermesRawWrite(t, transcripts, "orphan.jsonl", `{"role":"user","content":"orphan question","timestamp":"2026-05-14T10:00:00Z"}`+"\n")
				hermesRawWrite(t, transcripts, "session_legacy.json", `{"platform":"cli","messages":[{"role":"user","content":"legacy question","timestamp":"2026-05-14T10:00:00Z"}]}`)
				hermesRawWrite(t, transcripts, "cron_job-a_20261007_120000.jsonl", `{"role":"session_meta","platform":"cron"}`+"\n"+`{"role":"user","content":"scheduled question","timestamp":"2026-05-14T10:00:00Z"}`+"\n")
			}
			root := home
			if mode == "sessions root" {
				root = filepath.Join(home, "sessions")
			}
			h := newHermesBackfillHarness(t, root)
			local := hermesRawLocalResults(t, h.provider)
			require.NotEmpty(t, local)
			progress := h.run(t)
			assert.Equal(t, int64(1), progress.Captured)
			require.Len(t, h.transport.metadata.manifests, 1)
			derived := h.derive(t)
			require.Len(t, derived, 1)
			for key, out := range derived {
				assert.Equal(t, filepath.Join(home, "state.db"), key)
				var results []parser.ParseResult
				for _, r := range out.Outcome.Results {
					results = append(results, r.Result)
				}
				assert.Equal(t, local, hermesRawComparable(results))
				byID := make(map[string]parser.ParseResult)
				for _, r := range results {
					byID[r.Session.ID] = r
				}
				if hasDB {
					require.Contains(t, byID, "hermes:database")
					dbResult := byID["hermes:database"]
					require.Len(t, dbResult.Messages, 3)
					assert.Equal(t, "database question", dbResult.Messages[0].Content)
					require.Len(t, dbResult.Messages[1].ToolCalls, 1)
					assert.Equal(t, "call-1", dbResult.Messages[1].ToolCalls[0].ToolUseID)
					assert.Equal(t, "read_file", dbResult.Messages[1].ToolCalls[0].ToolName)
					assert.Equal(t, 13, byID["hermes:usage"].Session.TotalOutputTokens)
				}
				if mode != "database only" {
					assert.Equal(t, "transcript wins", byID["hermes:shared"].Messages[0].Content)
					assert.Equal(t, "orphan question", byID["hermes:orphan"].Messages[0].Content)
					assert.Equal(t, "legacy question", byID["hermes:legacy"].Messages[0].Content)
					assert.NotEmpty(t, byID["hermes:cron_job-a_20261007_120000"].Session.GroupKey)
				}
			}
			h.run(t)
			assert.Len(t, h.transport.metadata.manifests, 1, "unchanged sources must not create another generation")
		})
	}
}

func TestHermesRawBackfillChangesAndDatabaseTransitions(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	file := hermesRawWrite(t, filepath.Join(home, "sessions"), "shared.jsonl", `{"role":"user","content":"original"}`+"\n")
	h := newHermesBackfillHarness(t, home)
	h.run(t)
	h.derive(t)
	appendFile := func(content string) {
		f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = f.WriteString(content)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	appendFile(`{"role":"assistant","content":"append"}` + "\n")
	h.run(t)
	assert.Len(t, h.transport.metadata.manifests, 2)
	hermesRawWrite(t, filepath.Dir(file), "shared.jsonl", `{"role":"user","content":"replacement"}`+"\n")
	h.run(t)
	assert.Len(t, h.transport.metadata.manifests, 3)
	writer := hermesRawSeedState(t, home)
	h.run(t)
	assert.Len(t, h.transport.metadata.manifests, 4)
	h.derive(t)
	require.NoError(t, writer.Close())
	require.NoError(t, os.Remove(filepath.Join(home, "state.db")))
	h.run(t)
	assert.Len(t, h.transport.metadata.manifests, 5)
	after := h.derive(t)
	require.Contains(t, after, filepath.Join(home, "state.db"))
	hermesRawWrite(t, filepath.Dir(file), "other.jsonl", `{"role":"user","content":"remaining"}`+"\n")
	require.NoError(t, os.Remove(file))
	h.run(t)
	assert.Len(t, h.transport.metadata.manifests, 6)
	h.derive(t)
}

func TestHermesRawBackfillProfilesKeepSeparateContent(t *testing.T) {
	container, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	container = filepath.Join(container, ".hermes", "profiles")
	for _, profile := range []string{"alpha", "beta"} {
		hermesRawWrite(t, filepath.Join(container, profile, "sessions"), "shared.jsonl", `{"role":"user","content":"`+profile+` question"}`+"\n")
	}
	h := newHermesBackfillHarness(t, container)
	h.run(t)
	derived := h.derive(t)
	require.Len(t, derived, 2)
	for _, profile := range []string{"alpha", "beta"} {
		key := filepath.Join(container, profile, "state.db")
		require.Contains(t, derived, key)
		results := derived[key].Outcome.Results
		require.Len(t, results, 1)
		assert.Equal(t, "hermes:shared", results[0].Result.Session.ID)
		require.Len(t, results[0].Result.Messages, 1)
		assert.Equal(t, profile+" question", results[0].Result.Messages[0].Content)
	}
	h.run(t)
	assert.Len(t, h.transport.metadata.manifests, 2)
}
