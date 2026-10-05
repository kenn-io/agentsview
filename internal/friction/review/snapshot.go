package review

import (
	"encoding/json/v2"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/friction"
)

// snapshot_json is the canonical frozen input of a digest (spec §5.4). The
// wire form flattens struct-keyed maps and stores USD as decimal strings so
// scale survives the round trip.
type snapshotWire struct {
	Date            string              `json:"date"`
	Timezone        string              `json:"timezone"`
	RulesVersion    string              `json:"rules_version"`
	Signals         []signalWire        `json:"signals"`
	P0Alerts        map[string][]string `json:"p0_alerts"`
	Personas        []personaWire       `json:"personas"`
	Spend           *spendWire          `json:"spend"`
	ArchiveSpend    *archiveWire        `json:"archive_spend"`
	SessionsScanned int                 `json:"sessions_scanned"`
	RecurrenceCosts map[string]string   `json:"recurrence_costs"`
}

type signalWire struct {
	Kind        string `json:"kind"`
	SubjectID   string `json:"subject_id"`
	SubjectKind string `json:"subject_kind"`
	Seat        string `json:"seat"`
	Agent       string `json:"agent"`
	Machine     string `json:"machine"`
	Persona     string `json:"persona"`
	Channel     string `json:"channel"`
	Detector    string `json:"detector"`
	Text        string `json:"text"`
	ToolName    string `json:"tool_name"`
	Label       string `json:"label"`
	Evidence    string `json:"evidence"`
	Ordinal     *int   `json:"ordinal"`
	CallIndex   *int   `json:"call_index"`
	OccurredAt  string `json:"occurred_at"`
	Seq         int    `json:"seq"`
}

type personaWire struct {
	Persona      string  `json:"persona"`
	Channel      string  `json:"channel"`
	Sessions     int     `json:"sessions"`
	Corrections  int     `json:"corrections"`
	Errors       int     `json:"errors"`
	Workarounds  int     `json:"workarounds"`
	Deferrals    int     `json:"deferrals"`
	Patterns     int     `json:"patterns"`
	InputTokens  uint64  `json:"input_tokens"`
	OutputTokens uint64  `json:"output_tokens"`
	CostUSD      *string `json:"cost_usd"`
}

type spendWire struct {
	Total             *string           `json:"total"`
	SessionsWithStats int               `json:"sessions_with_stats"`
	SessionsWithCost  int               `json:"sessions_with_cost"`
	InputTokens       uint64            `json:"input_tokens"`
	OutputTokens      uint64            `json:"output_tokens"`
	RoleCosts         map[string]string `json:"role_costs"`
	ModelCosts        map[string]string `json:"model_costs"`
}

type periodWire struct {
	Total  string            `json:"total"`
	Days   int               `json:"days"`
	Agents map[string]string `json:"agents"`
	Models map[string]string `json:"models"`
}

type archiveWire struct {
	Yesterday *periodWire `json:"yesterday"`
	Week      periodWire  `json:"week"`
	WeekFrom  string      `json:"week_from"`
	WeekTo    string      `json:"week_to"`
	Timezone  string      `json:"timezone"`
}

func usdStrings(m map[string]friction.USD) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.String()
	}
	return out
}

func parseUSDMap(m map[string]string) (map[string]friction.USD, error) {
	out := make(map[string]friction.USD, len(m))
	for k, v := range m {
		u, err := friction.ParseUSD(v)
		if err != nil {
			return nil, fmt.Errorf("snapshot usd %q: %w", k, err)
		}
		out[k] = u
	}
	return out, nil
}

func usdPtrString(u *friction.USD) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

