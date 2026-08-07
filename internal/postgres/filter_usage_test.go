package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/db"
)

func TestPGSessionAutomatedScopePredicate(t *testing.T) {
	sql, _ := buildPGSessionFilter(db.SessionFilter{
		AutomatedScope: "automated",
	})
	assert.Contains(t, sql, "is_automated = TRUE")
}
