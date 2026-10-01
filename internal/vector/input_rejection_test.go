package vector

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
)

// rejectingEndpoint is an OpenAI-compatible endpoint that answers 400 when
// reject returns true for a request's inputs and unit vectors otherwise. It
// records every embedded input.
type rejectingEndpoint struct {
	reject atomic.Pointer[func(inputs []string) bool]
	mu     sync.Mutex
	inputs []string
}

func (e *rejectingEndpoint) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingsRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			return
		}
		if reject := e.reject.Load(); reject != nil && (*reject)(req.Input) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		e.inputs = append(e.inputs, req.Input...)
		e.mu.Unlock()
		writeJSON(t, w, http.StatusOK, vectorsResponse(len(req.Input), []float32{1, 0, 0}))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *rejectingEndpoint) setReject(reject func(inputs []string) bool) {
	e.reject.Store(&reject)
}

func (e *rejectingEndpoint) embedded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.inputs)
}

// TestBuildRequestWide400AbortsAndKeepsActiveGeneration covers an endpoint
// that rejects every request, as a wrong model, route, or request field does.
// The build must abort with every document still pending instead of
// skip-stamping the corpus and activating an empty generation, and once the
// endpoint is fixed the same generation embeds the unchanged documents.
func TestBuildRequestWide400AbortsAndKeepsActiveGeneration(t *testing.T) {
	ix := openTestIndex(t)
	ctx := t.Context()
	src := twoDocSource()
	previous := fakeGeneration("previous-model")
	_, err := ix.Build(ctx, src, fakeBuildEncoder(), previous, BuildOptions{})
	require.NoError(t, err)

	endpoint := &rejectingEndpoint{}
	endpoint.setReject(func([]string) bool { return true })
	enc, err := NewEncoder(testEncoderConfig(endpoint.serve(t).URL+"/v1"), embedconfig.RoleDocument)
	require.NoError(t, err)
	next := fakeGeneration("fake-model")

	result, err := ix.Build(ctx, src, enc, next, BuildOptions{BatchSize: 32})
	require.ErrorIs(t, err, errEndpointRejectsRequests)
	assert.Zero(t, result.Fill.Skipped, "a request-wide 400 must not skip-stamp documents")
	assert.False(t, result.Activated)
	active, _, err := ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	assert.Equal(t, previous.Fingerprint(), active, "the previous generation stays active")

	endpoint.setReject(func([]string) bool { return false })
	result, err = ix.Build(ctx, src, enc, next, BuildOptions{BatchSize: 32})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Fill.Documents, "the corrected endpoint embeds the unchanged documents")
	assert.True(t, result.Activated)
	assert.ElementsMatch(t, []string{"hello", "world"}, endpoint.embedded())
}

// TestBuildSkipsDocumentTheEndpointRejectsThroughKitClient is the real-client
// counterpart: when the endpoint embeds the probe input, a 400 belongs to the
// document, so only that document is skipped and the generation activates.
func TestBuildSkipsDocumentTheEndpointRejectsThroughKitClient(t *testing.T) {
	ix := openTestIndex(t)
	ctx := t.Context()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "", 0, "one"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "", 1, "poison"), endedAt: "2024-01-01T00:00:01Z"},
		{unit: userDoc("s1", "", 2, "three"), endedAt: "2024-01-01T00:00:02Z"},
	}}
	endpoint := &rejectingEndpoint{}
	endpoint.setReject(func(inputs []string) bool {
		return slices.ContainsFunc(inputs, func(input string) bool {
			return strings.Contains(input, "poison")
		})
	})
	enc, err := NewEncoder(testEncoderConfig(endpoint.serve(t).URL+"/v1"), embedconfig.RoleDocument)
	require.NoError(t, err)

	result, err := ix.Build(ctx, src, enc, fakeGeneration("fake-model"), BuildOptions{BatchSize: 32})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fill.Skipped, "only the rejected document is skipped")
	assert.Equal(t, 2, result.Fill.Documents)
	assert.True(t, result.Activated)
}
