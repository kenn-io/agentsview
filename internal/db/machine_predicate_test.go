package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMachinePredicatePreservesParameterOffset(t *testing.T) {
	var calls int
	var gotID, gotMachine string
	var gotPlaceholders []string
	dialect := PostgresQueryDialect().WithMachinePredicate(func(id, machine string, placeholders []string) string {
		calls++
		gotID, gotMachine, gotPlaceholders = id, machine, placeholders
		return "EXISTS (SELECT 1 WHERE " + machine + " IN (" + strings.Join(placeholders, ",") + "))"
	})
	b := NewQueryBuilder(dialect, 3)
	assert.NotEmpty(t, b.MachinePredicate("s.id", "s.machine", []string{"a", "b"}))
	assert.Equal(t, 1, calls)
	assert.Equal(t, "s.id", gotID)
	assert.Equal(t, "s.machine", gotMachine)
	assert.Equal(t, []string{"$4", "$5"}, gotPlaceholders)
	assert.Equal(t, []any{"a", "b"}, b.Args())

	empty := NewQueryBuilder(dialect, 0)
	assert.Equal(t, "1 = 0", empty.MachinePredicate("s.id", "s.machine", nil))
	assert.Empty(t, empty.Args())
}
