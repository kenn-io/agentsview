//go:build evalingest

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// evalQueryResponse mirrors the fields of the (unexported) recall query
// response that these tests assert on.
type evalQueryResponse struct {
	RecallEntries []db.RecallResult `json:"entries"`
	TrustedOnly   bool              `json:"trusted_only"`
	Context       string            `json:"context"`
}

type evalIngestResponse struct {
	RunID          string `json:"run_id"`
	TrajectoryID   string `json:"trajectory_id"`
	CorpusID       string `json:"corpus_id"`
	EntriesIndexed int    `json:"entries_indexed"`
}

func TestIngestEvalTrajectoryEndToEnd(t *testing.T) {
	te := setup(t)

	w := te.post(t, "/api/v1/recall/eval/trajectories", `
{"run_id":"run-a","trajectory_id":"traj-a","extractor_method":"eval-harness-raw-trajectory","source_version":"test-harness-v1","trajectory":{"question":"where did zaphod hide the towel","answer":"in the bathroom locker"}}
`)
	assertStatus(t, w, http.StatusOK)
	got := decode[db.EvalTrajectoryIngestResult](t, w)
	assert.Equal(t, "run-a", got.RunID)
	assert.Equal(t, "traj-a", got.TrajectoryID)
	assert.Equal(t, 1, got.EntriesIndexed)

	// Retrieve through the query endpoint, scoped by run + extractor method.
	q := te.post(t, "/api/v1/recall/query", `
{"query":"zaphod towel","source_run_id":"run-a","extractor_method":"eval-harness-raw-trajectory","trusted_only":false,"limit":10,"include_context":true}
`)
	assertStatus(t, q, http.StatusOK)
	resp := decode[evalQueryResponse](t, q)
	assert.False(t, resp.TrustedOnly)
	require.NotEmpty(t, resp.RecallEntries, "ingested chunk should be retrievable")
	m := resp.RecallEntries[0]
	assert.Equal(t, "fact", m.Type)
	assert.Equal(t, "eval-harness-raw-trajectory", m.ExtractorMethod)
	assert.Equal(t, "run-a", m.SourceRunID)
	assert.Contains(t, m.SourceEpisodeID, ":chunk:")
	assert.True(t, m.Transferable)
	assert.True(t, m.ProvenanceOK)
	assert.Contains(t, resp.Context, "towel")

	// Raw eval rows are deliberately quarantined from trusted recall even when
	// transferability and provenance are otherwise true.
	trusted := te.post(t, "/api/v1/recall/query", `
{"query":"zaphod towel","source_run_id":"run-a","extractor_method":"eval-harness-raw-trajectory","trusted_only":true,"limit":10,"include_context":true}
`)
	assertStatus(t, trusted, http.StatusOK)
	trustedResp := decode[evalQueryResponse](t, trusted)
	assert.True(t, trustedResp.TrustedOnly)
	assert.Empty(t, trustedResp.RecallEntries)

	// The placeholder session exists, marked as an eval session.
	sess, err := te.db.GetSession(context.Background(), m.SourceSessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "test-harness-v1", sess.SourceVersion)
}

func TestIngestEvalTrajectoryIdempotent(t *testing.T) {
	te := setup(t)
	body := `
{"run_id":"run-b","trajectory_id":"traj-b","extractor_method":"eval-harness-raw-trajectory","source_version":"test-harness-v1","trajectory":{"text":"hello there general"}}
`
	w1 := te.post(t, "/api/v1/recall/eval/trajectories", body)
	assertStatus(t, w1, http.StatusOK)
	assert.Equal(t, 1, decode[db.EvalTrajectoryIngestResult](t, w1).EntriesIndexed)

	w2 := te.post(t, "/api/v1/recall/eval/trajectories", body)
	assertStatus(t, w2, http.StatusOK)
	assert.Equal(t, 0, decode[db.EvalTrajectoryIngestResult](t, w2).EntriesIndexed)
}

func TestIngestEvalTrajectoryVersionsExposeQueryableCorpusID(t *testing.T) {
	te := setup(t)
	first := decode[evalIngestResponse](t, te.post(
		t,
		"/api/v1/recall/eval/trajectories",
		`{"run_id":"run-versioned","trajectory_id":"traj-versioned","extractor_method":"eval-harness-raw-trajectory","source_version":"harness-v1","trajectory":{"text":"alpha memory"}}`,
	))
	require.NotEmpty(t, first.CorpusID)
	assert.Equal(t, 1, first.EntriesIndexed)

	second := decode[evalIngestResponse](t, te.post(
		t,
		"/api/v1/recall/eval/trajectories",
		`{"run_id":"run-versioned","trajectory_id":"traj-versioned","extractor_method":"eval-harness-raw-trajectory","source_version":"harness-v2","trajectory":{"text":"beta memory"}}`,
	))
	require.NotEmpty(t, second.CorpusID)
	assert.NotEqual(t, first.CorpusID, second.CorpusID)
	assert.Equal(t, 1, second.EntriesIndexed)

	queryCorpus := func(corpusID string) evalQueryResponse {
		body := fmt.Sprintf(
			`{"query":"memory","source_run_id":"run-versioned","extractor_method":"eval-harness-raw-trajectory","source_session_id":"%s","trusted_only":false,"limit":10}`,
			corpusID,
		)
		w := te.post(t, "/api/v1/recall/query", body)
		assertStatus(t, w, http.StatusOK)
		return decode[evalQueryResponse](t, w)
	}
	firstCorpus := queryCorpus(first.CorpusID)
	require.Len(t, firstCorpus.RecallEntries, 1)
	assert.Equal(t, "alpha memory", firstCorpus.RecallEntries[0].Body)
	secondCorpus := queryCorpus(second.CorpusID)
	require.Len(t, secondCorpus.RecallEntries, 1)
	assert.Equal(t, "beta memory", secondCorpus.RecallEntries[0].Body)
}

