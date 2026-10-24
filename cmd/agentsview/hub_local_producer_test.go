package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/vector"
)

func localProducerRequest(t *testing.T, extra string) vector.BuildRequest {
	t.Helper()
	var req vector.BuildRequest
	require.NoError(t, json.Unmarshal([]byte(`{"local_producer":true`+extra+`}`), &req))
	return req
}

func TestHubLocalProducerFillsOnlyMissing(t *testing.T) {
	cfg, database, _ := hubProducerFixture(t)
	for _, id := range []string{"local", "foreign", "other~mirror", "auto"} {
		require.NoError(t, database.SetSessionDataVersion(t.Context(), id, db.CurrentDataVersion()))
	}
	ix, err := vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
	require.NoError(t, err)
	defer ix.Close()
	_, err = hubProducerBuild(ix, database, cfg)(t.Context(), vector.BuildRequest{}, nil)
	require.NoError(t, err)
	dbtest.SeedSessionWithMessages(t, database, "missing", "project", []db.Message{dbtest.UserMsg("missing", 0, "missing content")})
	require.NoError(t, database.SetSessionDataVersion(t.Context(), "missing", db.CurrentDataVersion()))
	g := vectorGeneration(publishedRecipeConfig("hub-model"))
	var encoded atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vectors/generation":
			assert.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()}))
		case "/api/v1/vectors/lookup":
			var req vector.HubLookupRequest
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) || !assert.Len(t, req.Identities, 1) {
				return
			}
			assert.Equal(t, "node~missing", req.Identities[0].SessionID)
			assert.NoError(t, json.MarshalWrite(w, vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: g.Fingerprint(), Absent: []int{0}}))
		case "/v1/embeddings":
			encoded.Add(1)
			var body struct {
				Model      string   `json:"model"`
				Input      []string `json:"input"`
				Dimensions int      `json:"dimensions"`
			}
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) || !assert.Len(t, body.Input, 1) {
				return
			}
			assert.Equal(t, "hub-model", body.Model)
			if body.Input[0] == "query: needle" {
				assert.Equal(t, []string{"query: needle"}, body.Input)
			} else {
				assert.Equal(t, []string{"passage: missing content"}, body.Input)
			}
			assert.Equal(t, 4, body.Dimensions)
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0, 0, 0}}}}))
		default:
			assert.Fail(t, "unexpected endpoint", "%s", r.URL.Path)
		}
	}))
	defer endpoint.Close()
	cfg.Remote.URL = endpoint.URL
	server := cfg.Vector.Embeddings.Servers["local"]
	server.Endpoint = endpoint.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = server
	_, err = hubProducerBuild(ix, database, cfg)(t.Context(), vector.BuildRequest{}, nil)
	require.NoError(t, err)
	before, err := ix.HubPullStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, "hash mismatch or hub coverage missing", before.Reason)
	require.EqualValues(t, 1, before.Unmatched)
	sourceVersion, err := database.EmbeddingDataVersion(t.Context(), false)
	require.NoError(t, err)
	require.Equal(t, db.CurrentDataVersion(), sourceVersion)
	m := embeddingManager(ix, database, vector.EncoderSet{}, cfg, "messages")
	defer m.Shutdown()
	ran, err := m.TryBuild(t.Context(), localProducerRequest(t, `,"using":"local"`))
	require.NoError(t, err)
	require.True(t, ran)
	require.EqualValues(t, 1, encoded.Load())
	gens, err := ix.Generations(t.Context())
	require.NoError(t, err)
	require.Len(t, gens, 1)
	require.Equal(t, g.Fingerprint(), gens[0].Fingerprint)
	require.EqualValues(t, 2, gens[0].Embedded)
	require.Zero(t, gens[0].Missing)
	require.Equal(t, "active", gens[0].State)
	exported, ok, err := ix.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, db.CurrentDataVersion(), exported.Generation().DataVersion)
	require.NoError(t, exported.Close())
	m.Shutdown()
	require.NoError(t, ix.Close())
	assertHubStatusReporting(t, cfg, vector.HubPullStatus{Fingerprint: g.Fingerprint(), Unmatched: 0})
	cfg.Remote.TokenFile = filepath.Join(t.TempDir(), "missing-token")
	cfg.Remote.URL = "http://127.0.0.1:1"
	closeSearch := installDirectVectorSearcher(cfg, database)
	require.NotNil(t, closeSearch)
	defer func() { require.NoError(t, closeSearch()) }()
	page, err := database.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "needle", Mode: "semantic", IncludeOneShot: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Matches, 2)
}

