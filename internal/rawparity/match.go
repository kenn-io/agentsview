package rawparity

import (
	"slices"
	"strings"
)

// Candidate contains identity evidence only. Group is empty for legacy rows;
// Aliases are recorded parser IDs, never guessed thread IDs or stripped prefixes.
// Devices includes every source sharing this physical row, before scope filters.
type Candidate struct {
	ID, PublicID, Agent, Machine, Group string
	Aliases, Devices                    []string
	SourceLocal                         bool
}

// MatchResult counts a connected candidate component once. Equal means a unique
// identity pair pending content comparison; it does not establish content parity.
type MatchResult struct {
	Class       string
	Raw, Legacy []Candidate
}

// Match builds a bipartite candidate graph using exact (agent, parser ID) edges.
// Host-prefixed/multi-machine IDs remain legacy history. Source-local collisions
// never establish automatic identity, even if their content happens to agree.
func Match(rows []Candidate) []MatchResult {
	type key struct{ agent, id string }
	parents := make([]int, len(rows))
	for i := range parents {
		parents[i] = i
	}
	var root func(int) int
	root = func(i int) int {
		for parents[i] != i {
			parents[i] = parents[parents[i]]
			i = parents[i]
		}
		return i
	}
	legacy := map[key]int{}
	for i, c := range rows {
		if c.Group == "" && !strings.Contains(c.ID, "~") {
			legacy[key{c.Agent, c.ID}] = i
		}
	}
	for i, c := range rows {
		if c.Group == "" {
			continue
		}
		for _, alias := range c.Aliases {
			if j, ok := legacy[key{c.Agent, alias}]; ok {
				parents[root(i)] = root(j)
			}
		}
	}
	positions := map[int]int{}
	out := []MatchResult{}
	for i, c := range rows {
		r := root(i)
		pos, ok := positions[r]
		if !ok {
			pos = len(out)
			positions[r] = pos
			out = append(out, MatchResult{})
		}
		if c.Group == "" {
			out[pos].Legacy = append(out[pos].Legacy, c)
		} else {
			out[pos].Raw = append(out[pos].Raw, c)
		}
	}
	for i := range out {
		c := &out[i]
		switch {
		case len(c.Raw) == 0:
			c.Class = "legacy_only"
		case len(c.Legacy) == 0:
			c.Class = "raw_only"
		case slices.ContainsFunc(c.Raw, func(r Candidate) bool { return r.SourceLocal }):
			c.Class = "unmatchable"
		case len(c.Raw) > 1 || len(c.Legacy) > 1:
			c.Class = "ambiguous"
		default:
			c.Class = "equal"
		}
	}
	return out
}