func TestIngestEvalTrajectoryRefusesDefaultDataDir(t *testing.T) {
	home := t.TempDir()
	defaultDataDir := filepath.Join(home, ".agentsview")
	te := setup(t, func(c *config.Config) {
		c.DataDir = defaultDataDir
		c.DBPath = filepath.Join(defaultDataDir, "test.db")
	})

	w := te.post(t, "/api/v1/recall/eval/trajectories", `
{"run_id":"run-c","trajectory_id":"traj-c","extractor_method":"eval-harness-raw-trajectory","source_version":"test-harness-v1","trajectory":{"text":"hi"}}
`)
	assertStatus(t, w, http.StatusForbidden)
	assertBodyContains(t, w, "default agentsview data directory")
	assertBodyContains(t, w, "allow_production_import")
}

func TestIngestEvalTrajectoryAllowsDefaultDataDirWithOverride(t *testing.T) {
	home := t.TempDir()
	defaultDataDir := filepath.Join(home, ".agentsview")
	te := setup(t, func(c *config.Config) {
		c.DataDir = defaultDataDir
		c.DBPath = filepath.Join(defaultDataDir, "test.db")
	})

	w := te.post(t, "/api/v1/recall/eval/trajectories?allow_production_import=true", `
{"run_id":"run-c","trajectory_id":"traj-c","extractor_method":"eval-harness-raw-trajectory","source_version":"test-harness-v1","trajectory":{"text":"hi"}}
`)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, 1, decode[db.EvalTrajectoryIngestResult](t, w).EntriesIndexed)
}

func TestIngestEvalTrajectoryEmptyObjectIndexesNothing(t *testing.T) {
	te := setup(t)
	w := te.post(t, "/api/v1/recall/eval/trajectories",
		`{"run_id":"r","trajectory_id":"t","extractor_method":"eval-harness-raw-trajectory","source_version":"test-harness-v1","trajectory":{"n":1}}`)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, 0, decode[db.EvalTrajectoryIngestResult](t, w).EntriesIndexed)
}

func TestIngestEvalTrajectoryValidation(t *testing.T) {
	te := setup(t)
	const path = "/api/v1/recall/eval/trajectories"
	const validFields = `"extractor_method":"eval-harness-raw-trajectory","source_version":"test-harness-v1"`
	tooLong := strings.Repeat("x", 201)
	cases := []struct {
		name string
		path string
		body string
		code int
	}{
		{"missing run_id", path, `{"trajectory_id":"t",` + validFields + `,"trajectory":{"text":"x"}}`, http.StatusBadRequest},
		{"missing trajectory_id", path, `{"run_id":"r",` + validFields + `,"trajectory":{"text":"x"}}`, http.StatusBadRequest},
		{"missing extractor_method", path, `{"run_id":"r","trajectory_id":"t","source_version":"test-harness-v1","trajectory":{"text":"x"}}`, http.StatusBadRequest},
		{"missing source_version", path, `{"run_id":"r","trajectory_id":"t","extractor_method":"eval-harness-raw-trajectory","trajectory":{"text":"x"}}`, http.StatusBadRequest},
		{"extractor_method too long", path, `{"run_id":"r","trajectory_id":"t","extractor_method":"` + tooLong + `","source_version":"test-harness-v1","trajectory":{"text":"x"}}`, http.StatusBadRequest},
		{"source_version too long", path, `{"run_id":"r","trajectory_id":"t","extractor_method":"eval-harness-raw-trajectory","source_version":"` + tooLong + `","trajectory":{"text":"x"}}`, http.StatusBadRequest},
		{"missing trajectory", path, `{"run_id":"r","trajectory_id":"t",` + validFields + `}`, http.StatusBadRequest},
		{"null trajectory", path, `{"run_id":"r","trajectory_id":"t",` + validFields + `,"trajectory":null}`, http.StatusBadRequest},
		{"invalid json", path, `{not json`, http.StatusBadRequest},
		{"invalid override param", path + "?allow_production_import=1", `{"run_id":"r","trajectory_id":"t",` + validFields + `,"trajectory":{"text":"x"}}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := te.post(t, tc.path, tc.body)
			assertStatus(t, w, tc.code)
		})
	}
}
