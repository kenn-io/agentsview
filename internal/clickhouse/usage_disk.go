package clickhouse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"io"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/db"
)

const usageDiskVersion = "usage-1|"

type usageGroupWire struct {
	Date                  string
	Ts                    string
	PricingTS             string
	SessionID             string
	Project               string
	Agent                 string
	Machine               string
	Model                 string
	ProviderID            string
	PriceModel            string
	Source                string
	MessageOrdinal        sql.NullInt64
	InputTok              int
	OutputTok             int
	CacheCr               int
	CacheCr1h             int
	CacheRd               int
	BillableInput         int
	BillableOutput        int
	BillableReason        int
	BillableCacheCr       int
	BillableCacheCr1h     int
	BillableCacheRd       int
	BillableWebSearch     int
	ExplicitCost          int64
	ReportedCostRows      int
	AuthoritativeCost     int64
	AuthoritativeCostRows int
	Explicit              bool
	Kind                  string
	ContextID             string
	BandAbove             int64
	Events                int
	TokenCost             int64
	Savings               int64
	PriceError            string
	Overflow              bool
	InexactModels         []string
	InexactProjects       []string
}

func usageGroupToWire(r chDailyUsageGroupRow) usageGroupWire {
	return usageGroupWire{
		Date:                  r.date,
		Ts:                    r.ts,
		PricingTS:             r.pricingTS,
		SessionID:             r.sessionID,
		Project:               r.project,
		Agent:                 r.agent,
		Machine:               r.machine,
		Model:                 r.model,
		ProviderID:            r.providerID,
		PriceModel:            r.priceModel,
		Source:                r.source,
		MessageOrdinal:        r.messageOrdinal,
		InputTok:              r.inputTok,
		OutputTok:             r.outputTok,
		CacheCr:               r.cacheCr,
		CacheCr1h:             r.cacheCr1h,
		CacheRd:               r.cacheRd,
		BillableInput:         r.billableInput,
		BillableOutput:        r.billableOutput,
		BillableReason:        r.billableReason,
		BillableCacheCr:       r.billableCacheCr,
		BillableCacheCr1h:     r.billableCacheCr1h,
		BillableCacheRd:       r.billableCacheRd,
		BillableWebSearch:     r.billableWebSearch,
		ExplicitCost:          r.explicitCost,
		ReportedCostRows:      r.reportedCostRows,
		AuthoritativeCost:     r.authoritativeCost,
		AuthoritativeCostRows: r.authoritativeCostRows,
		Explicit:              r.explicit,
		Kind:                  r.kind,
		ContextID:             r.contextID,
		BandAbove:             r.bandAbove,
		Events:                r.events,
		TokenCost:             r.tokenCost,
		Savings:               r.savings,
		PriceError:            r.priceError,
		Overflow:              r.overflow,
		InexactModels:         r.inexactModels,
		InexactProjects:       r.inexactProjects,
	}
}

func (w usageGroupWire) row() chDailyUsageGroupRow {
	var r chDailyUsageGroupRow
	r.date = w.Date
	r.ts = w.Ts
	r.pricingTS = w.PricingTS
	r.sessionID = w.SessionID
	r.project = w.Project
	r.agent = w.Agent
	r.machine = w.Machine
	r.model = w.Model
	r.providerID = w.ProviderID
	r.priceModel = w.PriceModel
	r.source = w.Source
	r.messageOrdinal = w.MessageOrdinal
	r.inputTok = w.InputTok
	r.outputTok = w.OutputTok
	r.cacheCr = w.CacheCr
	r.cacheCr1h = w.CacheCr1h
	r.cacheRd = w.CacheRd
	r.billableInput = w.BillableInput
	r.billableOutput = w.BillableOutput
	r.billableReason = w.BillableReason
	r.billableCacheCr = w.BillableCacheCr
	r.billableCacheCr1h = w.BillableCacheCr1h
	r.billableCacheRd = w.BillableCacheRd
	r.billableWebSearch = w.BillableWebSearch
	r.explicitCost = w.ExplicitCost
	r.reportedCostRows = w.ReportedCostRows
	r.authoritativeCost = w.AuthoritativeCost
	r.authoritativeCostRows = w.AuthoritativeCostRows
	r.explicit = w.Explicit
	r.kind = w.Kind
	r.contextID = w.ContextID
	r.bandAbove = w.BandAbove
	r.events = w.Events
	r.tokenCost = w.TokenCost
	r.savings = w.Savings
	r.priceError = w.PriceError
	r.overflow = w.Overflow
	r.inexactModels = w.InexactModels
	r.inexactProjects = w.InexactProjects
	return r
}

func encodeUsageGroups(rows []chDailyUsageGroupRow) ([]byte, bool) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(len(rows)); err != nil {
		return nil, false
	}
	for _, row := range rows {
		if err := enc.Encode(usageGroupToWire(row)); err != nil || buf.Len() > dailyUsageRowsBytes {
			return nil, false
		}
	}
	return buf.Bytes(), true
}

func decodeUsageGroups(data []byte) ([]chDailyUsageGroupRow, bool) {
	dec := gob.NewDecoder(bytes.NewReader(data))
	var count int
	if err := dec.Decode(&count); err != nil || count < 0 || count > dailyUsageRowsBytes/int(unsafe.Sizeof(chDailyUsageGroupRow{})) {
		return nil, false
	}
	var rows []chDailyUsageGroupRow
	var size int64
	for range count {
		var wire usageGroupWire
		if err := dec.Decode(&wire); err != nil {
			return nil, false
		}
		row := wire.row()
		size += dailyUsageRowBytes(row)
		if size > dailyUsageRowsBytes {
			return nil, false
		}
		rows = append(rows, row)
	}
	var extra usageGroupWire
	return rows, dec.Decode(&extra) == io.EOF
}

// usageDiskVersionAt captures a fresh parts snapshot and the logical rows it names.
func (s *Store) usageDiskVersionAt(ctx context.Context, pricingDigest string) (string, bool) {
	ctx, err := s.withPartsSnapshot(ctx)
	if err != nil {
		return "", false
	}
	state, err := s.preparedUsageState(ctx)
	if err != nil || !state.ready || len(state.raw) != 0 {
		return "", false
	}
	version, err := s.usageReadFingerprint(ctx, state, pricingDigest)
	if err != nil || version == "" {
		return "", false
	}
	return version, true
}

// usageDiskFilter drops the end date only when no source has later usage.
func (s *Store) usageDiskFilter(ctx context.Context, state preparedUsageState, f db.UsageFilter) db.UsageFilter {
	if f.To == "" || f.Timezone == "" {
		return f
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return f
	}
	_, end, err := db.FrictionDayBounds(f.To, loc)
	if err != nil {
		return f
	}
	readCtx, err := withUsageDeltaTables(ctx, state)
	if err != nil {
		return f
	}
	bound := end.UTC().Format(time.RFC3339Nano)
	source, args := chPreparedUsageSourceSQL(state, "ts >= "+chTimestampSQL, []any{bound})
	args = append(args, bound)
	var future bool
	err = s.queryRowContext(readCtx, "SELECT EXISTS(SELECT 1 FROM ("+source+") LIMIT 1) OR EXISTS(SELECT 1 FROM cursor_usage_events WHERE occurred_at >= "+chTimestampSQL+" LIMIT 1)", args...).Scan(&future)
	if err == nil && !future {
		f.To = ""
	}
	return f
}
