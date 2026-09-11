package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/db"
)

type hostedAvailabilitySearcher struct {
	available bool
	err       error
}

func (s hostedAvailabilitySearcher) SemanticAvailable(context.Context) (bool, error) {
	return s.available, s.err
}
func (hostedAvailabilitySearcher) SemanticSearch(context.Context, string, int) ([]db.VectorHit, error) {
	return nil, nil
}
func (hostedAvailabilitySearcher) ResolveMessageUnits(context.Context, []db.MessageRef) ([]db.UnitRef, error) {
	return nil, nil
}

func TestHostedStoreHasSemanticUsesDynamicAvailability(t *testing.T) {
	hosted := &HostedStore{physical: &Store{}}
	hosted.SetVectorSearcher(hostedAvailabilitySearcher{available: false})
	assert.False(t, hosted.HasSemantic())

	hosted.SetVectorSearcher(hostedAvailabilitySearcher{available: true})
	assert.True(t, hosted.HasSemantic())

	hosted.SetVectorSearcher(hostedAvailabilitySearcher{available: true, err: errors.New("catalog unavailable")})
	assert.False(t, hosted.HasSemantic(), "availability errors must not advertise semantic search")
}
