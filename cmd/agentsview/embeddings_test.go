package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
model = "test-model"
dimension = 3
max_input_chars = 1000

[vector.embeddings.servers.local]
endpoint = %q
batch_size = 10
timeout = "5s"
max_retries = 1
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
	// Three messages but only TWO embeddable units: the user message plus
	// the contiguous assistant run [1,2]. Tests asserting document counts
	// against this seed are therefore discriminating between unit-grouped
	// and per-message counting.
	dbtest.SeedSessionWithMessages(t, d, "sess-1", "proj", []db.Message{
		dbtest.UserMsg("sess-1", 0, "hello there"),
		dbtest.AsstMsg("sess-1", 1, "hi back"),
		dbtest.AsstMsg("sess-1", 2, "and a follow-up thought"),
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
		dbtest.AsstMsg("sess-1", 2, "and a follow-up thought"),
	})
	dbtest.SeedSessionWithMessages(t, d, "sess-auto", "proj", []db.Message{
		dbtest.UserMsg("sess-auto", 0, "roborev output"),
	}, func(s *db.Session) { s.IsAutomated = true })
}

// TestVectorGenerationParams asserts vectorGeneration's Params map carries
// exactly the three run_v1 fingerprint keys the plan requires, with
// chunk_overlap_chars derived from vector.ChunkOverlap so a future change to
// that formula cannot silently drift from the fingerprint. Empty affixes must
// be absent from the map (not present as ""), so configs written before these
// keys existed keep their fingerprints; non-empty affixes cut a new generation.
func TestVectorGenerationParams(t *testing.T) {
	c := config.VectorEmbeddingsConfig{
		Model:         "test-model",
		Dimension:     3,
		MaxInputChars: 4000,
	}

	gen := vectorGeneration(c)

	assert.Equal(t, "test-model", gen.Model)
	assert.Equal(t, 3, gen.Dimensions)
	assert.Equal(t, map[string]string{
		"max_input_chars":     "4000",
		"doc_unit_scheme":     "run_v1",
		"chunk_overlap_chars": strconv.Itoa(vector.ChunkOverlap(4000)),
	}, gen.Params)
	baselineFingerprint := gen.Fingerprint()

	c.InputSuffix = "<|endoftext|>"
	gen = vectorGeneration(c)
	assert.Equal(t, map[string]string{
		"max_input_chars":     "4000",
		"doc_unit_scheme":     "run_v1",
		"chunk_overlap_chars": strconv.Itoa(vector.ChunkOverlap(4000)),
		"input_suffix":        "<|endoftext|>",
	}, gen.Params)
	assert.NotEqual(t, baselineFingerprint, gen.Fingerprint(),
		"adding an input suffix cuts a new generation")

	suffixFingerprint := gen.Fingerprint()
	c.QueryPrefix = "task: search result | query: "
	gen = vectorGeneration(c)
	assert.Equal(t, map[string]string{
		"max_input_chars":     "4000",
		"doc_unit_scheme":     "run_v1",
		"chunk_overlap_chars": strconv.Itoa(vector.ChunkOverlap(4000)),
		"input_suffix":        "<|endoftext|>",
		"query_prefix":        "task: search result | query: ",
	}, gen.Params)
	assert.NotEqual(t, suffixFingerprint, gen.Fingerprint(),
		"adding a query prefix cuts a new generation")

	queryFingerprint := gen.Fingerprint()
	c.DocumentPrefix = "title: none | text: "
	gen = vectorGeneration(c)
	assert.Equal(t, map[string]string{
		"max_input_chars":     "4000",
		"doc_unit_scheme":     "run_v1",
		"chunk_overlap_chars": strconv.Itoa(vector.ChunkOverlap(4000)),
		"input_suffix":        "<|endoftext|>",
		"query_prefix":        "task: search result | query: ",
		"document_prefix":     "title: none | text: ",
	}, gen.Params)
	assert.NotEqual(t, queryFingerprint, gen.Fingerprint(),
		"adding a document prefix cuts a new generation")

	// request_dimensions follows the same only-when-set rule, and enabling it
	// must change the fingerprint even at an unchanged dimension value, so
	// the staleness gate forces a rebuild before reduced query vectors are
	// compared against native-dimension stored vectors.
	nativeFingerprint := gen.Fingerprint()
	c.RequestDimensions = true
	gen = vectorGeneration(c)
	assert.Equal(t, map[string]string{
		"max_input_chars":     "4000",
		"doc_unit_scheme":     "run_v1",
		"chunk_overlap_chars": strconv.Itoa(vector.ChunkOverlap(4000)),
		"input_suffix":        "<|endoftext|>",
		"query_prefix":        "task: search result | query: ",
		"document_prefix":     "title: none | text: ",
		"request_dimensions":  "true",
	}, gen.Params)
	assert.NotEqual(t, nativeFingerprint, gen.Fingerprint(),
		"enabling request_dimensions cuts a new generation")
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
	assert.Contains(t, lines[0], "STORE")
	assert.Contains(t, lines[0], "ID")
	assert.Contains(t, lines[0], "STATE")
	assert.Contains(t, lines[0], "MODEL")
	assert.Contains(t, lines[0], "DIM")
	assert.Contains(t, lines[0], "EMBEDDED")
	assert.Contains(t, lines[0], "MISSING")
	assert.Contains(t, lines[0], "FINGERPRINT")

	assert.Contains(t, lines[1], "messages")
	assert.Contains(t, lines[1], "active")
	assert.Contains(t, lines[1], "test-model")
	assert.Contains(t, lines[1], truncateFingerprint(fp))
	assert.NotContains(t, lines[1], fp, "fingerprint column must be truncated to 12 chars")
}

