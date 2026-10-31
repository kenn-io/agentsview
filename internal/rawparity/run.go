package rawparity

import (
	"context"
	"errors"
	"slices"
)

// Scope selects raw source devices/providers. Legacy machine names are not
// device identities; legacy-only rows remain tenant history under device filters.
type Scope struct {
	Device   string `json:"device"`
	Provider string `json:"provider"`
}
type Readiness struct {
	Device      string `json:"device"`
	Provider    string `json:"provider"`
	Projected   int    `json:"projected"`
	Pending     int    `json:"pending"`
	Failed      int    `json:"failed"`
	Unsupported int    `json:"unsupported"`
}

// Reader binds every method to one validated physical database snapshot. Keeping
// SQL in the adapter avoids a dependency cycle with postgres's Snapshot loader.
type Reader interface {
	Candidates(context.Context) ([]Candidate, error)
	Readiness(context.Context, Scope) ([]Readiness, error)
	Snapshots(context.Context, []string) (map[string]Snapshot, error)
	Links(context.Context, string) ([]Link, error)
}

type (
	Counts    map[string]int
	LegacyRef struct {
		ID      string `json:"id"`
		Machine string `json:"machine"`
	}
)

type Entry struct {
	Class     string      `json:"class"`
	RawIDs    []string    `json:"raw_ids,omitempty"`
	Legacy    []LegacyRef `json:"legacy,omitempty"`
	Differing []string    `json:"differing,omitempty"`
}
type Report struct {
	Scope     Scope                        `json:"scope"`
	Pass      bool                         `json:"pass"`
	Counts    Counts                       `json:"counts"`
	Devices   map[string]map[string]Counts `json:"devices"`
	Readiness []Readiness                  `json:"readiness"`
	Sessions  []Entry                      `json:"sessions"`
}

// Passes separates source readiness from content. Unsupported heads, empty
// scopes and unresolved identity never pass; one-sided retained history may.
func (r Report) Passes() bool {
	projected := 0
	for _, s := range r.Readiness {
		projected += s.Projected
		if s.Pending+s.Failed+s.Unsupported != 0 {
			return false
		}
	}
	return projected > 0 && r.Counts["differs"]+r.Counts["ambiguous"]+r.Counts["unmatchable"] == 0
}

func emptyCounts() Counts {
	return Counts{"equal": 0, "differs": 0, "raw_only": 0, "legacy_only": 0, "ambiguous": 0, "unmatchable": 0}
}

// Run classifies globally before filtering, so a device filter cannot turn an
// ambiguous shared group into a match. Complete transcripts are loaded only for
// unique pairs, one pair at a time, while identity evidence stays in memory.
func Run(ctx context.Context, reader Reader, scope Scope) (Report, error) {
	r := Report{Scope: scope, Counts: emptyCounts(), Devices: map[string]map[string]Counts{}, Sessions: []Entry{}}
	rows, err := reader.Candidates(ctx)
	if err != nil {
		return r, err
	}
	r.Readiness, err = reader.Readiness(ctx, scope)
	if err != nil {
		return r, err
	}
	matches := Match(rows)
	ids := map[string]string{}
	for _, m := range matches {
		if m.Class == "equal" {
			ids[m.Raw[0].ID] = m.Legacy[0].ID
		}
	}
	for _, m := range matches {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		selected := func(c Candidate) bool {
			return (scope.Provider == "" || scope.Provider == c.Agent) && (scope.Device == "" || slices.Contains(c.Devices, scope.Device))
		}
		if len(m.Raw) > 0 {
			if !slices.ContainsFunc(m.Raw, selected) {
				continue
			}
		} else if scope.Provider != "" && m.Legacy[0].Agent != scope.Provider {
			continue
		}
		entry := Entry{Class: m.Class}
		if m.Class == "equal" {
			a, b := m.Raw[0].ID, m.Legacy[0].ID
			snapshots, err := reader.Snapshots(ctx, []string{a, b})
			if err != nil {
				return r, err
			}
			left, lok := snapshots[a]
			right, rok := snapshots[b]
			if !lok || !rok {
				return r, errors.New("parity snapshot is missing a candidate")
			}
			links, err := reader.Links(ctx, a)
			if err != nil {
				return r, err
			}
			comparison, err := CompareResolved(left, right, links, ids)
			if err != nil {
				return r, err
			}
			if len(comparison.Differing) > 0 {
				entry.Class = "differs"
				entry.Differing = comparison.Differing
			}
		}
		r.Counts[entry.Class]++
		seen := map[[2]string]bool{}
		for _, c := range m.Raw {
			entry.RawIDs = append(entry.RawIDs, c.PublicID)
			for _, device := range c.Devices {
				if scope.Device != "" && device != scope.Device {
					continue
				}
				key := [2]string{device, c.Agent}
				if seen[key] {
					continue
				}
				seen[key] = true
				if r.Devices[device] == nil {
					r.Devices[device] = map[string]Counts{}
				}
				if r.Devices[device][c.Agent] == nil {
					r.Devices[device][c.Agent] = emptyCounts()
				}
				r.Devices[device][c.Agent][entry.Class]++
			}
		}
		for _, c := range m.Legacy {
			entry.Legacy = append(entry.Legacy, LegacyRef{ID: c.PublicID, Machine: c.Machine})
		}
		if entry.Class != "equal" {
			r.Sessions = append(r.Sessions, entry)
		}
	}
	r.Pass = r.Passes()
	return r, nil
}