func parseUSDPtr(s *string) (*friction.USD, error) {
	if s == nil {
		return nil, nil
	}
	u, err := friction.ParseUSD(*s)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func periodToWire(p friction.PeriodSpend) periodWire {
	return periodWire{Total: p.Total.String(), Days: p.Days, Agents: usdStrings(p.Agents), Models: usdStrings(p.Models)}
}

func periodFromWire(w periodWire) (friction.PeriodSpend, error) {
	total, err := friction.ParseUSD(w.Total)
	if err != nil {
		return friction.PeriodSpend{}, err
	}
	agents, err := parseUSDMap(w.Agents)
	if err != nil {
		return friction.PeriodSpend{}, err
	}
	models, err := parseUSDMap(w.Models)
	if err != nil {
		return friction.PeriodSpend{}, err
	}
	return friction.PeriodSpend{Total: total, Days: w.Days, Agents: agents, Models: models}, nil
}

func EncodeSnapshot(s friction.DigestSnapshot) ([]byte, error) {
	w := snapshotWire{
		Date: s.Date, Timezone: s.Timezone, RulesVersion: s.RulesVersion,
		Signals: make([]signalWire, 0, len(s.Signals)), P0Alerts: s.P0Alerts,
		Personas: make([]personaWire, 0, len(s.Personas)), SessionsScanned: s.SessionsScanned,
		RecurrenceCosts: usdStrings(s.RecurrenceCosts),
	}
	for _, sig := range s.Signals {
		sw := signalWire{
			Kind: string(sig.Kind), SubjectID: sig.SubjectID, SubjectKind: sig.SubjectKind,
			Seat:  sig.Dims.Seat,
			Agent: sig.Dims.Agent, Machine: sig.Dims.Machine, Persona: sig.Dims.Persona,
			Channel: sig.Dims.Channel, Detector: sig.Detector, Text: sig.Text,
			ToolName: sig.ToolName, Label: sig.Label, Evidence: sig.Evidence,
			Ordinal: sig.Ordinal, CallIndex: sig.CallIndex, Seq: sig.Seq,
		}
		if !sig.OccurredAt.IsZero() {
			sw.OccurredAt = sig.OccurredAt.UTC().Format(time.RFC3339Nano)
		}
		w.Signals = append(w.Signals, sw)
	}
	for k, pc := range s.Personas {
		w.Personas = append(w.Personas, personaWire{
			Persona: k.Persona, Channel: k.Channel, Sessions: pc.Sessions,
			Corrections: pc.Corrections, Errors: pc.Errors, Workarounds: pc.Workarounds,
			Deferrals: pc.Deferrals, Patterns: pc.Patterns, InputTokens: pc.InputTokens,
			OutputTokens: pc.OutputTokens, CostUSD: usdPtrString(pc.CostUSD),
		})
	}
	sort.Slice(w.Personas, func(i, j int) bool {
		if w.Personas[i].Persona != w.Personas[j].Persona {
			return w.Personas[i].Persona < w.Personas[j].Persona
		}
		return w.Personas[i].Channel < w.Personas[j].Channel
	})
	if s.Spend != nil {
		w.Spend = &spendWire{
			Total: usdPtrString(s.Spend.Total), SessionsWithStats: s.Spend.SessionsWithStats,
			SessionsWithCost: s.Spend.SessionsWithCost, InputTokens: s.Spend.InputTokens,
			OutputTokens: s.Spend.OutputTokens, RoleCosts: usdStrings(s.Spend.RoleCosts),
			ModelCosts: usdStrings(s.Spend.ModelCosts),
		}
	}
	if a := s.ArchiveSpend; a != nil {
		aw := &archiveWire{Week: periodToWire(a.Week), WeekFrom: a.WeekFrom, WeekTo: a.WeekTo, Timezone: a.Timezone}
		if a.Yesterday != nil {
			y := periodToWire(*a.Yesterday)
			aw.Yesterday = &y
		}
		w.ArchiveSpend = aw
	}
	b, err := json.Marshal(w, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encoding friction snapshot: %w", err)
	}
	return b, nil
}

func DecodeSnapshot(b []byte) (friction.DigestSnapshot, error) {
	var w snapshotWire
	if err := json.Unmarshal(b, &w); err != nil {
		return friction.DigestSnapshot{}, fmt.Errorf("decoding friction snapshot: %w", err)
	}
	s := friction.DigestSnapshot{
		Date: w.Date, Timezone: w.Timezone, RulesVersion: w.RulesVersion,
		P0Alerts: w.P0Alerts, SessionsScanned: w.SessionsScanned,
		Personas: make(map[friction.PersonaKey]*friction.PersonaCounts, len(w.Personas)),
	}
	for _, sw := range w.Signals {
		sig := friction.Signal{
			Kind: friction.Kind(sw.Kind), SubjectID: sw.SubjectID, SubjectKind: sw.SubjectKind,
			Dims:     friction.Dims{Seat: sw.Seat, Agent: sw.Agent, Machine: sw.Machine, Persona: sw.Persona, Channel: sw.Channel},
			Detector: sw.Detector, Text: sw.Text, ToolName: sw.ToolName, Label: sw.Label,
			Evidence: sw.Evidence, Ordinal: sw.Ordinal, CallIndex: sw.CallIndex, Seq: sw.Seq,
		}
		if sw.OccurredAt != "" {
			ts, err := time.Parse(time.RFC3339Nano, sw.OccurredAt)
			if err != nil {
				return friction.DigestSnapshot{}, fmt.Errorf("snapshot occurred_at: %w", err)
			}
			sig.OccurredAt = ts.UTC()
		}
		s.Signals = append(s.Signals, sig)
	}
	for _, pw := range w.Personas {
		cost, err := parseUSDPtr(pw.CostUSD)
		if err != nil {
			return friction.DigestSnapshot{}, err
		}
		s.Personas[friction.PersonaKey{Persona: pw.Persona, Channel: pw.Channel}] = &friction.PersonaCounts{
			Sessions: pw.Sessions, Corrections: pw.Corrections, Errors: pw.Errors,
			Workarounds: pw.Workarounds, Deferrals: pw.Deferrals, Patterns: pw.Patterns,
			InputTokens: pw.InputTokens, OutputTokens: pw.OutputTokens, CostUSD: cost,
		}
	}
	if sw := w.Spend; sw != nil {
		total, err := parseUSDPtr(sw.Total)
		if err != nil {
			return friction.DigestSnapshot{}, err
		}
		roles, err := parseUSDMap(sw.RoleCosts)
		if err != nil {
			return friction.DigestSnapshot{}, err
		}
		models, err := parseUSDMap(sw.ModelCosts)
		if err != nil {
			return friction.DigestSnapshot{}, err
		}
		s.Spend = &friction.SpendSummary{
			Total: total, SessionsWithStats: sw.SessionsWithStats,
			SessionsWithCost: sw.SessionsWithCost, InputTokens: sw.InputTokens,
			OutputTokens: sw.OutputTokens, RoleCosts: roles, ModelCosts: models,
		}
	}
	if aw := w.ArchiveSpend; aw != nil {
		week, err := periodFromWire(aw.Week)
		if err != nil {
			return friction.DigestSnapshot{}, err
		}
		a := &friction.ArchiveSpend{Week: week, WeekFrom: aw.WeekFrom, WeekTo: aw.WeekTo, Timezone: aw.Timezone}
		if aw.Yesterday != nil {
			y, err := periodFromWire(*aw.Yesterday)
			if err != nil {
				return friction.DigestSnapshot{}, err
			}
			a.Yesterday = &y
		}
		s.ArchiveSpend = a
	}
	costs, err := parseUSDMap(w.RecurrenceCosts)
	if err != nil {
		return friction.DigestSnapshot{}, err
	}
	s.RecurrenceCosts = costs
	return s, nil
}
