package db

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corerecall "go.kenn.io/agentsview/internal/recall"
)

func TestFlattenTrajectoryText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "object keys sorted, arrays in order, non-strings skipped",
			in:   `{"b":"second","a":"first","arr":["x","y"],"n":42,"ok":true,"blank":"  ","obj":{"z":"deep"}}`,
			want: "first\nx\ny\nsecond\ndeep",
		},
		{name: "bare string", in: `"hello world"`, want: "hello world"},
		{name: "array root", in: `["a","b"]`, want: "a\nb"},
		{name: "no strings", in: `{"n":1,"b":true,"z":null}`, want: ""},
		{
			name: "nested objects inside arrays",
			in:   `{"msgs":[{"role":"user","text":"q"},{"role":"asst","text":"a"}]}`,
			want: "user\nq\nasst\na",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := flattenTrajectoryText(json.RawMessage(tt.in))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFlattenTrajectoryTextInvalidJSON(t *testing.T) {
	_, err := flattenTrajectoryText(json.RawMessage(`{not json`))
	require.Error(t, err)
}

func TestFlattenTrajectoryTextDeterministic(t *testing.T) {
	in := json.RawMessage(`{"c":"3","a":"1","b":"2"}`)
	first, err := flattenTrajectoryText(in)
	require.NoError(t, err)
	assert.Equal(t, "1\n2\n3", first)
	for range 5 {
		again, err := flattenTrajectoryText(in)
		require.NoError(t, err)
		assert.Equal(t, first, again)
	}
}

func TestChunkText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		size int
		want []string
	}{
		{"splits with partial last", "abcdefg", 3, []string{"abc", "def", "g"}},
		{"exact multiple", "abcdef", 3, []string{"abc", "def"}},
		{"single chunk", "abc", 10, []string{"abc"}},
		{"empty", "", 3, nil},
		{"non-positive size falls back to default", "abc", 0, []string{"abc"}},
		{"multibyte runes are not split", "a√bc", 2, []string{"a√", "bc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, chunkText(tt.in, tt.size))
		})
	}
}

func TestEvalIngestIDDeterministicAndCollisionSafe(t *testing.T) {
	a, err := evalIngestID("eval-trajectory", "run1", "traj1", 0)
	require.NoError(t, err)
	b, err := evalIngestID("eval-trajectory", "run1", "traj1", 0)
	require.NoError(t, err)
	assert.Equal(t, a, b, "same parts must produce the same id")
	assert.Len(t, a, 64, "sha-256 hex is 64 chars")

	other, err := evalIngestID("eval-trajectory", "run1", "traj1", 1)
	require.NoError(t, err)
	assert.NotEqual(t, a, other, "a different chunk index must produce a different id")

	// Raw concatenation of ("a","b:c") and ("a:b","c") would collide; the
	// JSON-tuple hash must not.
	x, err := evalIngestID("a", "b:c")
	require.NoError(t, err)
	y, err := evalIngestID("a:b", "c")
	require.NoError(t, err)
	assert.NotEqual(t, x, y, "delimiter-ambiguous tuples must not collide")
}

func TestIngestEvalTrajectory(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Flattened text spans three 2000-rune chunks (4050 runes total).
	long := strings.Repeat("x", defaultEvalChunkChars*2+50)
	in := EvalTrajectoryIngest{
		RunID:           "run1",
		TrajectoryID:    "traj1",
		Trajectory:      json.RawMessage(`{"text":"` + long + `"}`),
		ExtractorMethod: "eval-harness-raw-trajectory",
		SourceVersion:   "test-harness-v1",
	}

	res, err := d.IngestEvalTrajectory(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, "run1", res.RunID)
	assert.Equal(t, "traj1", res.TrajectoryID)
	assert.Equal(t, 3, res.EntriesIndexed)

	id0, err := evalTrajectoryChunkID(
		in, evalTrajectoryContentDigest(long), 0,
	)
	require.NoError(t, err)
	m0, err := d.GetRecallEntry(ctx, id0)
	require.NoError(t, err)
	require.NotNil(t, m0)
	assert.Equal(t, corerecall.TypeFact, m0.Type)
	assert.Equal(t, corerecall.ScopeRepository, m0.Scope)
	assert.Equal(t, corerecall.StatusAccepted, m0.Status)
	assert.Equal(t, corerecall.ReviewStateEvalRaw, m0.ReviewState)
	assert.Equal(t, in.ExtractorMethod, m0.ExtractorMethod)
	assert.Equal(t, "run1", m0.SourceRunID)
	assert.Equal(t, "traj1:chunk:0", m0.SourceEpisodeID)
	assert.True(t, m0.Transferable)
	assert.True(t, m0.ProvenanceOK)
	assert.Nil(t, m0.Confidence, "raw chunks carry no confidence")
	assert.Equal(t, defaultEvalChunkChars, len([]rune(m0.Body)))
	assert.Equal(t, defaultEvalTrajectoryProject, m0.Project)
	assert.Equal(t, defaultEvalTrajectoryAgent, m0.Agent)

	sess, err := d.GetSession(ctx, m0.SourceSessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, in.SourceVersion, sess.SourceVersion)

	res2, err := d.IngestEvalTrajectory(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, 0, res2.EntriesIndexed, "re-ingest must insert nothing new")
}

