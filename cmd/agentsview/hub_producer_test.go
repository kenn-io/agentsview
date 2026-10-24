package main

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json/v2"
	"io"
	"maps"
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
	"go.kenn.io/agentsview/internal/requestsign"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/testutil"
	"go.kenn.io/agentsview/internal/vector"
)

func hubProducerFixture(t *testing.T, status ...int) (config.Config, *db.DB, *atomic.Int32) {
	t.Helper()
	cfg := vectorTestConfig(t.TempDir())
	cfg.Vector.Producer = "remote"
	cfg.Remote.SpokeHost = "node"
	cfg.Remote.TokenFile = filepath.Join(cfg.DataDir, "hub-token")
	require.NoError(t, os.WriteFile(cfg.Remote.TokenFile, []byte("token"), 0o600))
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	for _, tc := range []struct {
		id, machine, kind string
		automated         bool
	}{{"local", "local", "", false}, {"foreign", "remote", "", false}, {"other~mirror", "local", "", false}, {"auto", "local", "roborev", true}} {
		dbtest.SeedSessionWithMessages(t, database, tc.id, "project", []db.Message{dbtest.UserMsg(tc.id, 0, "content")}, func(s *db.Session) { s.Machine = tc.machine; s.IsAutomated = tc.automated; s.SessionKind = tc.kind })
	}
	g := vectorGeneration(publishedRecipeConfig("hub-model"))
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		if r.Method == http.MethodGet {
			if len(status) > 0 {
				calls.Add(1)
				w.WriteHeader(status[0])
				return
			}
			assert.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()}))
			return
		}
		calls.Add(1)
		var req vector.HubLookupRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			return
		}
		assert.Equal(t, g.Fingerprint(), req.Fingerprint)
		if !assert.Len(t, req.Identities, 1) {
			return
		}
		assert.Equal(t, "node~local", req.Identities[0].SessionID)
		v, err := vector.EncodeHubVector([]float32{1, 0, 0, 0})
		assert.NoError(t, err)
		assert.NoError(t, json.MarshalWrite(w, vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: g.Fingerprint(), Chunks: []vector.HubChunk{{Index: 0, ChunkCount: 1, Vector: v}}}))
	}))
	t.Cleanup(server.Close)
	cfg.Remote.URL = server.URL
	return cfg, database, calls
}

func TestRemoteProducerFactoryWiresHubBuild(t *testing.T) {
	cfg, database, calls := hubProducerFixture(t)
	ix, err := vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
	require.NoError(t, err)
	defer ix.Close()
	var encoded atomic.Int32
	encoders := vector.EncoderSet{Default: "local", ByName: map[string]vector.ManagedEncoder{"local": {Encode: func(context.Context, []string) ([][]float32, error) {
		encoded.Add(1)
		require.Fail(t, "remote producer encoded a document")
		return nil, nil
	}}}}
	m := embeddingManager(ix, database, encoders, cfg, vector.MessageIndexSpec().Name)
	defer m.Shutdown()
	_, err = m.TryBuild(t.Context(), vector.BuildRequest{Using: "typo"})
	require.ErrorIs(t, err, vector.ErrUnknownServer)
	require.Zero(t, calls.Load())
	ran, err := m.TryBuild(t.Context(), vector.BuildRequest{Using: "local"})
	require.NoError(t, err)
	require.True(t, ran)
	require.Zero(t, encoded.Load())
	require.EqualValues(t, 1, calls.Load())
	gens, err := ix.Generations(t.Context())
	require.NoError(t, err)
	require.Len(t, gens, 1)
	require.Equal(t, "active", gens[0].State)
	require.EqualValues(t, 1, gens[0].Embedded)
	require.Zero(t, gens[0].Missing)
	g, ok, err := ix.ActiveHubRecipe(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "hub-model", g.Model)
}