// TestEmbeddingsUnknownStoreFails asserts an unrecognized --store value
// fails fast with the known-stores list, before any config-driven daemon
// detection or vectors.db access.
func TestEmbeddingsUnknownStoreFails(t *testing.T) {
	dataDir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dataDir, "http://127.0.0.1:1")

	err := runEmbeddingsList(context.Background(), io.Discard, false, "bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown embedding store "bogus"`)
	assert.Contains(t, err.Error(), "messages")
}

// TestEmbeddingsListShowsStoreColumn asserts `embeddings list --store
// messages` renders the STORE column stamped with the resolved store name.
func TestEmbeddingsListShowsStoreColumn(t *testing.T) {
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

	var buf bytes.Buffer
	require.NoError(t, runEmbeddingsList(context.Background(), &buf, false, "messages"))
	assert.Contains(t, buf.String(), "STORE")
	assert.Contains(t, buf.String(), "messages")
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
	assert.Equal(t, "messages", body.Generations[0].Store)
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

// TestEmbeddingsBuildDirectEndToEnd seeds a temp archive with one user
// message and a two-message assistant run (two embeddable units), points
// [vector.embeddings] at an httptest OpenAI-compatible stub, and asserts the
// direct build path embeds both units and prints the exact final summary and
// activation line.
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

// TestRoleAwarePrefixesReachBuildAndSearch exercises the user-visible role
// split through a real direct build, vectors.db, and SQLite semantic search.
// The embeddings server observes document-prefixed build inputs and a
// query-prefixed search input, with the shared suffix applied last.
func TestRoleAwarePrefixesReachBuildAndSearch(t *testing.T) {
	dataDir := t.TempDir()
	seedEmbeddableArchive(t, dataDir)

	var mu sync.Mutex
	var captured []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		mu.Lock()
		captured = append(captured, req.Input...)
		mu.Unlock()

		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{
				"index": i, "embedding": []float32{1, 2, 3},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	defer stub.Close()

	cfg := vectorTestConfig(dataDir)
	server := cfg.Vector.Embeddings.Servers["local"]
	server.Endpoint = stub.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = server
	cfg.Vector.Embeddings.QueryPrefix = "query: "
	cfg.Vector.Embeddings.DocumentPrefix = "document: "
	cfg.Vector.Embeddings.InputSuffix = "<eos>"

	var buildOut bytes.Buffer
	require.NoError(t, runEmbeddingsBuildDirect(
		context.Background(), &buildOut, cfg, vector.BuildRequest{}))

	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	closeVector := installDirectVectorSearcher(cfg, database)
	require.NotNil(t, closeVector)
	t.Cleanup(func() { require.NoError(t, closeVector()) })

	result, err := database.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern:        "find greeting",
		Mode:           "semantic",
		Limit:          5,
		IncludeOneShot: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Matches,
		"semantic search should return the indexed session")

	mu.Lock()
	got := append([]string(nil), captured...)
	mu.Unlock()
	require.Len(t, got, 3, "two indexed units and one search query should be encoded")
	assert.Contains(t, got, "document: hello there<eos>")
	assert.Contains(t, got, "document: hi back\n\nand a follow-up thought<eos>")
	assert.Contains(t, got, "query: find greeting<eos>")
}

func TestRunDirectBuildPrintsFailedAttemptResult(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := vector.Open(ctx, path, false, 8192)
	require.NoError(t, err)
	defer ix.Close()
	gen := kitvec.Generation{Model: "fake-model", Dimensions: 4}
	src := testPushUnitSource()
	_, err = ix.Build(ctx, src, fakePushEncoder(), gen, vector.BuildOptions{})
	require.NoError(t, err)

	raw, err := sql.Open(vectorTestDriverName, path)
	require.NoError(t, err)
	var ordinal int64
	require.NoError(t, raw.QueryRow(
		`SELECT ordinal FROM message_vectors_generations WHERE gen_key = ?`,
		gen.Fingerprint()).Scan(&ordinal))
	_, err = raw.Exec(`UPDATE message_vectors_v`+strconv.FormatInt(ordinal, 10)+` SET embedding = ?`,
		make([]byte, 4*4))
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	failingEncoder := func(context.Context, []string) ([][]float32, error) {
		return nil, &vector.HTTPStatusError{
			Status: http.StatusBadRequest,
			Body:   "input exceeds token limit",
		}
	}
	m := vector.NewManager(ix, src, vector.EncoderSet{
		Default: "default",
		ByName: map[string]vector.ManagedEncoder{
			"default": {Encode: failingEncoder},
		},
	}, gen)

	var out bytes.Buffer
	err = runDirectBuild(ctx, &out, m, vector.BuildRequest{RepairInvalid: true})
	require.ErrorContains(t, err, "3 permanently rejected")
	status := m.Status()
	require.NotNil(t, status.LastResult)
	assert.True(t, status.LastResult.Repair.ScanComplete)
	assert.Equal(t, 3, status.LastResult.Repair.Failed)
	assert.Equal(t, 3, status.LastResult.Repair.Remaining)
	assert.True(t, status.LastResult.Repair.RemainingKnown)
	assert.Contains(t, out.String(), "Repair targets: 3 documents (3 chunks invalidated).")
	assert.Contains(t, out.String(), "Repair incomplete: 3 failed, 3 remaining.")
	assert.Contains(t, out.String(), "Embedded 0 documents (0 chunks), skipped 0, stale 0")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/embeddings/build", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/embeddings/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(status))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var daemonOut bytes.Buffer
	err = buildViaDaemon(ctx, &daemonOut, embeddingsDaemonClient{baseURL: srv.URL},
		vector.BuildRequest{RepairInvalid: true})
	require.ErrorContains(t, err, "3 permanently rejected")
	assert.Contains(t, daemonOut.String(), "Repair incomplete: 3 failed, 3 remaining.")
}

func TestRunDirectBuildPrintsCommittedTargetsAfterScanFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := vector.Open(ctx, path, false, 8192)
	require.NoError(t, err)
	defer ix.Close()
	gen := kitvec.Generation{Model: "fake-model", Dimensions: 4}
	const documentCount = 129
	src := fakePushUnitSource{units: make([]db.EmbeddableUnit, 0, documentCount)}
	for i := range documentCount {
		id := fmt.Sprintf("doc-%03d", i)
		src.units = append(src.units, db.EmbeddableUnit{
			SessionID: "session-1", Kind: "user", SourceUUID: id,
			Ordinal: i, OrdinalEnd: i, Content: id,
		})
	}
	_, err = ix.Build(ctx, src, fakePushEncoder(), gen, vector.BuildOptions{})
	require.NoError(t, err)

	raw, err := sql.Open(vectorTestDriverName, path)
	require.NoError(t, err)
	var ordinal int64
	require.NoError(t, raw.QueryRow(
		`SELECT ordinal FROM message_vectors_generations WHERE gen_key = ?`,
		gen.Fingerprint()).Scan(&ordinal))
	_, err = raw.Exec(`UPDATE message_vectors_v`+strconv.FormatInt(ordinal, 10)+` SET embedding = ?`,
		make([]byte, 4*4))
	require.NoError(t, err)
	_, err = raw.Exec(`
CREATE TRIGGER fail_second_repair_batch
BEFORE INSERT ON message_vectors_repair_queue
WHEN NEW.doc_key = 'u:session-1:doc-128'
BEGIN
    SELECT RAISE(ABORT, 'injected later repair batch failure');
END`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	m := vector.NewManager(ix, src, vector.EncoderSet{
		Default: "default",
		ByName: map[string]vector.ManagedEncoder{
			"default": {Encode: func(context.Context, []string) ([][]float32, error) {
				t.Fatal("scan failure must stop before refill")
				return nil, nil
			}},
		},
	}, gen)
	var out bytes.Buffer
	err = runDirectBuild(ctx, &out, m, vector.BuildRequest{RepairInvalid: true})
	require.ErrorContains(t, err, "injected later repair batch failure")
	status := m.Status()
	require.NotNil(t, status.LastResult)
	assert.True(t, status.LastResult.Repair.Scanned)
	assert.False(t, status.LastResult.Repair.ScanComplete)
	assert.Equal(t, 128, status.LastResult.Repair.Documents)
	assert.Equal(t, 128, status.LastResult.Repair.Remaining)
	assert.True(t, status.LastResult.Repair.RemainingKnown)
	assert.Contains(t, out.String(), "Repair targets: 128 documents (128 chunks invalidated).")
	assert.Contains(t, out.String(), "Repair scan incomplete.")
	assert.Contains(t, out.String(), "Repair incomplete: 0 failed, 128 remaining.")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/embeddings/build", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/embeddings/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(status))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var daemonOut bytes.Buffer
	err = buildViaDaemon(ctx, &daemonOut, embeddingsDaemonClient{baseURL: srv.URL},
		vector.BuildRequest{RepairInvalid: true})
	require.ErrorContains(t, err, "injected later repair batch failure")
	assert.Contains(t, daemonOut.String(), "Repair scan incomplete.")
	assert.Contains(t, daemonOut.String(), "Repair incomplete: 0 failed, 128 remaining.")
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

	// The scan-phase line is the deterministic progress signal here: the
	// build reports "scanning" until the stub's first (deliberately slow)
	// encode call reports chunk counters, and the poll ticker fires many
	// times inside that window. A chunk-counter line may or may not appear
	// depending on how quickly the build finishes after that first report.
	assert.Contains(t, out.String(),
		"progress: scanning archive for changed documents...",
		"a running build must print at least one progress line")
	assert.Contains(t, out.String(), "Embedded 2 documents (2 chunks), skipped 0, stale 0")
}

// TestPrintBuildProgressPhases pins per-phase progress rendering: the scan
// phase (and an empty phase from a daemon predating the scanning report)
// prints the scan line instead of a misleading "0/0 chunks", while the
// embedding phase prints chunk counters.
func TestPrintBuildProgressPhases(t *testing.T) {
	tests := []struct {
		name  string
		phase string
		done  int64
		total int64
		want  string
	}{
		{
			name: "scanning phase", phase: "scanning",
			want: "progress: scanning archive for changed documents...\n",
		},
		{
			name: "empty phase without totals treated as scanning", phase: "",
			want: "progress: scanning archive for changed documents...\n",
		},
		{
			name: "embedding phase with zero chunks", phase: "embedding",
			want: "progress: 0/0 chunks (0.0%)\n",
		},
		{
			name: "embedding phase with counters", phase: "embedding",
			done: 587, total: 6004,
			want: "progress: 587/6004 chunks (9.8%)\n",
		},
		{
			name: "totals without phase still print counters", phase: "",
			done: 1, total: 4,
			want: "progress: 1/4 chunks (25.0%)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			printBuildProgress(&out, tt.phase, tt.done, tt.total)
			assert.Equal(t, tt.want, out.String())
		})
	}
}

