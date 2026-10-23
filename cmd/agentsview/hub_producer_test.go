package main

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
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
	"hegel.dev/go/hegel"
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
		require.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		if r.Method == http.MethodGet {
			if len(status) > 0 {
				calls.Add(1)
				w.WriteHeader(status[0])
				return
			}
			require.NoError(t, json.MarshalWrite(w, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()}))
			return
		}
		calls.Add(1)
		var req vector.HubLookupRequest
		require.NoError(t, json.UnmarshalRead(r.Body, &req))
		require.Equal(t, g.Fingerprint(), req.Fingerprint)
		require.Len(t, req.Identities, 1)
		require.Equal(t, "node~local", req.Identities[0].SessionID)
		v, err := vector.EncodeHubVector([]float32{1, 0, 0, 0})
		require.NoError(t, err)
		require.NoError(t, json.MarshalWrite(w, vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: g.Fingerprint(), Chunks: []vector.HubChunk{{Index: 0, ChunkCount: 1, Vector: v}}}))
	}))
	t.Cleanup(server.Close)
	cfg.Remote.URL = server.URL
	return cfg, database, calls
}

func TestRemoteProducerFactoryRoutesEveryBuildMode(t *testing.T) {
	for _, mode := range []string{"ordinary", "full", "backstop", "repair", "async"} {
		t.Run(mode, func(t *testing.T) {
			cfg, database, calls := hubProducerFixture(t)
			ix, err := vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
			require.NoError(t, err)
			defer ix.Close()
			if mode == "full" || mode == "repair" {
				_, err := hubProducerBuild(ix, database, cfg)(t.Context(), vector.BuildRequest{}, nil)
				require.NoError(t, err)
				if mode == "repair" {
					raw, err := sql.Open(vectorTestDriverName, cfg.Vector.ResolvedDBPath(cfg.DataDir))
					require.NoError(t, err)
					_, err = raw.ExecContext(t.Context(), "UPDATE message_vectors_v1 SET embedding=zeroblob(16)")
					require.NoError(t, err)
					require.NoError(t, raw.Close())
				}
				calls.Store(0)
			}
			var encoded atomic.Int32
			encoders := vector.EncoderSet{Default: "local", ByName: map[string]vector.ManagedEncoder{"local": {Encode: func(context.Context, []string) ([][]float32, error) {
				encoded.Add(1)
				t.Error("remote producer encoded a document")
				return nil, nil
			}}}}
			m := embeddingManager(ix, database, encoders, cfg, vector.MessageIndexSpec().Name)
			defer m.Shutdown()
			req := vector.BuildRequest{FullRebuild: mode == "full", Backstop: mode == "backstop", RepairInvalid: mode == "repair"}
			if mode == "async" {
				require.NoError(t, m.StartBuild(req))
				m.Wait()
				require.Empty(t, m.Status().LastError)
			} else {
				ran, err := m.TryBuild(t.Context(), req)
				require.NoError(t, err)
				require.True(t, ran)
			}
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
		})
	}
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
			t.Setenv("AGENTSVIEW_SERVER_TOKEN", "token")
			if mode == "environment" {
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
	hegel.Test(t, func(ht *hegel.T) {
		c := publishedRecipeConfig(fmt.Sprintf("model-%d", hegel.Draw(ht, hegel.Integers[int](0, 1000000))))
		c.Dimension = hegel.Draw(ht, hegel.Integers[int](1, 4096))
		c.MaxInputChars = hegel.Draw(ht, hegel.Integers[int](1, 32768))
		g := vectorGeneration(c)
		cfg := config.Config{Vector: config.VectorConfig{Embeddings: config.VectorEmbeddingsConfig{MaxInputChars: 8192, DefaultServer: "query", Servers: map[string]config.VectorEmbeddingsServerConfig{"query": {Endpoint: "http://localhost:8080/v1"}}}}}
		got, err := decodeHubRecipe(cfg, vector.HubGeneration{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params, DataVersion: db.CurrentDataVersion()})
		require.NoError(ht, err)
		require.Equal(ht, g.Fingerprint(), vectorGeneration(got).Fingerprint())
		require.Equal(ht, cfg.Vector.Embeddings.Servers, got.Servers)
		require.Equal(ht, "query", got.DefaultServer)
	}, hegel.WithTestCases(1000))
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
