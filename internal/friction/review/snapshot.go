package review

import (
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

// Snapshot freezes the renderer input and usage at build time.
type Snapshot struct {
	friction.DigestSnapshot
	Timezone, RulesVersion string
	Spend                  *SpendSummary
	ArchiveSpend           *db.FrictionArchiveSpendSummary
}

// snapshot_json is the canonical frozen input of a digest (spec §5.4). The
// wire form stores USD as decimal strings so scale survives the round trip.
type snapshotWire struct {
	Date            string              `json:"date"`
	Timezone        string              `json:"timezone"`
	RulesVersion    string              `json:"rules_version"`
	Signals         []signalWire        `json:"signals"`
	P0Alerts        map[string][]string `json:"p0_alerts"`
	Spend           *spendWire          `json:"spend"`
	ArchiveSpend    *archiveWire        `json:"archive_spend"`
	SessionsScanned int                 `json:"sessions_scanned"`
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

func periodToWire(p db.FrictionPeriodSpend) periodWire {
	return periodWire{Total: p.Total.String(), Days: p.Days, Agents: usdStrings(p.Agents), Models: usdStrings(p.Models)}
}

func periodFromWire(w periodWire) (db.FrictionPeriodSpend, error) {
	total, err := friction.ParseUSD(w.Total)
	if err != nil {
		return db.FrictionPeriodSpend{}, err
	}
	agents, err := parseUSDMap(w.Agents)
	if err != nil {
		return db.FrictionPeriodSpend{}, err
	}
	models, err := parseUSDMap(w.Models)
	if err != nil {
		return db.FrictionPeriodSpend{}, err
	}
	return db.FrictionPeriodSpend{Total: total, Days: w.Days, Agents: agents, Models: models}, nil
}

func EncodeSnapshot(s Snapshot) ([]byte, error) {
	w := snapshotWire{
		Date: s.Date, Timezone: s.Timezone, RulesVersion: s.RulesVersion,
		Signals: make([]signalWire, 0, len(s.Signals)), P0Alerts: s.P0Alerts, SessionsScanned: s.SessionsScanned,
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

func DecodeSnapshot(b []byte) (Snapshot, error) {
	var w snapshotWire
	if err := json.Unmarshal(b, &w); err != nil {
		return Snapshot{}, fmt.Errorf("decoding friction snapshot: %w", err)
	}
	s := Snapshot{
		Date: w.Date, P0Alerts: w.P0Alerts, SessionsScanned: w.SessionsScanned, Timezone: w.Timezone, RulesVersion: w.RulesVersion,
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
				return Snapshot{}, fmt.Errorf("snapshot occurred_at: %w", err)
			}
			sig.OccurredAt = ts.UTC()
		}
		s.Signals = append(s.Signals, sig)
	}
	if sw := w.Spend; sw != nil {
		total, err := parseUSDPtr(sw.Total)
		if err != nil {
			return Snapshot{}, err
		}
		roles, err := parseUSDMap(sw.RoleCosts)
		if err != nil {
			return Snapshot{}, err
		}
		models, err := parseUSDMap(sw.ModelCosts)
		if err != nil {
			return Snapshot{}, err
		}
		s.Spend = &SpendSummary{
			Total: total, SessionsWithStats: sw.SessionsWithStats,
			SessionsWithCost: sw.SessionsWithCost, InputTokens: sw.InputTokens,
			OutputTokens: sw.OutputTokens, RoleCosts: roles, ModelCosts: models,
		}
	}
	if aw := w.ArchiveSpend; aw != nil {
		week, err := periodFromWire(aw.Week)
		if err != nil {
			return Snapshot{}, err
		}
		a := &db.FrictionArchiveSpendSummary{Week: week, WeekFrom: aw.WeekFrom, WeekTo: aw.WeekTo, Timezone: aw.Timezone}
		if aw.Yesterday != nil {
			y, err := periodFromWire(*aw.Yesterday)
			if err != nil {
				return Snapshot{}, err
			}
			a.Yesterday = &y
		}
		s.ArchiveSpend = a
	}
	return s, nil
}
