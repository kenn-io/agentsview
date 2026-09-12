package db

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/uptrace/bun"
)

func TestJoinBunSQLFragmentsPreservesBunPlaceholderArguments(t *testing.T) {
	got := JoinBunSQLFragments(" AND ",
		BunSQL("session.project = ?", "agentsview"),
		BunSQL("message.ordinal >= ?", 17),
	)

	assert.Equal(t,
		"session.project = ? AND message.ordinal >= ?", got.SQL)
	assert.Equal(t, []any{"agentsview", 17}, got.Args)
}

func TestBunValueKeepsUserDataOutOfSQL(t *testing.T) {
	value := `quote' ? $1 --`

	got := BunValue(value)

	assert.Equal(t, "?", got.SQL)
	assert.Equal(t, []any{value}, got.Args)
}

func TestBunIdentifierUsesBunQuoting(t *testing.T) {
	identifier := `vectors"; DROP TABLE sessions; --`

	got := BunIdentifier(identifier)

	assert.Equal(t, "?", got.SQL)
	assert.NotContains(t, got.SQL, identifier)
	assert.Equal(t, reflect.TypeFor[bun.Ident](), reflect.TypeOf(got.Args[0]))
	assert.Equal(t, identifier, string(got.Args[0].(bun.Ident)))
}
