package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsHostScopedForm(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"p-1", true},
		{"hostB~p-1", true},
		{"hostA~hostB~p-1", false},
		{"~p-1", false},
		{"hostB~xp-1", false},
		{"codex:p-1", false},
		{"", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, isHostScopedForm(tt.id, "p-1"), "id %q", tt.id)
	}
}

func TestRemoteParentResolverWithoutDatabaseUsesSourceHost(t *testing.T) {
	e := &Engine{idPrefix: "hostA~"}
	r := remoteParentResolver{e: e, stores: e.remoteParentStores(), childID: "hostA~c"}
	got, err := r.resolve(t.Context(), "p-1", storedParserParent)
	assert.NoError(t, err)
	assert.Equal(t, "hostA~p-1", got)
}
