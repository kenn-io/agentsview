package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGUsageFilterPreservesParameterOffset(t *testing.T) {
	pb := &paramBuilder{args: []any{"seed"}, n: 1}
	where := appendPGUsageSessionFilterClauses("WHERE true", pb, db.UsageFilter{
		ProjectLabels:        []string{"team,core"},
		ExcludeProjectLabels: []string{"other,group"},
	})
	assert.Contains(t, where, "s.project = $2")
	assert.Contains(t, where, "s.project != $3")
	assert.Equal(t, []any{"seed", "team,core", "other,group"}, pb.args)
	assert.Equal(t, 3, pb.n)
}
