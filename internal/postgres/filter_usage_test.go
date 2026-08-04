package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestPGSessionAndUsageAutomatedScopePredicates(t *testing.T) {
	tests := []struct {
		name     string
		want     string
		buildSQL func() string
	}{
		{
			name: "sessions automated",
			want: "is_automated = TRUE",
			buildSQL: func() string {
				sql, _ := buildPGSessionFilter(db.SessionFilter{
					AutomatedScope: "automated",
				})
				return sql
			},
		},
		{
			name: "usage automated",
			want: "COALESCE(s.is_automated, false) = TRUE",
			buildSQL: func() string {
				return appendPGUsageSessionFilterClauses(
					"WHERE true",
					&paramBuilder{},
					db.UsageFilter{AutomatedScope: "automated"},
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, tt.buildSQL(), tt.want)
		})
	}
}

func TestPGUsageAutomatedScopeOneShotExemption(t *testing.T) {
	sql := appendPGUsageSessionFilterClauses(
		"WHERE true",
		&paramBuilder{},
		db.UsageFilter{
			AutomatedScope: "automated",
			ExcludeOneShot: true,
		},
	)
	assert.Contains(t, sql,
		"(s.user_message_count > 1 OR COALESCE(s.is_automated, false) = TRUE)")
}

func TestPGUsageProjectLabelsPreserveCommas(t *testing.T) {
	pb := &paramBuilder{}
	sql := appendPGUsageSessionFilterClauses(
		"WHERE true",
		pb,
		db.UsageFilter{
			ProjectLabels:        []string{"team,core"},
			ExcludeProjectLabels: []string{"other,group"},
		},
	)

	assert.Contains(t, sql, "s.project = $1")
	assert.Contains(t, sql, "s.project != $2")
	require.Len(t, pb.args, 2)
	assert.Equal(t, "team,core", pb.args[0])
	assert.Equal(t, "other,group", pb.args[1])
}
