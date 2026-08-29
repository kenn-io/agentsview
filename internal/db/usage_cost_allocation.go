package db

import (
	"sort"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// usageCostAllocationKey is the daily usage breakdown granularity used when
// distributing an authoritative session total.
type usageCostAllocationKey struct {
	date       string
	project    string
	agent      string
	machine    string
	model      string
	providerID string
}

func sortedUsageCostAllocationKeys(
	estimated map[usageCostAllocationKey]money.Money,
) []usageCostAllocationKey {
	keys := make([]usageCostAllocationKey, 0, len(estimated))
	for key := range estimated {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.date != b.date {
			return a.date < b.date
		}
		if a.project != b.project {
			return a.project < b.project
		}
		if a.agent != b.agent {
			return a.agent < b.agent
		}
		if a.machine != b.machine {
			return a.machine < b.machine
		}
		if a.providerID != b.providerID {
			return a.providerID < b.providerID
		}
		return a.model < b.model
	})
	return keys
}

func allocateUsageCostByKey(
	total money.Money,
	estimated map[usageCostAllocationKey]money.Money,
) map[usageCostAllocationKey]money.Money {
	keys := sortedUsageCostAllocationKeys(estimated)
	weights := make([]money.Money, len(keys))
	for i, key := range keys {
		weights[i] = estimated[key]
	}
	costs := export.AllocateCostByWeight(total, weights)
	allocated := make(map[usageCostAllocationKey]money.Money, len(keys))
	for i, key := range keys {
		allocated[key] = costs[i]
	}
	return allocated
}
