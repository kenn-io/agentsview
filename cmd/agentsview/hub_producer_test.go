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
	return hubProducerScopeFixture(t, false, status...)
}

func hubProducerScopeFixture(t *testing.T, includeAutomated bool, status ...int) (config.Config, *db.DB, *atomic.Int32) {
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
			assert.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion(), IncludeAutomated: includeAutomated}))
			return
		}
		calls.Add(1)
		var req vector.HubLookupRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			return
		}
		assert.Equal(t, g.Fingerprint(), req.Fingerprint)
		want := []string{"node~local"}
		if includeAutomated {
			want = append(want, "node~auto")
		}
		got := make([]string, len(req.Identities))
		for i, identity := range req.Identities {
			got[i] = identity.SessionID
		}
		assert.ElementsMatch(t, want, got)
		v, err := vector.EncodeHubVector([]float32{1, 0, 0, 0})
		assert.NoError(t, err)
		chunks := make([]vector.HubChunk, len(req.Identities))
		for i := range chunks {
			chunks[i] = vector.HubChunk{Index: i, ChunkCount: 1, Vector: v}
		}
		assert.NoError(t, json.MarshalWrite(w, vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: g.Fingerprint(), Chunks: chunks}))
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
	for _, key := range []string{"request_dimensions", "query_prefix", "document_prefix", "input_suffix"} {
		t.Run(key, func(t *testing.T) {
			cfg := vectorTestConfig(t.TempDir())
			cfg.Vector.Producer = "remote"
			transport := cfg.Vector.Embeddings
			cfg.Vector.Embeddings = c
			cfg.Vector.Embeddings.Servers = transport.Servers
			cfg.Vector.Embeddings.DefaultServer = transport.DefaultServer
			cfg.Vector.Embeddings.RecipeKeys = map[string]bool{key: true}
			switch key {
			case "request_dimensions":
				cfg.Vector.Embeddings.RequestDimensions = false
			case "query_prefix":
				cfg.Vector.Embeddings.QueryPrefix = ""
			case "document_prefix":
				cfg.Vector.Embeddings.DocumentPrefix = ""
			case "input_suffix":
				cfg.Vector.Embeddings.InputSuffix = ""
			}
			require.NoError(t, cfg.Vector.Validate())
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
	t.Setenv("AGENTSVIEW_SIGNING_URL", "")
	bare := &httpHub{URL: server.URL, Token: "token", Client: &http.Client{Transport: transport}}
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

// A failed remote replacement survives a config switch to an ordinary local build.
func TestDirectLocalBuildCompletesPersistedHubStage(t *testing.T) {
	cfg, database, _ := hubProducerFixture(t)
	cfg.InstallationID = "installation"
	cfg.Vector.Producer = "local"
	oldEndpoint := newEmbeddingsStubServer(t, 3)
	defer oldEndpoint.Close()
	server := cfg.Vector.Embeddings.Servers["local"]
	server.Endpoint = oldEndpoint.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = server
	require.NoError(t, runEmbeddingsBuildDirect(t.Context(), io.Discard, cfg, vector.BuildRequest{}))
	// An ordinary local build must not adopt or rewrite archive ownership.
	session, err := database.GetSession(t.Context(), "local")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "local", session.Machine)
	g := vectorGeneration(publishedRecipeConfig("hub-model"))
	var lookups atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			assert.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()}))
			return
		}
		lookups.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer hub.Close()
	cfg.Vector.Producer = "remote"
	cfg.Remote.URL = hub.URL
	require.Error(t, runEmbeddingsBuildDirect(t.Context(), io.Discard, cfg, vector.BuildRequest{}))
	require.Positive(t, lookups.Load())
	ix, err := vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), true, 1000)
	require.NoError(t, err)
	gens, err := ix.Generations(t.Context())
	require.NoError(t, err)
	require.Len(t, gens, 2)
	require.Equal(t, "active", gens[0].State)
	require.Equal(t, "building", gens[1].State)
	require.Equal(t, g.Fingerprint(), gens[1].Fingerprint)
	require.Positive(t, gens[1].Missing)
	require.NoError(t, ix.Close())
	endpoint := newEmbeddingsStubServer(t, 4)
	defer endpoint.Close()
	cfg.Vector.Producer = "local"
	cfg.Vector.Embeddings = publishedRecipeConfig("hub-model")
	server.Endpoint = endpoint.URL + "/v1"
	cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"local": server}
	require.NoError(t, runEmbeddingsBuildDirect(t.Context(), io.Discard, cfg, vector.BuildRequest{}))
	ix, err = vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), true, 1000)
	require.NoError(t, err)
	defer ix.Close()
	active, ok, err := ix.ActiveFingerprint(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, g.Fingerprint(), active)
	missing, err := ix.MissingEmbeddedDocs(t.Context(), active, nil)
	require.NoError(t, err)
	assert.Zero(t, missing)
}

