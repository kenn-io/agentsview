package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestSortKeysMatchDoc guards the order_by doc string against drift from the
// shared sort registry. It parses the "Valid keys:" clause into exact tokens and
// requires them to equal db.SortKeys() in order, so a key omitted from the clause
// cannot be masked by the example text earlier in the doc, and a stale key left
// after a rename is caught too. This replaces the enum-sync guard that the dropped
// order_by enum tag used to provide.
func TestSortKeysMatchDoc(t *testing.T) {
	field, ok := reflect.TypeFor[sessionFilterInput]().FieldByName("OrderBy")
	require.True(t, ok, "sessionFilterInput.OrderBy field")
	doc := field.Tag.Get("doc")
	_, after, found := strings.Cut(doc, "Valid keys:")
	require.True(t, found, "order_by doc must enumerate keys after 'Valid keys:'")

	keys := strings.FieldsFunc(after, func(r rune) bool {
		return r == ',' || r == ' ' || r == '.'
	})
	assert.Equal(t, db.SortKeys(), keys,
		"order_by 'Valid keys:' clause must list exactly db.SortKeys() in order")
}
