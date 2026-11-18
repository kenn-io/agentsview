package postgres

import (
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/stringutil"
)

const pgActivityDiskVersion = "pg-activity-inputs-2|"

type pgActivityDiskRow struct {
	SessionID, UsageSource, Model, ProviderID, Agent                          string
	ClaudeMessageID, ClaudeRequestID, SourceUUID, UsageDedupKey               string
	US, Cost, WebSearches                                                     int64
	Input, Output, CacheCreate, CacheRead, Reasoning                          int64
	Ordinal, CacheCreate1h                                                    int32
	OrdinalValid, CostValid, CopilotReported, PricingTimeAbsent, CostRecorded bool
}

type pgActivityDiskPart struct {
	Identity          string
	Revision          int64
	GapCap            float64
	Bounds            []int64
	Trimmed           []uint64
	Runs              []activity.SessionRunPiece
	Users, Assistants []int64
	Usage             []pgActivityDiskRow
	Candidate         bool
	Priced            *pgActivityDiskPriced
}

type pgActivityDiskPriced struct {
	RangeStart, RangeEnd, EffectiveEnd, Next time.Time
	PricingDigest                            string
	Block                                    []byte
	Rows                                     []pgActivityDiskPricedRow
}

type pgActivityDiskPricedRow struct {
	Attribution                                string
	Cost                                       int64
	Index, WebSearches                         int32
	Reported, Priced, Contributes, SessionCost bool
}

func pgActivityRowToDisk(r pgActivityUsageRow) pgActivityDiskRow {
	return pgActivityDiskRow{
		SessionID: r.sessionID.Value(), UsageSource: r.usageSource.Value(), Model: r.model.Value(), ProviderID: r.providerID.Value(), Agent: r.agent.Value(),
		ClaudeMessageID: r.claudeMessageID, ClaudeRequestID: r.claudeRequestID, SourceUUID: r.sourceUUID, UsageDedupKey: r.usageDedupKey,
		US: r.us, Cost: r.cost, WebSearches: r.webSearches, Input: r.input, Output: r.output, CacheCreate: r.cacheCreate, CacheRead: r.cacheRead, Reasoning: r.reasoning,
		Ordinal: r.ordinal, CacheCreate1h: r.cacheCreate1h, OrdinalValid: r.ordinalValid, CostValid: r.costValid, CopilotReported: r.copilotReported, PricingTimeAbsent: r.pricingTimeAbsent, CostRecorded: r.costRecorded,
	}
}

func (r pgActivityDiskRow) activityRow(intern stringutil.Interner) pgActivityUsageRow {
	return pgActivityUsageRow{
		sessionID: intern.Intern(r.SessionID), usageSource: intern.Intern(r.UsageSource), model: intern.Intern(r.Model), providerID: intern.Intern(r.ProviderID), agent: intern.Intern(r.Agent),
		claudeMessageID: r.ClaudeMessageID, claudeRequestID: r.ClaudeRequestID, sourceUUID: r.SourceUUID, usageDedupKey: r.UsageDedupKey,
		us: r.US, cost: r.Cost, webSearches: r.WebSearches, input: r.Input, output: r.Output, cacheCreate: r.CacheCreate, cacheRead: r.CacheRead, reasoning: r.Reasoning,
		ordinal: r.Ordinal, cacheCreate1h: r.CacheCreate1h, ordinalValid: r.OrdinalValid, costValid: r.CostValid, copilotReported: r.CopilotReported, pricingTimeAbsent: r.PricingTimeAbsent, costRecorded: r.CostRecorded,
	}
}

