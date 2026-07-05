package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/vector"
)

// writeEmbeddingsTestConfig writes a config.toml under dataDir with [vector]
// enabled and pointed at endpoint, using small-but-valid operational values
// so config.Validate accepts it.
func writeEmbeddingsTestConfig(t *testing.T, dataDir, endpoint string) {
	t.Helper()
	writeTestConfig(t, dataDir, fmt.Sprintf(`
[vector]
enabled = true

[vector.embeddings]
endpoint = %q
model = "test-model"
dimension = 3
batch_size = 10
timeout = "5s"
max_retries = 1
max_input_chars = 1000
`, endpoint))
}

// newEmbeddingsStubServer returns an httptest server that answers the
// OpenAI-compatible /embeddings endpoint with dimension-length vectors for
// every input, mirroring the shape internal/vector/encoder_test.go's stub
// uses.
func newEmbeddingsStubServer(t *testing.T, dimension int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			vec := make([]float32, dimension)
			for j := range vec {
				vec[j] = float32(i + 1)
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
}

// seedEmbeddableArchive creates sessions.db at cfg's default path with one
// session carrying one user and one assistant message, both eligible for
// embedding.
func seedEmbeddableArchive(t *testing.T, dataDir string) {
	t.Helper()
	d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSessionWithMessages(t, d, "sess-1", "proj", []db.Message{
		dbtest.UserMsg("sess-1", 0, "hello there"),
		dbtest.AsstMsg("sess-1", 1, "hi back"),
	})
}

// seedEmbeddableArchiveWithAutomated seeds the same normal session
// seedEmbeddableArchive does, plus one automated session with one
// embeddable message, for the include-automated scope tests.
func seedEmbeddableArchiveWithAutomated(t *testing.T, dataDir string) {
	t.Helper()
	d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSessionWithMessages(t, d, "sess-1", "proj", []db.Message{
		dbtest.UserMsg("sess-1", 0, "hello there"),
		dbtest.AsstMsg("sess-1", 1, "hi back"),
	})
	dbtest.SeedSessionWithMessages(t, d, "sess-auto", "proj", []db.Message{
		dbtest.UserMsg("sess-auto", 0, "roborev output"),
	}, func(s *db.Session) { s.IsAutomated = true })
}

// TestEmbeddingsDisabledReturnsError asserts every subcommand refuses with
// the exact "vector search is not enabled" message when [vector] is off
// (the default), before attempting any daemon detection or I/O.
func TestEmbeddingsDisabledReturnsError(t *testing.T) {
	tests := []struct {
		name   string
		newCmd func() *cobra.Command
		args   []string
	}{
		{"build", newEmbeddingsBuildCommand, nil},
		{"list", newEmbeddingsListCommand, nil},
		{"activate", newEmbeddingsActivateCommand, []string{"1"}},
		{"retire", newEmbeddingsRetireCommand, []string{"1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testDataDir(t)

			cmd := tt.newCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			require.Error(t, err)
			assert.Equal(t,
				"vector search is not enabled: set [vector] enabled = true in config.toml",
				err.Error())
		})
	}
}

// TestEmbeddingsListRendersTable seeds vectors.db directly with one
// generation (bypassing a full build) and asserts `embeddings list`
// renders it as a table with the documented columns and a
// fingerprint truncated to 12 characters.
func TestEmbeddingsListRendersTable(t *testing.T) {
	dataDir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dataDir, "http://127.0.0.1:1")

	cfg, err := config.LoadMinimal()
	require.NoError(t, err)

	ctx := context.Background()
	ix, err := vector.Open(ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false,
		cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	fp, err := ix.EnsureGeneration(ctx, vectorGeneration(cfg.Vector.Embeddings), sqlitevec.StateActive)
	require.NoError(t, err)
	require.NoError(t, ix.Close())

	cmd := newEmbeddingsListCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2, "expected a header line and one generation row, got: %q", out.String())
	assert.Contains(t, lines[0], "ID")
	assert.Contains(t, lines[0], "STATE")
	assert.Contains(t, lines[0], "MODEL")
	assert.Contains(t, lines[0], "DIM")
	assert.Contains(t, lines[0], "EMBEDDED")
	assert.Contains(t, lines[0], "MISSING")
	assert.Contains(t, lines[0], "FINGERPRINT")

	assert.Contains(t, lines[1], "active")
	assert.Contains(t, lines[1], "test-model")
	assert.Contains(t, lines[1], truncateFingerprint(fp))
	assert.NotContains(t, lines[1], fp, "fingerprint column must be truncated to 12 chars")
}