func TestRemoteProducerDirectBuild(t *testing.T) {
	for _, mode := range []string{"file", "environment", "broken file", "changed", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			var status []int
			if mode == "changed" {
				status = []int{409}
			}
			if mode == "unavailable" {
				status = []int{503}
			}
			cfg, _, calls := hubProducerFixture(t, status...)
			t.Setenv("AGENTSVIEW_SERVER_TOKEN", "environment-token")
			if mode == "environment" {
				t.Setenv("AGENTSVIEW_SERVER_TOKEN", "token")
				cfg.Remote.TokenFile = ""
			}
			if mode == "broken file" {
				cfg.Remote.TokenFile += "-missing"
			}
			err := runEmbeddingsBuildDirect(t.Context(), io.Discard, cfg, vector.BuildRequest{})
			if mode == "changed" || mode == "unavailable" {
				require.Error(t, err)
				want := int32(1)
				if mode == "changed" {
					want = 3
				}
				require.Equal(t, want, calls.Load())
				return
			}
			if mode == "broken file" {
				require.Error(t, err)
				require.Zero(t, calls.Load())
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestRemoteProducerRefusesArchivesAwaitingParserWork(t *testing.T) {
	for _, state := range []string{"PRAGMA user_version=129", "INSERT INTO stats(key,value) VALUES ('scoped_upgrade_pending',1)"} {
		t.Run(state, func(t *testing.T) {
			cfg, database, calls := hubProducerFixture(t)
			require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(t.Context(), state); return err }))
			require.ErrorContains(t, runEmbeddingsBuildDirect(t.Context(), io.Discard, cfg, vector.BuildRequest{}), "needs resync")
			require.Zero(t, calls.Load())
		})
	}
}

func TestHubRecipeAdoptsFingerprintAndKeepsQueryTransport(t *testing.T) {
	for _, tc := range []struct {
		model               string
		dimension, maxChars int
	}{{"small", 1, 1}, {"hub", 4, 8192}, {"large", 4096, 32768}} {
		c := publishedRecipeConfig(tc.model)
		c.Dimension, c.MaxInputChars = tc.dimension, tc.maxChars
		g := vectorGeneration(c)
		cfg := config.Config{Vector: config.VectorConfig{Embeddings: config.VectorEmbeddingsConfig{MaxInputChars: 8192, DefaultServer: "query", Servers: map[string]config.VectorEmbeddingsServerConfig{"query": {Endpoint: "http://localhost:8080/v1"}}}}}
		got, err := decodeHubRecipe(cfg, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()})
		require.NoError(t, err)
		require.Equal(t, g.Fingerprint(), vectorGeneration(got).Fingerprint())
		require.Equal(t, cfg.Vector.Embeddings.Servers, got.Servers)
		require.Equal(t, "query", got.DefaultServer)
	}
}

func TestHubRecipeRejectsPersistentIncompatibilities(t *testing.T) {
	for _, kind := range []string{"data version mismatch", "automated scope mismatch", "recipe mismatch", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			c := publishedRecipeConfig("hub")
			g := vectorGeneration(c)
			cfg := config.Config{Vector: config.VectorConfig{Embeddings: c}}
			h := vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()}
			switch kind {
			case "data version mismatch":
				h.DataVersion--
			case "automated scope mismatch":
				h.IncludeAutomated = true
			case "recipe mismatch":
				cfg.Vector.Embeddings.Model = "other"
				cfg.Vector.Embeddings.RecipeKeys = map[string]bool{"model": true}
			case "unavailable":
				h.Unavailable = "metadata unavailable"
			}
			_, err := decodeHubRecipe(cfg, h)
			require.ErrorContains(t, err, kind)
		})
	}
}

func TestHubRecipeChecksExplicitZeroRecipeFields(t *testing.T) {
	c := publishedRecipeConfig("hub")
	g := vectorGeneration(c)
	for _, key := range []string{"model", "dimension", "request_dimensions", "max_input_chars", "query_prefix", "document_prefix", "input_suffix"} {
		t.Run(key, func(t *testing.T) {
			cfg := config.Config{Vector: config.VectorConfig{Embeddings: config.VectorEmbeddingsConfig{RecipeKeys: map[string]bool{key: true}}}}
			h := vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()}
			if key == "input_suffix" {
				h.Params = maps.Clone(g.Params)
				h.Params["input_suffix"] = "suffix"
				c := c
				c.InputSuffix = "suffix"
				h.Fingerprint = vectorGeneration(c).Fingerprint()
			}
			_, err := decodeHubRecipe(cfg, h)
			require.ErrorContains(t, err, "recipe mismatch")
		})
	}
}