// The build request overrides config for one completed generation. A later push
// validates that snapshot's scope while the next planned build still uses config.
func TestRemoteProducerExportsExplicitBuildScope(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		configured, requested bool
		wantSessions          []string
	}{
		{"include automated", false, true, []string{"local", "auto"}},
		{"exclude automated", true, false, []string{"local"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"manager", "direct"} {
				t.Run(mode, func(t *testing.T) {
					ctx := t.Context()
					cfg, database, _ := hubProducerScopeFixture(t, tc.requested)
					require.NoError(t, database.SetSessionDataVersionsContext(ctx, []string{"local", "foreign", "other~mirror", "auto"}, db.CurrentDataVersion()))
					cfg.Vector.IncludeAutomated = tc.configured
					req := vector.BuildRequest{IncludeAutomated: tc.requested}
					if mode == "manager" {
						ix, err := vector.Open(ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false, cfg.Vector.Embeddings.MaxInputChars)
						require.NoError(t, err)
						m := embeddingManager(ix, database, vector.EncoderSet{}, cfg, vector.MessageIndexSpec().Name)
						ran, err := m.TryBuild(ctx, req)
						m.Shutdown()
						require.NoError(t, ix.Close())
						require.NoError(t, err)
						require.True(t, ran)
					} else {
						require.NoError(t, runEmbeddingsBuildDirect(ctx, io.Discard, cfg, req))
					}
					ix, err := vector.Open(ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), true, cfg.Vector.Embeddings.MaxInputChars)
					require.NoError(t, err)
					raw, ok, err := ix.BeginExport(ctx, nil)
					require.NoError(t, err)
					require.True(t, ok)
					require.NotNil(t, raw.Generation().IncludeAutomated)
					assert.Equal(t, tc.requested, *raw.Generation().IncludeAutomated)
					assert.Equal(t, db.CurrentDataVersion(), raw.Generation().DataVersion)
					want := vectorGeneration(publishedRecipeConfig("hub-model"))
					assert.Equal(t, want.Fingerprint(), raw.Generation().Fingerprint)
					hashes, err := raw.SessionDocHashes(ctx, nil)
					require.NoError(t, err)
					require.Len(t, hashes, len(tc.wantSessions))
					for _, session := range tc.wantSessions {
						assert.Contains(t, hashes, session)
					}
					require.NoError(t, raw.Close())
					require.NoError(t, ix.Close())

					src := newVectorPushSource(cfg).(*vectorPushSource)
					defer src.Close()
					// Planning a future configured build must still reject this hub scope.
					_, err = src.plannedGeneration(ctx)
					require.ErrorContains(t, err, "automated scope mismatch")
					// Exercise the ordinary source first, then the --embed wrapper.
					for _, source := range []storage.VectorPushSource{src, &replicaEmbedder{source: src}} {
						exported, ok, err := source.BeginExport(ctx, nil)
						require.NoError(t, err)
						require.True(t, ok)
						got := exported.Generation()
						assert.Equal(t, want.Fingerprint(), got.Fingerprint)
						assert.Equal(t, want.Params, got.Params)
						require.NotNil(t, got.IncludeAutomated)
						assert.Equal(t, tc.requested, *got.IncludeAutomated)
						assert.Equal(t, db.CurrentDataVersion(), got.DataVersion)
						for _, session := range tc.wantSessions {
							docs, hash, err := exported.SessionDocs(ctx, session)
							require.NoError(t, err)
							assert.Equal(t, hashes[session], hash)
							require.Len(t, docs, 1)
							require.Len(t, docs[0].Chunks, 1)
							assert.Equal(t, []float32{1, 0, 0, 0}, docs[0].Chunks[0].Embedding)
						}
						require.NoError(t, exported.Close())
					}
					_, err = src.plannedGeneration(ctx)
					require.ErrorContains(t, err, "automated scope mismatch", "export must not mutate the configured next build")
				})
			}
		})
	}
}