// TestBuildProgressPrinterDeduplicates pins that unchanged status between
// polls prints nothing: a long scan renders one scan line, not one per poll,
// and a stalled chunk counter is not repeated.
func TestBuildProgressPrinterDeduplicates(t *testing.T) {
	var out bytes.Buffer
	var p buildProgressPrinter
	p.print(&out, "scanning", 0, 0)
	p.print(&out, "scanning", 0, 0)
	p.print(&out, "scanning", 0, 0)
	p.print(&out, "embedding", 10, 40)
	p.print(&out, "embedding", 10, 40)
	p.print(&out, "embedding", 17, 40)
	assert.Equal(t,
		"progress: scanning archive for changed documents...\n"+
			"progress: 10/40 chunks (25.0%)\n"+
			"progress: 17/40 chunks (42.5%)\n",
		out.String())
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
// embeddable-unit count (the seeded user message and the assistant run are
// one document each), and a "no" answer aborts without building.
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

	assert.Contains(t, out.String(), "This re-embeds all 2 documents. Continue?")
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
model = "test-model"
dimension = 3
max_input_chars = 1000

[vector.embeddings.servers.local]
endpoint = %q
batch_size = 10
timeout = "5s"
max_retries = 1
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
model = "test-model"
dimension = 3
max_input_chars = 1000

[vector.embeddings.servers.local]
endpoint = %q
batch_size = 10
timeout = "5s"
max_retries = 1
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
model = "test-model"
dimension = 3
max_input_chars = 1000

[vector.embeddings.servers.local]
endpoint = "http://127.0.0.1:1"
batch_size = 10
timeout = "5s"
max_retries = 1
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
		"generation 2 still has 2 documents needing embedding; use --force",
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

func TestResolveEmbeddingsDaemonClientReportsIncompatibleDaemon(t *testing.T) {
	dataDir := runtimeTestDir(t)
	writeLiveRuntime(t, dataDir, false,
		withRuntimeVersion("old"),
		withRuntimeAPIVersion(daemonAPIVersion-1),
	)
	require.True(t, IsLocalDaemonActive(dataDir))
	require.Nil(t, FindDaemonRuntime(dataDir))

	_, err := resolveEmbeddingsDaemonClient(config.Config{DataDir: dataDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon API version")
	assert.Contains(t, err.Error(), "restart the daemon after upgrading AgentsView")
}

func TestResolveEmbeddingsDaemonClientPrefersIncompatibleWritableDaemonError(
	t *testing.T,
) {
	dataDir := runtimeTestDir(t)
	readOnlyEndpoint, _ := writeLiveRuntime(t, dataDir, true)

	writablePID := startSleepProcess(t)
	writableEndpoint := newPingDaemonWithPID(t, writablePID)
	writablePath, err := writeRuntimeRecordForTest(dataDir, daemonRuntimeRecord(
		writableEndpoint.Host, writableEndpoint.Port,
		withRuntimePID(writablePID),
		withRuntimeVersion("old"),
		withRuntimeAPIVersion(daemonAPIVersion-1),
	))
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(writablePath) })

	compatible := FindDaemonRuntime(dataDir)
	require.NotNil(t, compatible)
	require.True(t, compatible.ReadOnly)
	assert.Equal(t, readOnlyEndpoint.Port, compatible.Port)

	_, err = resolveEmbeddingsDaemonClient(config.Config{DataDir: dataDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon API version")
	assert.Contains(t, err.Error(), "restart the daemon after upgrading AgentsView")
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
			assert.False(t, req.Backstop)
			assert.True(t, req.RepairInvalid,
				"--repair-invalid must pass through to the daemon")
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
					Repair: vector.RepairStats{
						Scanned: true, ScanComplete: true, RemainingKnown: true,
						Documents: 2, Chunks: 3,
					},
					Fill: kitvec.FillStats{Documents: 5, Chunks: 6},
				},
			})
		},
	})

	cmd := newEmbeddingsBuildCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--repair-invalid"})
	require.NoError(t, cmd.Execute())

	assert.True(t, buildCalled.Load(), "build must route through the daemon endpoint")
	assert.True(t, statusCalled.Load(), "build must poll the daemon status endpoint")
	assert.Contains(t, out.String(), "Embedded 5 documents (6 chunks), skipped 0, stale 0")
	assert.Contains(t, out.String(), "Repair targets: 2 documents (3 chunks invalidated).")
	assert.Contains(t, out.String(), "Generation activated.")
	assert.NoFileExists(t, filepath.Join(dataDir, "vectors.db"),
		"daemon-dispatched build must not open vectors.db directly")
}

