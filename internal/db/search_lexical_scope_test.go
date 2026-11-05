package db_test

import (
	"testing"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestLexicalScope(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedLexicalScope(t, d)
	dbtest.AssertLexicalScope(t, d)
}
