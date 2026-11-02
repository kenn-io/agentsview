package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGFilterWrappersPreserveParameterOffset(t *testing.T) {
	filter := db.AnalyticsFilter{From: "2024-06-01", To: "2024-06-03"}
	from, to := analyticsUTCRange(filter)
	for _, tc := range []struct {
		name     string
		build    func(*paramBuilder) string
		wantArgs []any
	}{
		{"usage", func(pb *paramBuilder) string {
			return appendPGUsageSessionFilterClauses("WHERE true", pb, db.UsageFilter{
				ProjectLabels: []string{"team,core"}, ExcludeProjectLabels: []string{"other,group"},
			}, db.PostgresQueryDialect())
		}, []any{"seed", "team,core", "other,group"}},
		{"analytics dates", func(pb *paramBuilder) string {
			return buildAnalyticsWhereWithDate(filter, pgDateColS, pb, true, "s.id", db.PostgresQueryDialect())
		}, []any{"seed", from, to}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pb := &paramBuilder{args: []any{"seed"}, n: 1}
			sql := tc.build(pb)
			assert.Contains(t, sql, "$2")
			assert.Contains(t, sql, "$3")
			assert.NotContains(t, sql, "$1")
			assert.Equal(t, tc.wantArgs, pb.args)
			assert.Equal(t, 3, pb.n)
		})
	}
}
