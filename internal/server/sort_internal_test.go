package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestSortKeysDocumented guards the order_by doc string against drift from the
// shared sort registry: every accepted sort key must be named in the parameter
// documentation so the OpenAPI surface lists the full allow-list (the enum tag
// could no longer express the comma-separated key:dir spec).
func TestSortKeysDocumented(t *testing.T) {
	field, ok := reflect.TypeFor[sessionFilterInput]().FieldByName("OrderBy")
	require.True(t, ok, "sessionFilterInput.OrderBy field")
	doc := field.Tag.Get("doc")
	require.NotEmpty(t, doc, "order_by doc tag")
	for _, key := range db.SortKeys() {
		assert.Contains(t, doc, key, "order_by doc must mention sort key %q", key)
	}
}

// TestSortKeysDocOmitsStaleKeys is the inverse guard: the "Valid keys:" clause
// should not name a key that no longer exists in the registry (a stale entry
// left after a rename).
func TestSortKeysDocOmitsStaleKeys(t *testing.T) {
	field, _ := reflect.TypeFor[sessionFilterInput]().FieldByName("OrderBy")
	doc := field.Tag.Get("doc")
	_, after, found := strings.Cut(doc, "Valid keys:")
	require.True(t, found, "doc should enumerate valid keys")
	valid := make(map[string]bool, len(db.SortKeys()))
	for _, k := range db.SortKeys() {
		valid[k] = true
	}
	for _, tok := range strings.FieldsFunc(after, func(r rune) bool {
		return r == ',' || r == ' ' || r == '.'
	}) {
		assert.True(t, valid[tok], "doc lists unknown sort key %q", tok)
	}
}