func TestHubLocalProducerRefusesRebuildAndRecall(t *testing.T) {
	for _, mode := range []string{"full_rebuild", "backstop", "recall"} {
		t.Run(mode, func(t *testing.T) {
			cfg, database, calls := hubProducerFixture(t)
			spec := vector.MessageIndexSpec()
			extra := `,"` + mode + `":true`
			if mode == "recall" {
				spec = vector.RecallIndexSpec()
				extra = ""
			}
			ix, err := vector.OpenSpec(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), spec, false, 1000)
			require.NoError(t, err)
			defer ix.Close()
			m := embeddingManager(ix, database, vector.EncoderSet{}, cfg, spec.Name)
			defer m.Shutdown()
			require.ErrorIs(t, m.StartBuild(localProducerRequest(t, extra)), vector.ErrInvalidBuildRequest)
			require.Zero(t, calls.Load())
		})
	}
}

func TestHubPullStatusSurvivesReopen(t *testing.T) {
	for _, reason := range []string{"data version mismatch", "automated scope mismatch", "recipe mismatch", "hash mismatch or hub coverage missing", "hub pull unavailable"} {
		t.Run(reason, func(t *testing.T) {
			cfg, database, _ := hubProducerFixture(t)
			healthy := cfg
			req := vector.BuildRequest{IncludeAutomated: reason == "automated scope mismatch"}
			if reason == "recipe mismatch" {
				cfg.Vector.Embeddings.RecipeKeys = map[string]bool{"model": true}
			}
			if reason == "hub pull unavailable" {
				cfg.Remote.TokenFile = filepath.Join(t.TempDir(), "missing-token")
			}
			if reason == "hash mismatch or hub coverage missing" || reason == "data version mismatch" {
				version := db.CurrentDataVersion()
				if reason == "data version mismatch" {
					version--
				}
				g := vectorGeneration(publishedRecipeConfig("hub-model"))
				hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						assert.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: version}))
					} else {
						assert.NoError(t, json.MarshalWrite(w, vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: g.Fingerprint(), Absent: []int{0}}))
					}
				}))
				defer hub.Close()
				cfg.Remote.URL = hub.URL
			}
			ix, err := vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
			require.NoError(t, err)
			_, err = hubProducerBuild(ix, database, cfg)(t.Context(), req, nil)
			missing := int64(-1)
			fingerprint := ""
			if reason == "hash mismatch or hub coverage missing" {
				require.NoError(t, err)
				missing = 1
				fingerprint = vectorGeneration(publishedRecipeConfig("hub-model")).Fingerprint()
			} else {
				require.Error(t, err)
			}
			require.NoError(t, ix.Close())
			assertHubStatusReporting(t, cfg, vector.HubPullStatus{Fingerprint: fingerprint, Unmatched: missing, Reason: reason})

			ix, err = vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
			require.NoError(t, err)
			if missing > 0 {
				cfg.Remote.TokenFile = filepath.Join(t.TempDir(), "missing-token")
				_, err = hubProducerBuild(ix, database, cfg)(t.Context(), vector.BuildRequest{}, nil)
				require.Error(t, err)
				require.NoError(t, ix.Close())
				assertHubStatusReporting(t, cfg, vector.HubPullStatus{Fingerprint: fingerprint, Unmatched: missing, Reason: "hub pull unavailable"})
				ix, err = vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
				require.NoError(t, err)
			}
			result, err := hubProducerBuild(ix, database, healthy)(t.Context(), vector.BuildRequest{}, nil)
			require.NoError(t, err)
			require.NoError(t, ix.Close())
			assertHubStatusReporting(t, healthy, vector.HubPullStatus{Fingerprint: result.Fingerprint, Unmatched: 0})
		})
	}
}

