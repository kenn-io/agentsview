package friction

import (
	"math/big"
	"sort"
)

// RootRole keys top-level sessions in SpendSummary.RoleCosts
// (jilog digest.rs:1199; spec §8.3 step 5, D28).
const RootRole = "(root)"

// SessionUsage is one subject's usage, folded into the digest's spend
// and persona rollups. Role is RootRole or "subagent"; "" means root.
type SessionUsage struct {
	SubjectID, Role           string
	InputTokens, OutputTokens uint64
	CostUSD                   *USD
	ModelCosts                map[string]USD
}

// SpendSummary is the observed-spend block (jilog SpendSummary,
// digest.rs:183-198). Total is nil when no session carried a cost.
type SpendSummary struct {
	Total                               *USD
	SessionsWithStats, SessionsWithCost int
	InputTokens, OutputTokens           uint64
	RoleCosts, ModelCosts               map[string]USD
}

// Accumulate folds one session into the summary and returns its cost
// (nil when it had none), porting accumulate_stats (digest.rs:1169-1203).
// Costs arrive already parsed, so jilog's unparseable-cost warnings have
// no equivalent here.
func (s *SpendSummary) Accumulate(u SessionUsage) *USD {
	s.SessionsWithStats++
	s.InputTokens += u.InputTokens
	s.OutputTokens += u.OutputTokens
	for _, model := range sortedKeys(u.ModelCosts) {
		s.ModelCosts = addToMap(s.ModelCosts, model, u.ModelCosts[model])
	}
	if u.CostUSD == nil {
		return nil
	}
	cost := *u.CostUSD
	s.SessionsWithCost++
	s.Total = addPtr(s.Total, cost)
	role := u.Role
	if role == "" {
		role = RootRole
	}
	s.RoleCosts = addToMap(s.RoleCosts, role, cost)
	return &cost
}

// RecurrenceCostAnnotations ports recurrence_cost_annotations
// (digest.rs:1208-1243): for every non-deferral signal whose fingerprint
// was linked to an open issue before this build, sum the costs of the
// distinct subjects it occurred in. Fingerprints whose subjects carried
// no cost get no entry.
func RecurrenceCostAnnotations(
	sigs []Signal, openFingerprints map[string]bool, sessionCosts map[string]USD,
) map[string]USD {
	out := map[string]USD{}
	if len(openFingerprints) == 0 {
		return out
	}
	subjects := map[string]map[string]bool{}
	for _, sig := range sigs {
		// jilog excludes deferrals; D36 excludes interruptions.
		if sig.Kind == KindDeferral || sig.Kind == KindInterruption {
			continue
		}
		fp := sig.Fingerprint()
		if !openFingerprints[fp] {
			continue
		}
		if subjects[fp] == nil {
			subjects[fp] = map[string]bool{}
		}
		subjects[fp][sig.SubjectID] = true
	}
	for fp, set := range subjects {
		var sum *USD
		for _, id := range sortedKeys(set) {
			if c, ok := sessionCosts[id]; ok {
				sum = addPtr(sum, c)
			}
		}
		if sum != nil {
			out[fp] = *sum
		}
	}
	return out
}

// addUSD adds b to a, treating the zero USD (nil Coeff) as rust_decimal's
// Decimal::ZERO at scale 0, so 0 + b keeps b's scale exactly.
func addUSD(a, b USD) USD {
	if a.Coeff == nil {
		return b
	}
	return a.Add(b)
}

func addPtr(total *USD, v USD) *USD {
	if total == nil {
		c := v
		return &c
	}
	sum := total.Add(v)
	return &sum
}

func addToMap(m map[string]USD, k string, v USD) map[string]USD {
	if m == nil {
		m = map[string]USD{}
	}
	m[k] = addUSD(m[k], v)
	return m
}

// cmpUSD compares two amounts numerically, independent of scale.
func cmpUSD(a, b USD) int {
	scale := max(a.Scale, b.Scale)
	return scaledCoeff(a, scale).Cmp(scaledCoeff(b, scale))
}

func scaledCoeff(u USD, scale uint8) *big.Int {
	c := new(big.Int)
	if u.Coeff != nil {
		c.Set(u.Coeff)
	}
	if d := scale - u.Scale; d > 0 {
		c.Mul(c, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil))
	}
	return c
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
