package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVectorOwnerIdentityOwns(t *testing.T) {
	owner := vectorOwnerIdentity{
		markerID:       "marker-a",
		machine:        "laptop",
		legacyMachines: []string{"old-laptop"},
	}
	tests := []struct {
		name        string
		ownerMarker string
		machine     string
		want        bool
	}{
		{name: "matching marker", ownerMarker: "marker-a", machine: "other", want: true},
		{name: "foreign marker", ownerMarker: "marker-b", machine: "laptop", want: false},
		{name: "legacy row empty machine", machine: "", want: true},
		{name: "legacy row local machine", machine: "local", want: true},
		{name: "legacy row own machine", machine: "laptop", want: true},
		{name: "legacy row alias machine", machine: "old-laptop", want: true},
		{name: "legacy row foreign machine", machine: "other-machine", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, owner.owns(tt.ownerMarker, tt.machine))
		})
	}
}

// notReadyVectorSource reports a not-ready local index, as the vectors.db
// adapter does while an embeddings build is rewriting the active generation.
type notReadyVectorSource struct{}

func (notReadyVectorSource) Generation(
	context.Context,
) (VectorGenerationInfo, bool, error) {
	return VectorGenerationInfo{}, false, fmt.Errorf(
		"%w: 7 document(s) pending", ErrVectorSourceNotReady)
}

func (notReadyVectorSource) SessionDocHashes(
	context.Context, []string,
) (map[string]string, error) {
	return nil, nil
}

func (notReadyVectorSource) SessionDocs(
	context.Context, string,
) ([]VectorPushDoc, string, error) {
	return nil, "", nil
}

// TestPushVectorsSkipsWhenSourceNotReady pins that a not-ready source turns
// the vector phase into a clean skip — not an error, and not a push over a
// partial local view that would evict valid PG vectors.
func TestPushVectorsSkipsWhenSourceNotReady(t *testing.T) {
	sync := &Sync{vectorSource: notReadyVectorSource{}}

	res, err := sync.pushVectors(context.Background(), false, nil, nil, nil)

	require.NoError(t, err)
	assert.True(t, res.Skipped)
	assert.Contains(t, res.SkippedReason, "not fully embedded")
	assert.Contains(t, res.SkippedReason, "7 document(s) pending")
}

// spyVectorSource fails the test on any call: an empty change scope must
// finish the vector phase without touching the source (or PG) at all.
type spyVectorSource struct{ t *testing.T }

func (s spyVectorSource) Generation(
	context.Context,
) (VectorGenerationInfo, bool, error) {
	s.t.Fatal("Generation must not be called for an empty scope")
	return VectorGenerationInfo{}, false, nil
}

func (s spyVectorSource) SessionDocHashes(
	context.Context, []string,
) (map[string]string, error) {
	s.t.Fatal("SessionDocHashes must not be called for an empty scope")
	return nil, nil
}

func (s spyVectorSource) SessionDocs(
	context.Context, string,
) ([]VectorPushDoc, string, error) {
	s.t.Fatal("SessionDocs must not be called for an empty scope")
	return nil, "", nil
}

// TestPushVectorsEmptyScopeReadsNothing pins the change-scoped contract for
// pushes with no changed sessions: no source reads, no PG reads, no skip
// marker (an empty scope is a successful no-op, not a degraded phase, so the
// watch loop keeps scoping subsequent change pushes).
func TestPushVectorsEmptyScopeReadsNothing(t *testing.T) {
	sync := &Sync{vectorSource: spyVectorSource{t: t}}

	res, err := sync.pushVectors(
		context.Background(), false, []string{}, nil, nil,
	)

	require.NoError(t, err)
	assert.False(t, res.Skipped)
	assert.Zero(t, res.SessionsPushed)
	assert.Zero(t, res.SessionsEvicted)
}
