package postgres

import (
	"go.kenn.io/agentsview/internal/rawparity"
)

// rawLogicalSupersessions matches the global universe before eligibility. One
// logical group may have several physical revisions/cohorts. Public IDs and
// content equality supply no identity evidence. Eligibility must be supplied
// explicitly by the caller; no-scope/default-active is not activation authority.
func rawLogicalSupersessions(rows []rawparity.Candidate, served map[string]bool, previous map[string]string) map[string]string {
	// New automatic pairs retain physical ambiguity checks. An established map
	// may survive a physical split, but only with the same unique logical edge.
	initial := map[string]string{}
	for _, pair := range rawparity.Match(rows) {
		if pair.Class == "equal" {
			initial[pair.Legacy[0].ID] = pair.Raw[0].Group
		}
	}
	logical := []rawparity.Candidate{}
	groups := map[string]int{}
	for _, c := range rows {
		if c.Group == "" {
			logical = append(logical, c)
			continue
		}
		i, ok := groups[c.Group]
		if !ok {
			i = len(logical)
			groups[c.Group] = i
			logical = append(logical, rawparity.Candidate{ID: c.Group, Group: c.Group, Agent: c.Agent})
		}
		logical[i].Aliases = append(logical[i].Aliases, c.Aliases...)
		logical[i].SourceLocal = logical[i].SourceLocal || c.SourceLocal
	}
	out := map[string]string{}
	for _, pair := range rawparity.Match(logical) {
		if pair.Class == "equal" {
			legacy, group := pair.Legacy[0].ID, pair.Raw[0].Group
			if served[group] && (initial[legacy] == group || previous[legacy] == group) {
				out[legacy] = group
			}
		}
	}
	return out
}
