package friction

import (
	"fmt"
	"sort"
)

// PersonaKey identifies one fleet persona/channel pair. Channel "" means
// the agent has no registered channel (jilog's None).
type PersonaKey struct{ Persona, Channel string }

// PersonaCounts is one persona's rollup (jilog PersonaCounts,
// digest.rs:81-121). Tokens always; CostUSD only when a session carried
// a cost (jilog#2hwm).
type PersonaCounts struct { //nolint:recvcheck // Mutating fold methods need pointers; read-only helpers keep value semantics.
	Sessions, Corrections, Errors, Workarounds, Deferrals, Patterns int
	InputTokens, OutputTokens                                       uint64
	CostUSD                                                         *USD
}

// AddSignals adds one session's per-kind counts (digest.rs:447-462). The
// caller increments Sessions once per subject. Frustration and
// interruption signals are not counted (D36 keeps the persona line
// jilog-exact).
func (c *PersonaCounts) AddSignals(sigs []Signal) {
	for _, s := range sigs {
		switch s.Kind {
		case KindCorrection:
			c.Corrections++
		case KindError:
			c.Errors++
		case KindWorkaround:
			c.Workarounds++
		case KindDeferral:
			c.Deferrals++
		case KindPattern:
			c.Patterns++
		case KindFrustration, KindInterruption:
			// Not part of the persona rollup (D36).
		}
	}
}

// AddUsage folds one session's usage (fold_session_stats,
// digest.rs:1134-1164, persona half).
func (c *PersonaCounts) AddUsage(u SessionUsage) {
	c.InputTokens += u.InputTokens
	c.OutputTokens += u.OutputTokens
	if u.CostUSD != nil {
		c.CostUSD = addPtr(c.CostUSD, *u.CostUSD)
	}
}

func (c PersonaCounts) signalTotal() int {
	return c.Corrections + c.Errors + c.Workarounds + c.Deferrals + c.Patterns
}

func (c PersonaCounts) hasUsage() bool {
	return c.InputTokens > 0 || c.OutputTokens > 0 || c.CostUSD != nil
}

// PersonaDisplayKey is `persona@channel`, or the bare persona without a
// channel, both parts sanitized for a Markdown code span (persona_key,
// digest.rs:123-137).
func PersonaDisplayKey(k PersonaKey) string {
	p := SanitizeDisplay(k.Persona)
	if k.Channel == "" {
		return p
	}
	return p + "@" + SanitizeDisplay(k.Channel)
}

// PersonaEntry is one display-keyed rollup row.
type PersonaEntry struct {
	Key        string
	PersonaKey PersonaKey
	Counts     PersonaCounts
}

// DisplayKeyedPersonas ports display_keyed_personas (digest.rs:156-177):
// tuples are visited in (persona, channel) order, colliding display keys
// get " (2)", " (3)", … suffixes, and the result is ordered by display
// key bytes like jilog's BTreeMap.
func DisplayKeyedPersonas(m map[PersonaKey]*PersonaCounts) []PersonaEntry {
	tuples := make([]PersonaKey, 0, len(m))
	for k := range m {
		tuples = append(tuples, k)
	}
	sort.Slice(tuples, func(i, j int) bool {
		if tuples[i].Persona != tuples[j].Persona {
			return tuples[i].Persona < tuples[j].Persona
		}
		return tuples[i].Channel < tuples[j].Channel
	})
	used := make(map[string]bool, len(tuples))
	out := make([]PersonaEntry, 0, len(tuples))
	for _, k := range tuples {
		base := PersonaDisplayKey(k)
		key := base
		for n := 2; used[key]; n++ {
			key = fmt.Sprintf("%s (%d)", base, n)
		}
		used[key] = true
		var counts PersonaCounts
		if c := m[k]; c != nil {
			counts = *c
		}
		out = append(out, PersonaEntry{Key: key, PersonaKey: k, Counts: counts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
