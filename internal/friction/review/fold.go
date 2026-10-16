package review

import (
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

// SpendSummary captures costs and tokens without changing the renderer contract.
type SpendSummary struct {
	Total                               *friction.USD
	SessionsWithStats, SessionsWithCost int
	InputTokens, OutputTokens           uint64
	RoleCosts, ModelCosts               map[string]friction.USD
}

type spendFold struct{ spend SpendSummary }

func newSpendFold() *spendFold {
	return &spendFold{spend: SpendSummary{
		RoleCosts: map[string]friction.USD{}, ModelCosts: map[string]friction.USD{},
	}}
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

func (f *spendFold) addUsage(u db.FrictionSessionUsage) {
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
}

func (f *spendFold) summary() *SpendSummary {
	if f.spend.SessionsWithStats == 0 {
		return nil
	}
	s := f.spend
	return &s
}
