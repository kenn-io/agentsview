package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGAutomatedScopePredicates(t *testing.T) {
	tests := []struct {
		name     string
		scope    string
		exclude  bool
		want     string
		notWant  string
		buildSQL func(string, bool) string
	}{
		{
			name:    "analytics human",
			scope:   "human",
			want:    "is_automated = FALSE",
			notWant: "is_automated = TRUE",
			buildSQL: func(scope string, exclude bool) string {
				return buildAnalyticsWhereWithDate(
					db.AnalyticsFilter{
						AutomatedScope:   scope,
						ExcludeAutomated: exclude,
					},
					"created_at",
					&paramBuilder{},
					false,
					"id",
				)
			},
		},
		{
			name:    "analytics all",
			scope:   "all",
			exclude: true,
			notWant: "is_automated = FALSE",
			buildSQL: func(scope string, exclude bool) string {
				return buildAnalyticsWhereWithDate(
					db.AnalyticsFilter{
						AutomatedScope:   scope,
						ExcludeAutomated: exclude,
					},
					"created_at",
					&paramBuilder{},
					false,
					"id",
				)
			},
		},
		{
			name:    "analytics automated",
			scope:   "automated",
			want:    "is_automated = TRUE",
			notWant: "is_automated = FALSE",
			buildSQL: func(scope string, exclude bool) string {
				return buildAnalyticsWhereWithDate(
					db.AnalyticsFilter{
						AutomatedScope:   scope,
						ExcludeAutomated: exclude,
					},
					"created_at",
					&paramBuilder{},
					false,
					"id",
				)
			},
		},
		{
			name:  "sessions automated",
			scope: "automated",
			want:  "is_automated = TRUE",
			buildSQL: func(scope string, exclude bool) string {
				sql, _ := buildPGSessionFilter(db.SessionFilter{
					AutomatedScope:   scope,
					ExcludeAutomated: exclude,
				})
				return sql
			},
		},
		{
			name:  "usage automated",
			scope: "automated",
			want:  "COALESCE(s.is_automated, false) = TRUE",
			buildSQL: func(scope string, exclude bool) string {
				sql := appendPGUsageSessionFilterClauses(
					"WHERE true",
					&paramBuilder{},
					db.UsageFilter{
						AutomatedScope:   scope,
						ExcludeAutomated: exclude,
					},
				)
				return sql
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql := tt.buildSQL(tt.scope, tt.exclude)
			if tt.want != "" {
				assert.Contains(t, sql, tt.want, "SQL missing expected predicate")
			}
			if tt.notWant != "" {
				assert.NotContains(t, sql, tt.notWant, "SQL has unexpected predicate")
			}
		})
	}
}

func TestPGAutomatedScopeOneShotExemption(t *testing.T) {
	sql := buildAnalyticsWhereWithDate(
		db.AnalyticsFilter{
			AutomatedScope: "automated",
			ExcludeOneShot: true,
		},
		"created_at",
		&paramBuilder{},
		false,
		"id",
	)
	want := "(user_message_count > 1 OR is_automated = TRUE)"
	assert.Contains(t, sql, want, "analytics SQL missing one-shot exemption")

	usageSQL := appendPGUsageSessionFilterClauses(
		"WHERE true",
		&paramBuilder{},
		db.UsageFilter{
			AutomatedScope: "automated",
			ExcludeOneShot: true,
		},
	)
	want = "(s.user_message_count > 1 OR COALESCE(s.is_automated, false) = TRUE)"
	assert.Contains(t, usageSQL, want, "usage SQL missing one-shot exemption")
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

func TestPGAnalyticsMachineMultiSelectPredicate(t *testing.T) {
	pb := &paramBuilder{}
	sql := buildAnalyticsWhereWithDate(
		db.AnalyticsFilter{
			Machine: " laptop,server ",
		},
		"created_at",
		pb,
		false,
		"id",
	)

	want := "machine IN ($1,$2)"
	assert.Contains(t, sql, want, "analytics SQL missing machine IN predicate")
	assert.NotContains(t, sql, "machine = ",
		"analytics SQL used literal machine equality")
	require.Len(t, pb.args, 2)
	assert.Equal(t, "laptop", pb.args[0])
	assert.Equal(t, "server", pb.args[1])
}