func TestEmbeddingsBuildRejectsBackstopWithRepair(t *testing.T) {
	cmd := newEmbeddingsBuildCommand()
	require.NoError(t, cmd.Flags().Set("backstop", "true"))
	require.NoError(t, cmd.Flags().Set("repair-invalid", "true"))

	err := cmd.ValidateFlagGroups()
	require.Error(t, err)
	assert.ErrorContains(t, err, "if any flags in the group")
}

func TestEmbeddingsBuildRejectsRepairWithFullRebuild(t *testing.T) {
	cmd := newEmbeddingsBuildCommand()
	cmd.SetArgs([]string{"--repair-invalid", "--full-rebuild", "--yes"})

	err := cmd.Execute()

	require.Error(t, err)
	assert.ErrorContains(t, err, "[full-rebuild repair-invalid]")
}

func TestPrintBuildSummaryReportsCleanRepairScan(t *testing.T) {
	var out bytes.Buffer
	printBuildSummary(&out, vector.BuildResult{
		Repair: vector.RepairStats{
			Scanned: true, ScanComplete: true, RemainingKnown: true,
		},
	})

	assert.Contains(t, out.String(), "Repair targets: 0 documents (0 chunks invalidated).")
}

func TestPrintBuildSummaryReportsUnknownRemainingCount(t *testing.T) {
	var out bytes.Buffer
	printBuildSummary(&out, vector.BuildResult{
		Repair: vector.RepairStats{
			Scanned: true, ScanComplete: false, Documents: 7, Remaining: 7,
		},
	})

	assert.Contains(t, out.String(), "Repair scan incomplete.")
	assert.Contains(t, out.String(), "Repair incomplete: 0 failed, remaining unknown.")
	assert.NotContains(t, out.String(), "7 remaining")
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

// TestBuildViaDaemonAlwaysSendsIncludeAutomated pins the wire contract for
// CLI-initiated daemon builds: include_automated must be present in the
// request body even when false, because the daemon treats an omitted field
// as "use my configured scope" — omitting it would let a daemon configured
// with include_automated = true silently ignore an explicit
// --include-automated=false override.
func TestBuildViaDaemonAlwaysSendsIncludeAutomated(t *testing.T) {
	for _, includeAutomated := range []bool{false, true} {
		t.Run(fmt.Sprintf("include_automated=%t", includeAutomated), func(t *testing.T) {
			var body map[string]json.RawMessage
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/embeddings/build", func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]bool{"started": true})
			})
			mux.HandleFunc("/api/v1/embeddings/status", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(vector.BuildStatus{Running: false})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			client := embeddingsDaemonClient{baseURL: srv.URL}
			var out bytes.Buffer
			err := buildViaDaemon(context.Background(), &out, client,
				vector.BuildRequest{IncludeAutomated: includeAutomated})
			require.NoError(t, err)

			raw, present := body["include_automated"]
			require.True(t, present,
				"include_automated must always be sent; omitted means daemon-config scope")
			assert.Equal(t, fmt.Sprintf("%t", includeAutomated), string(raw))
		})
	}
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
			LastResult: &vector.BuildResult{
				Repair: vector.RepairStats{
					Scanned: true, ScanComplete: true, RemainingKnown: true,
					Documents: 2, Chunks: 3, Failed: 1, Remaining: 1,
				},
				Fill: kitvec.FillStats{Documents: 1, Chunks: 1},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := embeddingsDaemonClient{baseURL: srv.URL}
	var out bytes.Buffer
	err := buildViaDaemon(context.Background(), &out, client, vector.BuildRequest{})
	require.Error(t, err)
	assert.Equal(t, "encoder rejected input", err.Error())
	assert.Contains(t, out.String(), "Repair targets: 2 documents (3 chunks invalidated).")
	assert.Contains(t, out.String(), "Repair incomplete: 1 failed, 1 remaining.")
	assert.Contains(t, out.String(), "Embedded 1 documents (1 chunks), skipped 0, stale 0")
}

// TestDirectListGenerationsVersionMismatchSurfacesRebuildRequired pins the
// `embeddings list` direct path's version gate: against a vectors.db written
// by an older mirror schema version it must surface the rebuild-required
// error instead of listing stale-shape generation data.
func TestDirectListGenerationsVersionMismatchSurfacesRebuildRequired(t *testing.T) {
	dataDir := t.TempDir()
	cfg := vectorTestConfig(dataDir)
	path := cfg.Vector.ResolvedDBPath(dataDir)

	seed, err := vector.Open(context.Background(), path, false, cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	require.NoError(t, seed.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.Exec(`UPDATE vector_meta SET value = '2' WHERE key = 'mirror_schema_version'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	_, err = directListGenerations(context.Background(), cfg, vector.MessageIndexSpec())
	require.Error(t, err)
	assert.ErrorIs(t, err, vector.ErrMirrorVersionMismatch,
		"a stale-shape vectors.db must not be listed as if it were current")
	assert.Contains(t, err.Error(), "embeddings build",
		"the error must carry the rebuild remediation")
}
