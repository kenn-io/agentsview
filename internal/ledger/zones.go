package ledger

import (
	"cmp"
	"slices"
)

// SortZoneEvents orders query results the way jilog query prints them
// (query.rs:106-149): zones in configured order first, then any other
// zone by name, and zones without events dropped.
func SortZoneEvents(results []ZoneEvents, configured []string) []ZoneEvents {
	rank := make(map[string]int, len(configured))
	for i, z := range configured {
		if _, ok := rank[z]; !ok {
			rank[z] = i
		}
	}
	out := make([]ZoneEvents, 0, len(results))
	for _, r := range results {
		if len(r.Events) > 0 {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b ZoneEvents) int {
		ra, aok := rank[a.Zone]
		rb, bok := rank[b.Zone]
		switch {
		case aok && bok:
			return cmp.Compare(ra, rb)
		case aok:
			return -1
		case bok:
			return 1
		}
		return cmp.Compare(a.Zone, b.Zone)
	})
	return out
}

// SortZones orders zone ids like SortZoneEvents, keeping every id once.
func SortZones(zones, configured []string) []string {
	results := make([]ZoneEvents, 0, len(zones)+len(configured))
	seen := map[string]bool{}
	for _, z := range append(slices.Clone(configured), zones...) {
		if !seen[z] {
			seen[z] = true
			results = append(results, ZoneEvents{Zone: z, Events: []Event{{}}})
		}
	}
	out := make([]string, 0, len(results))
	for _, r := range SortZoneEvents(results, configured) {
		out = append(out, r.Zone)
	}
	return out
}