func assertHubStatusReporting(t *testing.T, cfg config.Config, want vector.HubPullStatus) {
	t.Helper()
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	text := fmt.Sprintf(`[vector]
enabled = true
producer = "remote"
[vector.embeddings.servers.local]
endpoint = "http://127.0.0.1:1/v1"
[remote]
url = "http://127.0.0.1:1"
spoke_host = "node"
allow_insecure_http = true
token_file = %q
`, filepath.Join(cfg.DataDir, "missing-token"))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "config.toml"), []byte(text), 0o600))
	status, err := readHubPullStatus(t.Context(), cfg)
	require.NoError(t, err)
	require.Equal(t, &want, status)
	var out bytes.Buffer
	require.NoError(t, runEmbeddingsList(t.Context(), &out, true, ""))
	var got struct {
		HubPull vector.HubPullStatus `json:"hub_pull"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Equal(t, want, got.HubPull)
	out.Reset()
	cmd := newDoctorCommand()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"embeddings"})
	require.NoError(t, cmd.Execute())
	line := "Hub vectors: unmatched document count unavailable"
	if want.Unmatched >= 0 {
		line = fmt.Sprintf("Hub vectors: %d unmatched documents", want.Unmatched)
	}
	if want.Reason != "" {
		line += "; " + want.Reason
	}
	expected := "Embedding producer: remote\n" + line + "\n"
	if want.Unmatched > 0 {
		expected += "Fill unmatched documents with 'agentsview embeddings build --local-producer'.\n"
	}
	require.Equal(t, expected, out.String())
}

// A repair-only request must not discover or encode unrelated source documents.
func TestHubLocalProducerRepairDoesNotDiscoverUnrelatedDocuments(t *testing.T) {
	t.Run("zero targets", func(t *testing.T) {
		cfg, database, _ := hubProducerFixture(t)
		ix, err := vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
		require.NoError(t, err)
		defer ix.Close()
		_, err = hubProducerBuild(ix, database, cfg)(t.Context(), vector.BuildRequest{}, nil)
		require.NoError(t, err)
		dbtest.SeedSessionWithMessages(t, database, "unrelated", "project", []db.Message{dbtest.UserMsg("unrelated", 0, "unrelated content")})
		var encoded atomic.Int32
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			encoded.Add(1)
			var body struct {
				Input []string `json:"input"`
			}
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
				return
			}
			data := make([]map[string]any, len(body.Input))
			for i := range body.Input {
				data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0}}
			}
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
		}))
		defer endpoint.Close()
		server := cfg.Vector.Embeddings.Servers["local"]
		server.Endpoint = endpoint.URL + "/v1"
		cfg.Vector.Embeddings.Servers["local"] = server
		m := embeddingManager(ix, database, vector.EncoderSet{}, cfg, vector.MessageIndexSpec().Name)
		defer m.Shutdown()
		ran, err := m.TryBuild(t.Context(), vector.BuildRequest{RepairInvalid: true, LocalProducer: true})
		require.NoError(t, err)
		require.True(t, ran)
		assert.Zero(t, encoded.Load(), "zero repair targets must not encode an unrelated new document")
		gens, err := ix.Generations(t.Context())
		require.NoError(t, err)
		require.Len(t, gens, 1)
		assert.EqualValues(t, 1, gens[0].Embedded)
		assert.Zero(t, gens[0].Missing)
		assert.Equal(t, "active", gens[0].State)
	})

	for _, mode := range []string{"active absent", "staged absent", "transport", "canceled", "source changed during lookup"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			cfg, database, _ := hubProducerFixture(t)
			path := cfg.Vector.ResolvedDBPath(cfg.DataDir)
			ix, err := vector.Open(ctx, path, false, 1000)
			require.NoError(t, err)
			defer ix.Close()
			_, err = hubProducerBuild(ix, database, cfg)(ctx, vector.BuildRequest{}, nil)
			require.NoError(t, err)
			oldGen := vectorGeneration(publishedRecipeConfig("hub-model"))
			gen := oldGen
			staged := mode == "staged absent"
			if staged {
				gen = vectorGeneration(publishedRecipeConfig("replacement-model"))
				dbtest.SeedSessionWithMessages(t, database, "pending", "project", []db.Message{dbtest.UserMsg("pending", 0, "pending content")})
			}
			preparingStage, finalAttempt := staged, false
			attemptCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var encoded atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/vectors/generation":
					assert.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: gen.Dimensions, Params: gen.Params, DataVersion: db.CurrentDataVersion()}))
				case "/api/v1/vectors/lookup":
					var req vector.HubLookupRequest
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
						return
					}
					reply := vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: gen.Fingerprint()}
					if preparingStage {
						for i, id := range req.Identities {
							if id.SessionID == "node~local" {
								reply.Chunks = append(reply.Chunks, vector.HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAAAAAAAAAAAAAAA=="})
							} else {
								assert.Equal(t, "node~pending", id.SessionID)
								reply.Absent = append(reply.Absent, i)
							}
						}
					} else {
						if !assert.Len(t, req.Identities, 1) {
							return
						}
						assert.Equal(t, "node~local", req.Identities[0].SessionID)
						if finalAttempt {
							switch mode {
							case "transport":
								w.WriteHeader(http.StatusServiceUnavailable)
								return
							case "canceled":
								cancel()
							case "source changed during lookup":
								assert.NoError(t, database.ReplaceSessionMessages(ctx, "local", []db.Message{dbtest.UserMsg("local", 0, "changed content")}))
							}
						}
						reply.Absent = []int{0}
					}
					assert.NoError(t, json.MarshalWrite(w, reply))
				case "/v1/embeddings":
					encoded.Add(1)
					var body struct {
						Input []string `json:"input"`
					}
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
						return
					}
					assert.Equal(t, []string{"passage: content"}, body.Input)
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0, 0, 0}}}}))
				default:
					assert.Fail(t, "unexpected endpoint", "%s", r.URL.Path)
				}
			}))
			defer endpoint.Close()
			cfg.Remote.URL = endpoint.URL
			server := cfg.Vector.Embeddings.Servers["local"]
			server.Endpoint = endpoint.URL + "/v1"
			cfg.Vector.Embeddings.Servers["local"] = server
			m := embeddingManager(ix, database, vector.EncoderSet{}, cfg, vector.MessageIndexSpec().Name)
			defer m.Shutdown()
			if staged {
				ran, err := m.TryBuild(ctx, vector.BuildRequest{})
				require.NoError(t, err)
				require.True(t, ran)
				active, ok, err := ix.ActiveFingerprint(ctx)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, oldGen.Fingerprint(), active)
			}
			preparingStage = false
			raw, err := sql.Open(vectorTestDriverName, path)
			require.NoError(t, err)
			defer raw.Close()
			prefix := "message_vectors"
			if staged {
				require.NoError(t, raw.QueryRowContext(ctx, "SELECT value FROM vector_meta WHERE key=?", "pull_stage:"+gen.Fingerprint()).Scan(&prefix))
			}
			var ordinal int64
			require.NoError(t, raw.QueryRowContext(ctx, "SELECT ordinal FROM "+prefix+"_generations WHERE gen_key=?", gen.Fingerprint()).Scan(&ordinal))
			// Damage only a real stored vector; the repair operation owns queue creation.
			_, err = raw.ExecContext(ctx, fmt.Sprintf("UPDATE %s_v%d SET embedding=?", prefix, ordinal), make([]byte, 4*4))
			require.NoError(t, err)
			ran, err := m.TryBuild(ctx, vector.BuildRequest{RepairInvalid: true})
			require.True(t, ran)
			require.Error(t, err)
			seedResult := m.Status().LastResult
			require.NotNil(t, seedResult)
			require.True(t, seedResult.Repair.RemainingKnown)
			require.Equal(t, 1, seedResult.Repair.Remaining)
			var queued int
			require.NoError(t, raw.QueryRowContext(ctx, "SELECT count(*) FROM "+prefix+"_repair_queue WHERE ordinal=?", ordinal).Scan(&queued))
			require.Equal(t, 1, queued)
			dbtest.SeedSessionWithMessages(t, database, "unrelated", "project", []db.Message{dbtest.UserMsg("unrelated", 0, "unrelated content")})
			finalAttempt = true
			ran, err = m.TryBuild(attemptCtx, vector.BuildRequest{RepairInvalid: true, LocalProducer: true})
			require.True(t, ran)
			if mode == "transport" || mode == "canceled" || mode == "source changed during lookup" {
				require.Error(t, err)
				if mode == "canceled" {
					require.ErrorIs(t, err, context.Canceled)
				}
				assert.Zero(t, encoded.Load(), "a real pull failure must not be hidden by local repair")
				require.NoError(t, raw.QueryRowContext(ctx, "SELECT count(*) FROM "+prefix+"_repair_queue WHERE ordinal=?", ordinal).Scan(&queued))
				assert.Equal(t, 1, queued)
				return
			}
			require.NoError(t, err, "hub absence must leave the allowed local repair fallback reachable")
			assert.EqualValues(t, 1, encoded.Load())
			repaired := m.Status().LastResult
			require.NotNil(t, repaired)
			assert.True(t, repaired.Repair.RemainingKnown)
			assert.Zero(t, repaired.Repair.Remaining)
			assert.False(t, repaired.Activated)
			require.NoError(t, raw.QueryRowContext(ctx, "SELECT count(*) FROM "+prefix+"_repair_queue WHERE ordinal=?", ordinal).Scan(&queued))
			assert.Zero(t, queued)
			infos, err := ix.Generations(ctx)
			require.NoError(t, err)
			for _, info := range infos {
				if info.Fingerprint == gen.Fingerprint() {
					assert.EqualValues(t, 1, info.Embedded)
					wantMissing := int64(0)
					if staged {
						wantMissing = 1
					}
					assert.Equal(t, wantMissing, info.Missing)
				}
			}
			active, ok, err := ix.ActiveFingerprint(ctx)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, oldGen.Fingerprint(), active)
			exported, ok, err := ix.BeginExport(ctx, nil)
			require.NoError(t, err)
			require.True(t, ok)
			require.NotNil(t, exported.Generation().IncludeAutomated)
			assert.False(t, *exported.Generation().IncludeAutomated)
			docs, _, err := exported.SessionDocs(ctx, "unrelated")
			require.NoError(t, err)
			assert.Empty(t, docs)
			require.NoError(t, exported.Close())
			if staged {
				var kept string
				require.NoError(t, raw.QueryRowContext(ctx, "SELECT value FROM vector_meta WHERE key=?", "pull_stage:"+gen.Fingerprint()).Scan(&kept))
				assert.Equal(t, prefix, kept, "repair leaves ordinary stage publication for a later normal build")
			}
		})
	}
}