func TestRemoteProducerSignsForRestrictedListener(t *testing.T) {
	var verifier *requestsign.Verifier
	g := vectorGeneration(publishedRecipeConfig("hub-model"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifier.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
			if r.Method == http.MethodGet {
				assert.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()}))
				return
			}
			var req vector.HubLookupRequest
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
				return
			}
			if !assert.Len(t, req.Identities, 1) {
				return
			}
			assert.Equal(t, "node~local", req.Identities[0].SessionID)
			assert.NoError(t, json.MarshalWrite(w, vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: g.Fingerprint(), Chunks: []vector.HubChunk{{Index: 0, ChunkCount: 1, Vector: "AACAPwAAAAAAAAAAAAAAAA=="}}}))
		}), nil, nil).ServeHTTP(w, r)
	}))
	defer server.Close()
	dir := testutil.PrivateTempDir(t)
	keyFile := filepath.Join(dir, "key")
	require.NoError(t, requestsign.GenerateSecretFile(keyFile))
	secret, err := requestsign.ReadSecret(keyFile)
	require.NoError(t, err)
	state := filepath.Join(dir, "replay.db")
	require.NoError(t, requestsign.InitReplay(state))
	replay, err := requestsign.OpenReplay(state)
	require.NoError(t, err)
	defer replay.Close()
	verifier, err = requestsign.NewVerifier(requestsign.VerifyConfig{ExternalURL: server.URL, Replay: replay, Keys: func() (map[string]requestsign.Key, error) {
		return map[string]requestsign.Key{"reader": {ID: "reader", Secret: secret}}, nil
	}})
	require.NoError(t, err)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.InsecureSkipVerify = false
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	transport.TLSClientConfig.RootCAs.AddCert(server.Certificate())
	defer transport.CloseIdleConnections()
	oldTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	bare := &vector.HTTPHub{URL: server.URL, Token: "token", Client: &http.Client{Transport: transport}}
	_, err = bare.HubVectorGeneration(t.Context())
	require.ErrorContains(t, err, "401")
	t.Setenv("AGENTSVIEW_SIGNING_URL", server.URL)
	t.Setenv("AGENTSVIEW_SIGNING_KEY_ID", "reader")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_FILE", keyFile)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "token")
	cfg := vectorTestConfig(t.TempDir())
	cfg.Vector.Producer, cfg.Remote.URL, cfg.Remote.SpokeHost = "remote", server.URL, "node"
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	dbtest.SeedSessionWithMessages(t, database, "local", "project", []db.Message{dbtest.UserMsg("local", 0, "content")})
	require.NoError(t, runEmbeddingsBuildDirect(t.Context(), io.Discard, cfg, vector.BuildRequest{}))
}

func TestRemoteProducerExportsAdoptedRecipe(t *testing.T) {
	cfg, _, _ := hubProducerFixture(t)
	cfg.Vector.Embeddings.Model = ""
	cfg.Vector.Embeddings.Dimension = 0
	e := &replicaEmbedder{source: &vectorPushSource{cfg: cfg}, buildOnPrepare: true}
	defer e.Close()
	planned, ok, err := e.PlannedGeneration(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "hub-model", planned.Model)
	require.NotEmpty(t, planned.Params["document_prefix"])
	require.NoError(t, runEmbeddingsBuildDirect(t.Context(), io.Discard, cfg, vector.BuildRequest{}))
	exported, ok, err := e.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, planned.Fingerprint, exported.Generation().Fingerprint)
	require.Equal(t, planned.Params, exported.Generation().Params)
	require.NoError(t, exported.Close())
	// An unrelated active local generation must not be relabeled as the hub's.
	stale := cfg
	stale.Vector.Producer = "local"
	stale.Vector.Embeddings = publishedRecipeConfig("stale")
	buildTestVectorsDB(t, stale)
	_, ok, err = e.BeginExport(t.Context(), nil)
	require.ErrorIs(t, err, storage.ErrVectorSourceNotReady)
	require.False(t, ok)
	// Explicit recipe constraints still refuse publication after adoption.
	e.source.cfg.Vector.Embeddings.RecipeKeys = map[string]bool{"model": true}
	e.source.cfg.Vector.Embeddings.Model = "other"
	_, _, err = e.PlannedGeneration(t.Context())
	require.ErrorContains(t, err, "recipe mismatch")
}