func TestIngestEvalTrajectoryConcurrentReingestIsIdempotent(t *testing.T) {
	d := testDB(t)
	in := EvalTrajectoryIngest{
		RunID:           "run-concurrent",
		TrajectoryID:    "traj-concurrent",
		Trajectory:      json.RawMessage(`{"text":"` + strings.Repeat("x", defaultEvalChunkChars*2+50) + `"}`),
		ExtractorMethod: "eval-harness-raw-trajectory",
		SourceVersion:   "test-harness-v1",
	}
	const workers = 16
	start := make(chan struct{})
	type outcome struct {
		result EvalTrajectoryIngestResult
		err    error
	}
	outcomes := make(chan outcome, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for range workers {
		go func() {
			ready.Done()
			<-start
			result, err := d.IngestEvalTrajectory(context.Background(), in)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	ready.Wait()
	close(start)

	totalIndexed := 0
	for range workers {
		outcome := <-outcomes
		require.NoError(t, outcome.err)
		totalIndexed += outcome.result.EntriesIndexed
	}
	assert.Equal(t, 3, totalIndexed)
	entries, err := d.ListRecallEntries(context.Background(), RecallQuery{
		SourceRunID: in.RunID,
		Status:      corerecall.StatusAccepted,
		Limit:       10,
	})
	require.NoError(t, err)
	assert.Len(t, entries, 3)
}

func TestIngestEvalTrajectoryVersionsIdentityByExtractorMetadataAndContent(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	base := EvalTrajectoryIngest{
		RunID:           "run1",
		TrajectoryID:    "traj1",
		Trajectory:      json.RawMessage(`{"text":"first trajectory"}`),
		ExtractorMethod: "extractor-v1",
		SourceVersion:   "harness-v1",
	}

	first, err := d.IngestEvalTrajectory(ctx, base)
	require.NoError(t, err)
	assert.Equal(t, 1, first.EntriesIndexed)

	changedExtractor := base
	changedExtractor.ExtractorMethod = "extractor-v2"
	second, err := d.IngestEvalTrajectory(ctx, changedExtractor)
	require.NoError(t, err)
	assert.Equal(t, 1, second.EntriesIndexed)

	changedSourceVersion := base
	changedSourceVersion.SourceVersion = "harness-v2"
	third, err := d.IngestEvalTrajectory(ctx, changedSourceVersion)
	require.NoError(t, err)
	assert.Equal(t, 1, third.EntriesIndexed)

	changedContent := base
	changedContent.Trajectory = json.RawMessage(`{"text":"revised trajectory"}`)
	fourth, err := d.IngestEvalTrajectory(ctx, changedContent)
	require.NoError(t, err)
	assert.Equal(t, 1, fourth.EntriesIndexed)

	entries, err := d.ListRecallEntries(ctx, RecallQuery{
		SourceRunID: "run1",
		Status:      corerecall.StatusAccepted,
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, entries, 4)
	assert.ElementsMatch(t, []string{
		"first trajectory",
		"first trajectory",
		"first trajectory",
		"revised trajectory",
	}, []string{entries[0].Body, entries[1].Body, entries[2].Body, entries[3].Body})
}

func TestIngestEvalTrajectoryNoStringsIndexesNothing(t *testing.T) {
	d := testDB(t)
	res, err := d.IngestEvalTrajectory(context.Background(), EvalTrajectoryIngest{
		RunID:           "run1",
		TrajectoryID:    "traj-empty",
		Trajectory:      json.RawMessage(`{"n":1,"ok":true}`),
		ExtractorMethod: "eval-harness-raw-trajectory",
		SourceVersion:   "test-harness-v1",
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.EntriesIndexed)
}

func TestIngestEvalTrajectoryRequiresIDs(t *testing.T) {
	d := testDB(t)
	_, err := d.IngestEvalTrajectory(context.Background(), EvalTrajectoryIngest{
		TrajectoryID:    "traj1",
		Trajectory:      json.RawMessage(`{"text":"hi"}`),
		ExtractorMethod: "eval-harness-raw-trajectory",
		SourceVersion:   "test-harness-v1",
	})
	require.Error(t, err)
}

func TestIngestEvalTrajectoryRequiresExtractorMethodAndSourceVersion(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	base := EvalTrajectoryIngest{
		RunID:        "run1",
		TrajectoryID: "traj1",
		Trajectory:   json.RawMessage(`{"text":"hi"}`),
	}

	missingExtractor := base
	missingExtractor.SourceVersion = "test-harness-v1"
	_, err := d.IngestEvalTrajectory(ctx, missingExtractor)
	require.ErrorContains(t, err, "extractor_method is required")

	missingSourceVersion := base
	missingSourceVersion.ExtractorMethod = "eval-harness-raw-trajectory"
	_, err = d.IngestEvalTrajectory(ctx, missingSourceVersion)
	require.ErrorContains(t, err, "source_version is required")

	tooLong := base
	tooLong.ExtractorMethod = strings.Repeat("x", maxEvalFieldRunes+1)
	tooLong.SourceVersion = "test-harness-v1"
	_, err = d.IngestEvalTrajectory(ctx, tooLong)
	require.ErrorContains(t, err, "extractor_method exceeds maximum length")
}