// TestEmbeddingsListJSONFormat asserts `embeddings list --format json`
// wraps the generation list in the same {"generations": [...]} shape the
// Task 14 HTTP endpoint uses.
func TestEmbeddingsListJSONFormat(t *testing.T) {
	dataDir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dataDir, "http://127.0.0.1:1")

	cfg, err := config.LoadMinimal()
	require.NoError(t, err)
	ctx := context.Background()
	ix, err := vector.Open(ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false,
		cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	_, err = ix.EnsureGeneration(ctx, vectorGeneration(cfg.Vector.Embeddings), sqlitevec.StateActive)
	require.NoError(t, err)
	require.NoError(t, ix.Close())

	cmd := newEmbeddingsListCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--format", "json"})
	require.NoError(t, cmd.Execute())

	var body struct {
		Generations []vector.GenerationInfo `json:"generations"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &body))
	require.Len(t, body.Generations, 1)
	assert.Equal(t, "active", body.Generations[0].State)
}

// TestEmbeddingsListEmptyIndexReturnsNoRows asserts `embeddings list`
// against a data dir where vectors.db was never built prints only the
// header, without erroring.
func TestEmbeddingsListEmptyIndexReturnsNoRows(t *testing.T) {
	dataDir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dataDir, "http://127.0.0.1:1")

	cmd := newEmbeddingsListCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 1, "expected only the header line, got: %q", out.String())
}

// TestEmbeddingsBuildDirectEndToEnd seeds a temp archive with one user and
// one assistant message, points [vector.embeddings] at an httptest
// OpenAI-compatible stub, and asserts the direct build path embeds both
// messages and prints the exact final summary and activation line.
func TestEmbeddingsBuildDirectEndToEnd(t *testing.T) {
	dataDir := testDataDir(t)
	stub := newEmbeddingsStubServer(t, 3)
	defer stub.Close()
	writeEmbeddingsTestConfig(t, dataDir, stub.URL+"/v1")
	seedEmbeddableArchive(t, dataDir)

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	assert.Contains(t, out.String(), "Embedded 2 documents (2 chunks), skipped 0, stale 0")
	assert.Contains(t, out.String(), "Generation activated.")
}

// TestEmbeddingsBuildDirectPrintsProgress shrinks the direct path's
// progress ticker and slows the embeddings stub down so the build is still
// running when the ticker fires, asserting at least one progress line in
// the documented format is printed before the final summary.
func TestEmbeddingsBuildDirectPrintsProgress(t *testing.T) {
	orig := directBuildProgressInterval
	directBuildProgressInterval = 5 * time.Millisecond
	t.Cleanup(func() { directBuildProgressInterval = orig })

	dataDir := testDataDir(t)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		time.Sleep(150 * time.Millisecond)

		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 2, 3}}
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	defer stub.Close()
	writeEmbeddingsTestConfig(t, dataDir, stub.URL+"/v1")
	seedEmbeddableArchive(t, dataDir)

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	assert.Regexp(t, `progress: \d+/\d+ chunks \(\d+\.\d%\)`, out.String(),
		"a running build must print at least one progress line")
	assert.Contains(t, out.String(), "Embedded 2 documents (2 chunks), skipped 0, stale 0")
}

// TestEmbeddingsBuildDirectConcurrentFlockFails asserts a second direct
// build invocation, run while another process (simulated by acquiring the
// lock in the test) holds vectors.write.lock, fails immediately with the
// write-lock-held error rather than racing the first build.
func TestEmbeddingsBuildDirectConcurrentFlockFails(t *testing.T) {
	dataDir := testDataDir(t)
	stub := newEmbeddingsStubServer(t, 3)
	defer stub.Close()
	writeEmbeddingsTestConfig(t, dataDir, stub.URL+"/v1")
	seedEmbeddableArchive(t, dataDir)

	held, err := tryAcquireNamedLock(dataDir, vectorsWriteLockFile)
	require.NoError(t, err)
	defer held.Close()

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)

	err = cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "held by another process")
	assert.Contains(t, err.Error(), vectorsWriteLockFile)
}

// TestEmbeddingsBuildFullRebuildPromptsAndAborts asserts --full-rebuild
// without --yes prints the exact confirmation prompt with the true
// embeddable-message count, and a "no" answer aborts without building.
func TestEmbeddingsBuildFullRebuildPromptsAndAborts(t *testing.T) {
	dataDir := testDataDir(t)
	stub := newEmbeddingsStubServer(t, 3)
	defer stub.Close()
	writeEmbeddingsTestConfig(t, dataDir, stub.URL+"/v1")
	seedEmbeddableArchive(t, dataDir)

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs([]string{"--full-rebuild"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, out.String(), "This re-embeds all 2 messages. Continue?")
	assert.Contains(t, out.String(), "Aborted.")
	assert.NotContains(t, out.String(), "Embedded")
}

// TestEmbeddingsBuildFullRebuildYesSkipsPrompt asserts --yes skips the
// confirmation prompt entirely and proceeds straight to the build.
func TestEmbeddingsBuildFullRebuildYesSkipsPrompt(t *testing.T) {
	dataDir := testDataDir(t)
	stub := newEmbeddingsStubServer(t, 3)
	defer stub.Close()
	writeEmbeddingsTestConfig(t, dataDir, stub.URL+"/v1")
	seedEmbeddableArchive(t, dataDir)

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--full-rebuild", "--yes"})
	require.NoError(t, cmd.Execute())

	assert.NotContains(t, out.String(), "Continue?")
	assert.Contains(t, out.String(), "Embedded 2 documents (2 chunks), skipped 0, stale 0")
}

// TestEmbeddingsBuildDirectExcludesAutomatedByDefault asserts the direct
// build path's default scope (no config include_automated, no
// --include-automated flag) never embeds an automated session's messages.
func TestEmbeddingsBuildDirectExcludesAutomatedByDefault(t *testing.T) {
	dataDir := testDataDir(t)
	stub := newEmbeddingsStubServer(t, 3)
	defer stub.Close()
	writeEmbeddingsTestConfig(t, dataDir, stub.URL+"/v1")
	seedEmbeddableArchiveWithAutomated(t, dataDir)

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	assert.Contains(t, out.String(), "Embedded 2 documents (2 chunks), skipped 0, stale 0",
		"the automated session's message must not be embedded by default")
}

// TestEmbeddingsBuildDirectIncludeAutomatedFlagOverridesConfig asserts
// --include-automated forces the automated session's message into the build
// even though [vector].include_automated defaults to false.
func TestEmbeddingsBuildDirectIncludeAutomatedFlagOverridesConfig(t *testing.T) {
	dataDir := testDataDir(t)
	stub := newEmbeddingsStubServer(t, 3)
	defer stub.Close()
	writeEmbeddingsTestConfig(t, dataDir, stub.URL+"/v1")
	seedEmbeddableArchiveWithAutomated(t, dataDir)

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--include-automated"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, out.String(), "Embedded 3 documents (3 chunks), skipped 0, stale 0",
		"--include-automated must embed the automated session's message too")
}

// TestEmbeddingsBuildDirectIncludeAutomatedConfigDefault asserts
// [vector].include_automated = true embeds automated sessions without
// needing the --include-automated flag on every build.
func TestEmbeddingsBuildDirectIncludeAutomatedConfigDefault(t *testing.T) {
	dataDir := testDataDir(t)
	stub := newEmbeddingsStubServer(t, 3)
	defer stub.Close()
	writeTestConfig(t, dataDir, fmt.Sprintf(`
[vector]
enabled = true
include_automated = true

[vector.embeddings]
endpoint = %q
model = "test-model"
dimension = 3
batch_size = 10
timeout = "5s"
max_retries = 1
max_input_chars = 1000
`, stub.URL+"/v1"))
	seedEmbeddableArchiveWithAutomated(t, dataDir)

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	assert.Contains(t, out.String(), "Embedded 3 documents (3 chunks), skipped 0, stale 0")
}

// TestEmbeddingsBuildDirectIncludeAutomatedFlagFalseOverridesConfigTrue
// asserts --include-automated=false narrows the scope back down for this one
// build even when [vector].include_automated defaults to true: the parsed
// flag value must win, not just "the flag was passed forces true".
func TestEmbeddingsBuildDirectIncludeAutomatedFlagFalseOverridesConfigTrue(t *testing.T) {
	dataDir := testDataDir(t)
	stub := newEmbeddingsStubServer(t, 3)
	defer stub.Close()
	writeTestConfig(t, dataDir, fmt.Sprintf(`
[vector]
enabled = true
include_automated = true

[vector.embeddings]
endpoint = %q
model = "test-model"
dimension = 3
batch_size = 10
timeout = "5s"
max_retries = 1
max_input_chars = 1000
`, stub.URL+"/v1"))
	seedEmbeddableArchiveWithAutomated(t, dataDir)

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--include-automated=false"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, out.String(), "Embedded 2 documents (2 chunks), skipped 0, stale 0",
		"--include-automated=false must exclude the automated session's message "+
			"even though the config default is true")
}

// TestEmbeddingsBuildIncludeAutomatedFlagThreadsToDaemonRequest drives the
// daemon build path and asserts --include-automated forces
// BuildRequest.IncludeAutomated to true in the request body, overriding the
// (default false) config value for this one build.
func TestEmbeddingsBuildIncludeAutomatedFlagThreadsToDaemonRequest(t *testing.T) {
	dataDir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dataDir, "http://127.0.0.1:1")

	var gotIncludeAutomated atomic.Bool
	startEmbeddingsTestDaemon(t, dataDir, map[string]http.HandlerFunc{
		"POST /api/v1/embeddings/build": func(w http.ResponseWriter, r *http.Request) {
			var req vector.BuildRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			gotIncludeAutomated.Store(req.IncludeAutomated)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]bool{"started": true})
		},
		"GET /api/v1/embeddings/status": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(vector.BuildStatus{Running: false})
		},
	})

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--include-automated"})
	require.NoError(t, cmd.Execute())

	assert.True(t, gotIncludeAutomated.Load(), "--include-automated must pass through to the daemon")
}

// TestEmbeddingsBuildDaemonRequestDefaultsToConfigScope asserts that without
// --include-automated, the daemon request body still carries the resolved
// config value explicitly (true here), rather than silently falling back to
// the zero value the daemon's own (possibly different) config would use.
func TestEmbeddingsBuildDaemonRequestDefaultsToConfigScope(t *testing.T) {
	dataDir := testDataDir(t)
	writeTestConfig(t, dataDir, `
[vector]
enabled = true
include_automated = true

[vector.embeddings]
endpoint = "http://127.0.0.1:1"
model = "test-model"
dimension = 3
batch_size = 10
timeout = "5s"
max_retries = 1
max_input_chars = 1000
`)

	var gotIncludeAutomated atomic.Bool
	startEmbeddingsTestDaemon(t, dataDir, map[string]http.HandlerFunc{
		"POST /api/v1/embeddings/build": func(w http.ResponseWriter, r *http.Request) {
			var req vector.BuildRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			gotIncludeAutomated.Store(req.IncludeAutomated)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]bool{"started": true})
		},
		"GET /api/v1/embeddings/status": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(vector.BuildStatus{Running: false})
		},
	})

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	assert.True(t, gotIncludeAutomated.Load(),
		"the CLI must resolve its local config's include_automated=true and send it explicitly")
}

// TestEmbeddingsRetireDirectRoundTrip builds an active generation directly,
// asserts retiring it without --force is refused with the manager's exact
// message, and that --force performs the retirement and prints the success
// line.
func TestEmbeddingsRetireDirectRoundTrip(t *testing.T) {
	dataDir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dataDir, "http://127.0.0.1:1")

	cfg, err := config.LoadMinimal()
	require.NoError(t, err)
	ctx := context.Background()
	ix, err := vector.Open(ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false,
		cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	_, err = ix.EnsureGeneration(ctx, vectorGeneration(cfg.Vector.Embeddings), sqlitevec.StateActive)
	require.NoError(t, err)
	require.NoError(t, ix.Close())

	retireCmd := newEmbeddingsRetireCommand()
	var out bytes.Buffer
	retireCmd.SetOut(&out)
	retireCmd.SetArgs([]string{"1"})
	err = retireCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is active")
	assert.Contains(t, err.Error(), "use --force")

	forceCmd := newEmbeddingsRetireCommand()
	var forceOut bytes.Buffer
	forceCmd.SetOut(&forceOut)
	forceCmd.SetArgs([]string{"1", "--force"})
	require.NoError(t, forceCmd.Execute())
	assert.Equal(t, "Generation 1 retired.\n", forceOut.String())
}

// TestEmbeddingsActivateDirectRefusalThenForce builds generation 1
// end-to-end (active, 2 docs embedded), registers a second never-filled
// building generation with Missing = 2, and asserts the direct path
// surfaces Manager.Activate's refusal wording verbatim without --force,
// then succeeds with --force.
func TestEmbeddingsActivateDirectRefusalThenForce(t *testing.T) {
	dataDir := testDataDir(t)
	stub := newEmbeddingsStubServer(t, 3)
	defer stub.Close()
	writeEmbeddingsTestConfig(t, dataDir, stub.URL+"/v1")
	seedEmbeddableArchive(t, dataDir)

	buildCmd := newEmbeddingsBuildCommand()
	var buildOut bytes.Buffer
	buildCmd.SetOut(&buildOut)
	buildCmd.SetArgs(nil)
	require.NoError(t, buildCmd.Execute())

	cfg, err := config.LoadMinimal()
	require.NoError(t, err)
	ctx := context.Background()
	ix, err := vector.Open(ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false,
		cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	otherGen := kitvec.Generation{Model: "other-model", Dimensions: 3}
	_, err = ix.EnsureGeneration(ctx, otherGen, sqlitevec.StateBuilding)
	require.NoError(t, err)
	require.NoError(t, ix.Close())

	activateCmd := newEmbeddingsActivateCommand()
	var out bytes.Buffer
	activateCmd.SetOut(&out)
	activateCmd.SetArgs([]string{"2"})
	err = activateCmd.Execute()
	require.Error(t, err)
	assert.Equal(t,
		"generation 2 still has 2 messages needing embedding; use --force",
		err.Error(), "the manager's refusal message must surface verbatim")

	forceCmd := newEmbeddingsActivateCommand()
	var forceOut bytes.Buffer
	forceCmd.SetOut(&forceOut)
	forceCmd.SetArgs([]string{"2", "--force"})
	require.NoError(t, forceCmd.Execute())
	assert.Equal(t, "Generation 2 activated.\n", forceOut.String())
}

// TestEmbeddingsActivateInvalidIDReturnsError asserts a non-numeric
// argument is rejected before any config or I/O work happens.
func TestEmbeddingsActivateInvalidIDReturnsError(t *testing.T) {
	testDataDir(t)
	cmd := newEmbeddingsActivateCommand()
	cmd.SetArgs([]string{"not-a-number"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid generation id")
}

// startEmbeddingsTestDaemon starts an httptest server that answers the kit
// daemon ping (so FindDaemonRuntime's probe succeeds) plus the given
// embeddings endpoint handlers, writes a live writable runtime record for
// it into dataDir (so IsLocalDaemonActive reports true), and registers
// cleanup. Handlers are keyed by "METHOD /path".
func startEmbeddingsTestDaemon(
	t *testing.T, dataDir string, handlers map[string]http.HandlerFunc,
) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET /api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	}))
	for pattern, h := range handlers {
		mux.HandleFunc(pattern, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	endpoint := serverEndpoint(t, srv)
	writeDaemonRuntimeForTest(t, dataDir, endpoint.Host, endpoint.Port, "test", false)
}

// TestEmbeddingsListDispatchesToDaemon drives the real `embeddings list`
// command with a live writable daemon runtime record present, asserting
// the command routes through the daemon's /generations endpoint (never
// touching vectors.db, which does not exist) and renders its response.
func TestEmbeddingsListDispatchesToDaemon(t *testing.T) {
	dataDir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dataDir, "http://127.0.0.1:1")

	var listCalled atomic.Bool
	startEmbeddingsTestDaemon(t, dataDir, map[string]http.HandlerFunc{
		"GET /api/v1/embeddings/generations": func(w http.ResponseWriter, r *http.Request) {
			listCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"generations": []vector.GenerationInfo{{
					ID: 1, State: "active", Model: "daemon-model", Dimension: 3,
					Fingerprint: "abcdef0123456789", Embedded: 7,
				}},
			})
		},
	})

	cmd := newEmbeddingsListCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	assert.True(t, listCalled.Load(), "list must route through the daemon endpoint")
	assert.Contains(t, out.String(), "daemon-model")
	assert.Contains(t, out.String(), "abcdef012345")
	assert.NoFileExists(t, filepath.Join(dataDir, "vectors.db"),
		"daemon-dispatched list must not open vectors.db directly")
}

// TestEmbeddingsBuildDispatchesToDaemon drives the real `embeddings build`
// command with a live writable daemon runtime record present: the daemon
// accepts the build (202) and immediately reports a completed run, so the
// poll terminates after one status call and prints the final summary.
func TestEmbeddingsBuildDispatchesToDaemon(t *testing.T) {
	dataDir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dataDir, "http://127.0.0.1:1")

	var buildCalled, statusCalled atomic.Bool
	startEmbeddingsTestDaemon(t, dataDir, map[string]http.HandlerFunc{
		"POST /api/v1/embeddings/build": func(w http.ResponseWriter, r *http.Request) {
			buildCalled.Store(true)
			var req vector.BuildRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.True(t, req.Backstop, "--backstop must pass through to the daemon")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]bool{"started": true})
		},
		"GET /api/v1/embeddings/status": func(w http.ResponseWriter, r *http.Request) {
			statusCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(vector.BuildStatus{
				Running: false,
				LastResult: &vector.BuildResult{
					Activated: true,
					Fill:      kitvec.FillStats{Documents: 5, Chunks: 6},
				},
			})
		},
	})

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--backstop"})
	require.NoError(t, cmd.Execute())

	assert.True(t, buildCalled.Load(), "build must route through the daemon endpoint")
	assert.True(t, statusCalled.Load(), "build must poll the daemon status endpoint")
	assert.Contains(t, out.String(), "Embedded 5 documents (6 chunks), skipped 0, stale 0")
	assert.Contains(t, out.String(), "Generation activated.")
	assert.NoFileExists(t, filepath.Join(dataDir, "vectors.db"),
		"daemon-dispatched build must not open vectors.db directly")
}

// TestEmbeddingsActivateDispatchesToDaemon drives the real `embeddings
// activate` command with a live writable daemon runtime record present,
// asserting the id and --force flag pass through to the daemon endpoint.
func TestEmbeddingsActivateDispatchesToDaemon(t *testing.T) {
	dataDir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dataDir, "http://127.0.0.1:1")

	var gotForce atomic.Bool
	startEmbeddingsTestDaemon(t, dataDir, map[string]http.HandlerFunc{
		"POST /api/v1/embeddings/generations/3/activate": func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Force bool `json:"force"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			gotForce.Store(body.Force)
			w.WriteHeader(http.StatusNoContent)
		},
	})

	cmd := newEmbeddingsActivateCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"3", "--force"})
	require.NoError(t, cmd.Execute())

	assert.True(t, gotForce.Load(), "--force must pass through to the daemon")
	assert.Equal(t, "Generation 3 activated.\n", out.String())
}

// TestBuildViaDaemonConflictThenPolls drives buildViaDaemon (the daemon
// build path's core logic) against a fake HTTP server that refuses the
// first build attempt with 409 and reports Running until the second status
// poll, asserting the CLI prints the "already running" notice and then the
// same final summary format the direct path uses.
func TestBuildViaDaemonConflictThenPolls(t *testing.T) {
	orig := embeddingsPollInterval
	embeddingsPollInterval = time.Millisecond
	t.Cleanup(func() { embeddingsPollInterval = orig })

	var statusCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/embeddings/build", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "an embeddings build is already running",
		})
	})
	mux.HandleFunc("/api/v1/embeddings/status", func(w http.ResponseWriter, r *http.Request) {
		n := statusCalls.Add(1)
		status := vector.BuildStatus{Running: n < 2}
		if n >= 2 {
			status.LastResult = &vector.BuildResult{
				Activated: true,
				Fill:      kitvec.FillStats{Documents: 3, Chunks: 3},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := embeddingsDaemonClient{baseURL: srv.URL}
	var out bytes.Buffer
	err := buildViaDaemon(context.Background(), &out, client, vector.BuildRequest{})
	require.NoError(t, err)

	assert.Contains(t, out.String(), "a build is already running (daemon)")
	assert.Contains(t, out.String(), "Embedded 3 documents (3 chunks), skipped 0, stale 0")
	assert.Contains(t, out.String(), "Generation activated.")
	assert.GreaterOrEqual(t, statusCalls.Load(), int32(2))
}

// TestBuildViaDaemonLastErrorReturnsNonZero asserts a stopped build with a
// non-empty LastError becomes the returned error, so the CLI exits
// non-zero, matching the direct path's error propagation.
func TestBuildViaDaemonLastErrorReturnsNonZero(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/embeddings/build", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"started": true})
	})
	mux.HandleFunc("/api/v1/embeddings/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(vector.BuildStatus{
			Running:   false,
			LastError: "encoder rejected input",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := embeddingsDaemonClient{baseURL: srv.URL}
	var out bytes.Buffer
	err := buildViaDaemon(context.Background(), &out, client, vector.BuildRequest{})
	require.Error(t, err)
	assert.Equal(t, "encoder rejected input", err.Error())
}
