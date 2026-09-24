package review

import "go.kenn.io/agentsview/internal/friction"

// spendFold ports fold_session_stats/accumulate_stats (digest.rs:1134-1203)
// and the persona rollup (digest.rs:385-424). In the archive every subject
// has messages, so a subject is always counted through countSession and
// addUsage folds with jilog's count_session=false.
type spendFold struct {
	spend    friction.SpendSummary
	personas map[friction.PersonaKey]*friction.PersonaCounts
}

func newSpendFold() *spendFold {
	return &spendFold{
		spend: friction.SpendSummary{
			RoleCosts:  map[string]friction.USD{},
			ModelCosts: map[string]friction.USD{},
		},
		personas: map[friction.PersonaKey]*friction.PersonaCounts{},
	}
}

func addUSD(m map[string]friction.USD, key string, v friction.USD) {
	if cur, ok := m[key]; ok {
		m[key] = cur.Add(v)
		return
	}
	m[key] = v
}

func sumUSD(p *friction.USD, v friction.USD) *friction.USD {
	if p == nil {
		c := v
		return &c
	}
	s := p.Add(v)
	return &s
}

func (f *spendFold) persona(key *friction.PersonaKey) *friction.PersonaCounts {
	pc := f.personas[*key]
	if pc == nil {
		pc = &friction.PersonaCounts{}
		f.personas[*key] = pc
	}
	return pc
}

// countSession folds one subject's signal counts into its persona. Sessions
// with no signals still count (digest.rs:411-424).
func (f *spendFold) countSession(key *friction.PersonaKey, sigs []friction.Signal) {
	if key == nil {
		return
	}
	pc := f.persona(key)
	pc.Sessions++
	for _, s := range sigs {
		switch s.Kind {
		case friction.KindCorrection:
			pc.Corrections++
		case friction.KindError:
			pc.Errors++
		case friction.KindWorkaround:
			pc.Workarounds++
		case friction.KindDeferral:
			pc.Deferrals++
		case friction.KindPattern:
			pc.Patterns++
		case friction.KindFrustration, friction.KindInterruption:
			// Not in jilog's persona line (spec §9.1); the digest's own
			// frontmatter and sections count them.
		}
	}
}

// addUsage is accumulate_stats plus the persona usage part of
// fold_session_stats.
func (f *spendFold) addUsage(key *friction.PersonaKey, u friction.SessionUsage) {
	f.spend.SessionsWithStats++
	f.spend.InputTokens += u.InputTokens
	f.spend.OutputTokens += u.OutputTokens
	for model, c := range u.ModelCosts {
		addUSD(f.spend.ModelCosts, model, c)
	}
	if u.CostUSD != nil {
		f.spend.SessionsWithCost++
		f.spend.Total = sumUSD(f.spend.Total, *u.CostUSD)
		role := u.Role
		if role == "" {
			role = "(root)"
		}
		addUSD(f.spend.RoleCosts, role, *u.CostUSD)
	}
	if key != nil {
		pc := f.persona(key)
		pc.InputTokens += u.InputTokens
		pc.OutputTokens += u.OutputTokens
		if u.CostUSD != nil {
			pc.CostUSD = sumUSD(pc.CostUSD, *u.CostUSD)
		}
	}
}

func (f *spendFold) summary() *friction.SpendSummary {
	if f.spend.SessionsWithStats == 0 {
		return nil
	}
	s := f.spend
	return &s
}
