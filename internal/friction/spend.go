package friction

import (
	"fmt"
	"math/big"
	"sort"
	"time"
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

// PeriodSpend is archive spend over one day or the trailing week.
// Days counts rows present.
type PeriodSpend struct { //nolint:recvcheck // add mutates; ranking methods use the planned value-receiver API.
	Total          USD
	Days           int
	Agents, Models map[string]USD
}

// ArchiveSpend is the archive spend block. WeekFrom and WeekTo are
// YYYY-MM-DD dates in Timezone.
type ArchiveSpend struct {
	Yesterday                  *PeriodSpend
	Week                       PeriodSpend
	WeekFrom, WeekTo, Timezone string
}

// DailySpend is one local-date usage row.
type DailySpend struct {
	Date           string
	Total          USD
	Agents, Models map[string]USD
}

// NamedUSD is one ranked agent or model cost.
type NamedUSD struct {
	Name string
	USD  USD
}

func (p *PeriodSpend) add(r DailySpend) {
	p.Days++
	p.Total = addUSD(p.Total, r.Total)
	for _, k := range sortedKeys(r.Agents) {
		p.Agents = addToMap(p.Agents, k, r.Agents[k])
	}
	for _, k := range sortedKeys(r.Models) {
		p.Models = addToMap(p.Models, k, r.Models[k])
	}
}

// AgentsByCost ranks agents by cost descending, ties by name.
func (p PeriodSpend) AgentsByCost() []NamedUSD { return rankByCost(p.Agents) }

// TopModels returns the n most expensive models, ties by name. Nonpositive
// limits return no models.
func (p PeriodSpend) TopModels(n int) []NamedUSD {
	if n <= 0 {
		return nil
	}
	ranked := rankByCost(p.Models)
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	return ranked
}

func rankByCost(m map[string]USD) []NamedUSD {
	out := make([]NamedUSD, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, NamedUSD{Name: k, USD: m[k]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return cmpUSD(out[i].USD, out[j].USD) > 0
	})
	return out
}

// ArchiveWindow returns (date-7, date-1), excluding the digest day.
func ArchiveWindow(date string) (from, to string, err error) {
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return "", "", fmt.Errorf("archive spend window: %w", err)
	}
	return d.AddDate(0, 0, -7).Format(time.DateOnly),
		d.AddDate(0, 0, -1).Format(time.DateOnly), nil
}

// SummarizeArchiveSpend buckets rows into yesterday and the trailing
// week, returning nil when no row falls in the window.
func SummarizeArchiveSpend(rows []DailySpend, date, timezone string) (*ArchiveSpend, error) {
	from, to, err := ArchiveWindow(date)
	if err != nil {
		return nil, err
	}
	var week PeriodSpend
	var yesterday *PeriodSpend
	for _, r := range rows {
		if _, err := time.Parse(time.DateOnly, r.Date); err != nil {
			return nil, fmt.Errorf("archive spend row date %q: %w", r.Date, err)
		}
		if r.Date < from || r.Date > to {
			continue
		}
		week.add(r)
		if r.Date == to {
			var y PeriodSpend
			y.add(r)
			yesterday = &y
		}
	}
	if week.Days == 0 {
		return nil, nil
	}
	return &ArchiveSpend{
		Yesterday: yesterday, Week: week,
		WeekFrom: from, WeekTo: to, Timezone: timezone,
	}, nil
}
