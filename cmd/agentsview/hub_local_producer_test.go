package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

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
	ix, err := vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
	require.NoError(t, err)
	defer ix.Close()
	_, err = hubProducerBuild(ix, database, cfg)(t.Context(), vector.BuildRequest{}, nil)
	require.NoError(t, err)
	dbtest.SeedSessionWithMessages(t, database, "missing", "project", []db.Message{dbtest.UserMsg("missing", 0, "missing content")})
	g := vectorGeneration(publishedRecipeConfig("hub-model"))
	var encoded atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vectors/generation":
			require.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()}))
		case "/api/v1/vectors/lookup":
			var req vector.HubLookupRequest
			require.NoError(t, json.UnmarshalRead(r.Body, &req))
			require.Len(t, req.Identities, 1)
			require.Equal(t, "node~missing", req.Identities[0].SessionID)
			require.NoError(t, json.MarshalWrite(w, vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: g.Fingerprint(), Absent: []int{0}}))
		case "/v1/embeddings":
			encoded.Add(1)
			var body struct {
				Model      string   `json:"model"`
				Input      []string `json:"input"`
				Dimensions int      `json:"dimensions"`
			}
			require.NoError(t, json.UnmarshalRead(r.Body, &body))
			require.Equal(t, "hub-model", body.Model)
			require.Len(t, body.Input, 1)
			if body.Input[0] == "query: needle" {
				require.Equal(t, []string{"query: needle"}, body.Input)
			} else {
				require.Equal(t, []string{"passage: missing content"}, body.Input)
			}
			require.Equal(t, 4, body.Dimensions)
			require.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0, 0, 0}}}}))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
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
	m := embeddingManager(ix, database, vector.EncoderSet{}, cfg, "messages")
	defer m.Shutdown()
	ran, err := m.TryBuild(t.Context(), localProducerRequest(t, ""))
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
						require.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: version}))
					} else {
						require.NoError(t, json.MarshalWrite(w, vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: g.Fingerprint(), Absent: []int{0}}))
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
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "config.toml"), []byte(text), 0600))
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
