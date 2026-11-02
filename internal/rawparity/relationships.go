package rawparity

import (
	"encoding/json/v2"
	"errors"
	"slices"

	"go.kenn.io/agentsview/internal/db"
)

// Link is one raw relationship coordinate resolved from source-owned physical
// evidence. Kind is parent, call, event or usage_root. Targets retain hidden/trashed rows.
// Missing means at least one recorded owner edge
// has no admissible target, including an ambiguous cross-source fallback.
// A prefix owner without an edge makes no claim at that coordinate.
// Loaders must use the same validated physical snapshot as session content.
type Link struct {
	Kind                           string
	Ordinal, CallIndex, EventIndex int
	Targets                        []string
	Missing                        bool
}

// CompareResolved compares a raw snapshot's source-owned physical relationship
// targets against legacy physical IDs. matches must contain only globally unique
// raw-to-legacy identity pairs established before any scope filtering. Missing,
// plural or unmapped targets cannot pass merely because literal aliases agree.
// Unknown kinds, duplicate semantic coordinates and orphan links return errors.
// The inputs and their child storage are never modified. Compare remains useful
// for snapshots whose caller has already resolved relationships.
func CompareResolved(raw, legacy Snapshot, links []Link, matches map[string]string) (Comparison, error) {
	var err error
	raw, err = resolvedRelationships(raw, links, matches)
	if err != nil {
		return Comparison{}, err
	}
	legacy.MatchIDs = legacyRelationshipIDs(legacy)
	return Compare(raw, legacy)
}

// Keep unresolved physical IDs in a separate domain from every legacy ID.
// Literal strings, even strings shaped like these tokens, cannot prove a match.
func legacyRelationshipIDs(s Snapshot) map[string]string {
	ids := map[string]string{}
	add := func(id string) {
		if id != "" {
			ids[id] = "\x00legacy:" + id
		}
	}
	if s.Session.ParentSessionID != nil {
		add(*s.Session.ParentSessionID)
	}
	for _, id := range s.Session.ParentSessionIDs {
		add(id)
	}
	if s.PlanUsage != nil {
		add(s.PlanUsage.CountedUnder)
	}
	for _, m := range s.Messages {
		for _, c := range m.ToolCalls {
			add(c.SubagentSessionID)
			for _, e := range c.ResultEvents {
				add(e.SubagentSessionID)
			}
		}
	}
	return ids
}

func resolvedRelationships(s Snapshot, links []Link, matches map[string]string) (Snapshot, error) {
	const unresolved = "\x00unresolved"
	type coordinate struct {
		kind                 string
		ordinal, call, event int
	}
	calls := map[coordinate]*db.ToolCall{}
	events := map[coordinate]*db.ToolResultEvent{}
	// Snapshots can be reused by a reader: never mutate its child slice storage.
	s.MatchIDs = nil
	s.Messages = slices.Clone(s.Messages)
	if s.Session.ParentSessionID != nil && *s.Session.ParentSessionID != "" || len(s.Session.ParentSessionIDs) > 0 {
		s.Session.ParentSessionIDs = []string{unresolved}
	}
	s.Session.ParentSessionID = nil
	if s.PlanUsage != nil {
		p := *s.PlanUsage
		if p.CountedUnder != "" {
			p.CountedUnder = unresolved
		}
		s.PlanUsage = &p
	}
	for mi := range s.Messages {
		m := &s.Messages[mi]
		m.ToolCalls = slices.Clone(m.ToolCalls)
		for ci := range m.ToolCalls {
			c := &m.ToolCalls[ci]
			c.ResultEvents = slices.Clone(c.ResultEvents)
			calls[coordinate{kind: "call", ordinal: m.Ordinal, call: c.CallIndex}] = c
			if c.SubagentSessionID != "" {
				c.SubagentSessionID = unresolved
			}
			for ei := range c.ResultEvents {
				events[coordinate{kind: "event", ordinal: m.Ordinal, call: c.CallIndex, event: c.ResultEvents[ei].EventIndex}] = &c.ResultEvents[ei]
				if c.ResultEvents[ei].SubagentSessionID != "" {
					c.ResultEvents[ei].SubagentSessionID = unresolved
				}
			}
		}
	}
	seen := map[coordinate]bool{}
	for _, link := range links {
		key := coordinate{kind: link.Kind}
		if link.Kind == "call" || link.Kind == "event" {
			key.ordinal = link.Ordinal
			key.call = link.CallIndex
		}
		if link.Kind == "event" {
			key.event = link.EventIndex
		}
		if seen[key] {
			return Snapshot{}, errors.New("duplicate parity relationship coordinate")
		}
		seen[key] = true
		if link.Kind != "parent" && link.Kind != "call" && link.Kind != "event" && link.Kind != "usage_root" {
			return Snapshot{}, errors.New("unknown parity relationship kind")
		}
		targets := []string{}
		for _, id := range link.Targets {
			if legacy, ok := matches[id]; ok {
				targets = append(targets, "\x00legacy:"+legacy)
			} else {
				targets = append(targets, "\x00raw:"+id)
			}
		}
		if link.Missing || len(targets) == 0 {
			targets = append(targets, unresolved)
		}
		slices.Sort(targets)
		targets = slices.Compact(targets)
		if link.Kind == "parent" {
			s.Session.ParentSessionIDs = targets
			continue
		}
		target := targets[0]
		if len(targets) > 1 {
			encoded, err := json.Marshal(targets)
			if err != nil {
				return Snapshot{}, errors.New("cannot encode parity relationship targets")
			}
			target = "\x00plural:" + string(encoded)
		}
		switch link.Kind {
		case "usage_root":
			if s.PlanUsage == nil {
				return Snapshot{}, errors.New("parity usage root has no owning plan usage")
			}
			s.PlanUsage.CountedUnder = target
		case "call":
			c := calls[key]
			if c == nil {
				return Snapshot{}, errors.New("parity relationship has no owning coordinate")
			}
			c.SubagentSessionID = target
		default:
			e := events[key]
			if e == nil {
				return Snapshot{}, errors.New("parity relationship has no owning coordinate")
			}
			e.SubagentSessionID = target
		}
	}
	return s, nil
}
