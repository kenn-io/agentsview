package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestSortKeysMatchHumaEnum guards the hard-coded order_by enum tag against
// drift from the shared sort registry.
func TestSortKeysMatchHumaEnum(t *testing.T) {
	field, ok := reflect.TypeFor[sessionFilterInput]().FieldByName("OrderBy")
	require.True(t, ok, "sessionFilterInput.OrderBy field")
	enum := field.Tag.Get("enum")
	require.NotEmpty(t, enum, "order_by enum tag")
	assert.Equal(t, db.SortKeys(), strings.Split(enum, ","),
		"order_by enum must match db.SortKeys()")
}