func (s *Store) saveActivityInputs() {
	if !s.activityDisk.Enabled() {
		return
	}
	for _, slot := range s.runtimeState().activityInputs.Snapshot() {
		if slot.Rows[0].identity == "" {
			continue
		}
		input := slot.Rows[0]
		meta := pgActivityDiskPart{Identity: input.identity, Revision: input.revision, GapCap: input.gapCap}
		for _, row := range input.usage {
			meta.Usage = append(meta.Usage, pgActivityRowToDisk(row))
		}
		if p := input.priced; p != nil {
			block, err := json.Marshal(p.block)
			if err == nil {
				meta.Priced = &pgActivityDiskPriced{RangeStart: p.rangeStart, RangeEnd: p.rangeEnd, EffectiveEnd: p.effectiveEnd, Next: p.next, PricingDigest: p.pricingDigest, Block: block}
				for _, row := range p.rows {
					meta.Priced.Rows = append(meta.Priced.Rows, pgActivityDiskPricedRow{row.attribution.Value(), row.cost, row.index, row.webSearches, row.reported, row.priced, row.contributes, row.sessionCost})
				}
			}
		}
		rows := make([]kept.Part[pgActivityDiskPart], 0, len(input.sessions)+1)
		rows = append(rows, kept.Part[pgActivityDiskPart]{Value: meta})
		for id, session := range input.sessions {
			bounds, trimmed, runs := session.runs.Pieces()
			rows = append(rows, kept.Part[pgActivityDiskPart]{ID: id, Value: pgActivityDiskPart{Bounds: bounds, Trimmed: trimmed, Runs: runs, Users: session.users, Assistants: session.assistants, Candidate: input.candidates[id]}})
		}
		save := func() error {
			data, err := kept.EncodeRows(rows, 256<<20, pgActivityDiskPartBytes)
			if err != nil {
				return err
			}
			return s.activityDisk.Put(slot.Slot, pgActivityDiskVersion+slot.Version, data)
		}
		if err := save(); errors.Is(err, kept.ErrLimitExceeded) && rows[0].Value.Priced != nil {
			rows[0].Value.Priced = nil
			_ = save()
		}
	}
}

func pgActivityDiskPartBytes(part kept.Part[pgActivityDiskPart]) int64 {
	size := int64(len(part.ID) + len(part.Value.Identity))
	size += int64(len(part.Value.Users)+len(part.Value.Assistants)) * 8
	size += int64(len(part.Value.Bounds)+len(part.Value.Trimmed)) * 8
	for _, run := range part.Value.Runs {
		size += int64(len(run.Model) + 8)
	}
	if priced := part.Value.Priced; priced != nil {
		size += int64(len(priced.Block) + len(priced.PricingDigest))
		for _, row := range priced.Rows {
			size += int64(len(row.Attribution) + 20)
		}
	}
	for _, row := range part.Value.Usage {
		// Cover gob field markers, string lengths, numeric values, and flags.
		size += 256
		size += int64(len(row.SessionID) + len(row.UsageSource) + len(row.Model) + len(row.ProviderID) + len(row.Agent) +
			len(row.ClaudeMessageID) + len(row.ClaudeRequestID) + len(row.SourceUUID) + len(row.UsageDedupKey))
	}
	return size
}

func (s *Store) loadActivityInputs(slot, version, identity string) (pgActivityInputs, bool) {
	parts := kept.LoadParts[pgActivityDiskPart](&s.activityDisk, slot, pgActivityDiskVersion+version)
	meta, ok := parts[""]
	if !ok || meta.Value.Identity != identity {
		return pgActivityInputs{}, false
	}
	delete(parts, "")
	input := pgActivityInputs{identity: identity, revision: meta.Value.Revision, gapCap: meta.Value.GapCap, sessions: map[string]pgActivitySession{}, candidates: map[string]bool{}}
	intern := make(stringutil.Interner)
	input.usage = make([]pgActivityUsageRow, 0, len(meta.Value.Usage))
	for _, usage := range meta.Value.Usage {
		input.usage = append(input.usage, usage.activityRow(intern))
	}
	for _, row := range parts {
		part := row.Value
		runs, err := activity.NewSessionRuns(part.Bounds, part.Trimmed, part.Runs)
		if err != nil {
			return pgActivityInputs{}, false
		}
		if part.Candidate {
			input.candidates[row.ID] = true
		}
		input.sessions[row.ID] = pgActivitySession{runs: runs, users: part.Users, assistants: part.Assistants}
	}
	if p := meta.Value.Priced; p != nil {
		var block *export.PricingBlock
		if json.Unmarshal(p.Block, &block) == nil {
			priced := &pgActivityPriced{rangeStart: p.RangeStart, rangeEnd: p.RangeEnd, effectiveEnd: p.EffectiveEnd, next: p.Next, pricingDigest: p.PricingDigest, block: block}
			seen := make(map[int32]bool, len(p.Rows))
			for _, row := range p.Rows {
				if row.Index < 0 || int(row.Index) >= len(input.usage) || seen[row.Index] {
					priced = nil
					break
				}
				seen[row.Index] = true
				priced.rows = append(priced.rows, pgActivityPricedRow{intern.Intern(row.Attribution), row.Cost, row.Index, row.WebSearches, row.Reported, row.Priced, row.Contributes, row.SessionCost})
			}
			input.priced = priced
		}
	}
	return input, true
}
