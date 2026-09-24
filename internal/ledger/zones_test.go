package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSortZoneEvents(t *testing.T) {
	ev := []Event{{}}
	got := SortZoneEvents([]ZoneEvents{
		{Zone: "zeta", Events: ev},
		{Zone: "ops", Events: ev},
		{Zone: "empty"},
		{Zone: "alpha", Events: ev},
		{Zone: "default", Events: ev},
	}, []string{"default", "ops", "empty"})
	var zones []string
	for _, z := range got {
		zones = append(zones, z.Zone)
	}
	assert.Equal(t, []string{"default", "ops", "alpha", "zeta"}, zones,
		"configured zones in config order, then the rest by name; empty zones omitted")
}

func TestSortZones(t *testing.T) {
	assert.Equal(t, []string{"default", "ops", "hub-only"},
		SortZones([]string{"hub-only", "ops"}, []string{"default", "ops"}))
	assert.Equal(t, []string{"a", "b"}, SortZones([]string{"b", "a", "b"}, nil))
}
