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

func TestVectorDocsMatchTranscriptRevision(t *testing.T) {
	tests := []struct {
		name     string
		docs     []VectorPushDoc
		revision string
		want     bool
	}{
		{name: "empty document set", revision: "4", want: true},
		{
			name: "all documents match",
			docs: []VectorPushDoc{
				{TranscriptRevision: "4"},
				{TranscriptRevision: "4"},
			},
			revision: "4",
			want:     true,
		},
		{
			name: "one document is newer",
			docs: []VectorPushDoc{
				{TranscriptRevision: "4"},
				{TranscriptRevision: "5"},
			},
			revision: "4",
			want:     false,
		},
		{
			name:     "unstamped document fails closed",
			docs:     []VectorPushDoc{{}},
			revision: "0",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				vectorDocsMatchTranscriptRevision(tt.docs, tt.revision))
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
	context.Context,
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

	res, err := sync.pushVectors(context.Background(), false, nil, nil)

	require.NoError(t, err)
	assert.True(t, res.Skipped)
	assert.Contains(t, res.SkippedReason, "not fully embedded")
	assert.Contains(t, res.SkippedReason, "7 document(s) pending")
}
