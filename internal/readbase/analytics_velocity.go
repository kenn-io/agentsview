package readbase

import (
	"context"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type velocitySession struct {
	agent string
	mc    int
}

type velocityAccumulator struct {
	turnCycles     []float64
	firstResponses []float64
	totalMsgs      int
	totalChars     int
	totalToolCalls int
	activeMinutes  float64
	sessions       int
}

func processSessionVelocity(
	accums []*velocityAccumulator,
	msgs []db.TimingMessage,
	toolCount int,
) {
	const maxCycleSec = 1800.0
	// Top-session SQL uses the same gap cap for active duration.
	const maxGapSec = db.ActiveGapCapSec

	for _, acc := range accums {
		acc.sessions++
	}
	for i := 1; i < len(msgs); i++ {
		prev := msgs[i-1]
		cur := msgs[i]
		if !prev.Valid || !cur.Valid {
			continue
		}
		if prev.Role == "user" && cur.Role == "assistant" {
			delta := cur.Time.Sub(prev.Time).Seconds()
			if delta > 0 && delta <= maxCycleSec {
				for _, acc := range accums {
					acc.turnCycles = append(acc.turnCycles, delta)
				}
			}
		}
	}

	var firstUser, firstAsst *db.TimingMessage
	firstUserIdx := -1
	for i := range msgs {
		if msgs[i].Role == "user" && msgs[i].Valid {
			firstUser = &msgs[i]
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx >= 0 {
		for i := firstUserIdx + 1; i < len(msgs); i++ {
			if msgs[i].Role == "assistant" && msgs[i].Valid {
				firstAsst = &msgs[i]
				break
			}
		}
	}
	if firstUser != nil && firstAsst != nil {
		delta := firstAsst.Time.Sub(firstUser.Time).Seconds()
		if delta < 0 {
			delta = 0
		}
		for _, acc := range accums {
			acc.firstResponses = append(acc.firstResponses, delta)
		}
	}

	activeSec := 0.0
	assistantChars := 0
	for i, msg := range msgs {
		if msg.Role == "assistant" {
			assistantChars += msg.ContentLength
		}
		if i > 0 && msgs[i-1].Valid && msg.Valid {
			gap := msg.Time.Sub(msgs[i-1].Time).Seconds()
			if gap > 0 {
				if gap > maxGapSec {
					gap = maxGapSec
				}
				activeSec += gap
			}
		}
	}
	activeMinutes := activeSec / 60
	if activeMinutes > 0 {
		for _, acc := range accums {
			acc.totalMsgs += len(msgs)
			acc.totalChars += assistantChars
			acc.totalToolCalls += toolCount
			acc.activeMinutes += activeMinutes
		}
	}
}

func (a *velocityAccumulator) computeOverview() db.VelocityOverview {
	sort.Float64s(a.turnCycles)
	sort.Float64s(a.firstResponses)

	out := db.VelocityOverview{}
	out.TurnCycleSec = db.Percentiles{
		P50: db.Round1(db.PercentileFloat(a.turnCycles, 0.5)),
		P90: db.Round1(db.PercentileFloat(a.turnCycles, 0.9)),
	}
	out.FirstResponseSec = db.Percentiles{
		P50: db.Round1(db.PercentileFloat(a.firstResponses, 0.5)),
		P90: db.Round1(db.PercentileFloat(a.firstResponses, 0.9)),
	}
	if a.activeMinutes > 0 {
		out.MsgsPerActiveMin = db.Round1(float64(a.totalMsgs) / a.activeMinutes)
		out.CharsPerActiveMin = db.Round1(float64(a.totalChars) / a.activeMinutes)
		out.ToolCallsPerActiveMin = db.Round1(float64(a.totalToolCalls) / a.activeMinutes)
	}
	return out
}

func (s *Analytics) GetAnalyticsVelocity(
	ctx context.Context, f db.AnalyticsFilter,
) (db.VelocityResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.VelocityResponse{}, err
	}
	if len(sessions) == 0 {
		return db.VelocityResponse{
			ByAgent:      []db.VelocityBreakdown{},
			ByComplexity: []db.VelocityBreakdown{},
		}, nil
	}

	sessionIDs := make([]string, 0, len(sessions))
	sessionInfo := make(map[string]velocitySession, len(sessions))
	for _, sess := range sessions {
		sessionIDs = append(sessionIDs, sess.ID)
		sessionInfo[sess.ID] = velocitySession{
			agent: sess.Agent,
			mc:    sess.MessageCount,
		}
	}
	if strings.TrimSpace(f.Model) != "" {
		stats, err := s.getAnalyticsFilteredMessageStats(
			ctx, sessionIDs, f,
		)
		if err != nil {
			return db.VelocityResponse{}, err
		}
		for _, sid := range sessionIDs {
			info := sessionInfo[sid]
			info.mc = stats[sid].Messages
			sessionInfo[sid] = info
		}
	}

	var sessionMsgs map[string][]db.TimingMessage
	if strings.TrimSpace(f.Model) != "" {
		sessionMsgs, err = s.filteredVelocityMessages(
			ctx, sessionIDs, f,
		)
	} else {
		sessionMsgs, err = s.backend.VelocityMessages(
			ctx, sessionIDs, f, AnalyticsLocation(f.Timezone),
		)
	}
	if err != nil {
		return db.VelocityResponse{}, err
	}
	var toolCounts map[string]int
	if strings.TrimSpace(f.Model) != "" {
		toolCounts, err = s.filteredToolCounts(
			ctx, sessionIDs, f,
		)
	} else {
		toolCounts, err = s.backend.VelocityToolCounts(ctx, sessionIDs, f)
	}
	if err != nil {
		return db.VelocityResponse{}, err
	}

	overall := &velocityAccumulator{}
	byAgent := make(map[string]*velocityAccumulator)
	byComplexity := make(map[string]*velocityAccumulator)
	for _, sid := range sessionIDs {
		msgs := sessionMsgs[sid]
		if len(msgs) < 2 {
			continue
		}
		info := sessionInfo[sid]
		agentKey := info.agent
		compKey := db.ComplexityBucket(info.mc)
		if byAgent[agentKey] == nil {
			byAgent[agentKey] = &velocityAccumulator{}
		}
		if byComplexity[compKey] == nil {
			byComplexity[compKey] = &velocityAccumulator{}
		}
		processSessionVelocity(
			[]*velocityAccumulator{overall, byAgent[agentKey], byComplexity[compKey]},
			msgs,
			toolCounts[sid],
		)
	}

	resp := db.VelocityResponse{
		Overall:      overall.computeOverview(),
		ByAgent:      []db.VelocityBreakdown{},
		ByComplexity: []db.VelocityBreakdown{},
	}
	for _, key := range db.SortedKeys(byAgent) {
		acc := byAgent[key]
		if acc == nil {
			continue
		}
		resp.ByAgent = append(resp.ByAgent, db.VelocityBreakdown{
			Label:    key,
			Sessions: acc.sessions,
			Overview: acc.computeOverview(),
		})
	}

	compOrder := map[string]int{"1-15": 0, "16-60": 1, "61+": 2}
	compKeys := db.SortedKeys(byComplexity)
	sort.Slice(compKeys, func(i, j int) bool {
		return compOrder[compKeys[i]] < compOrder[compKeys[j]]
	})
	for _, key := range compKeys {
		acc := byComplexity[key]
		if acc == nil {
			continue
		}
		resp.ByComplexity = append(resp.ByComplexity, db.VelocityBreakdown{
			Label:    key,
			Sessions: acc.sessions,
			Overview: acc.computeOverview(),
		})
	}
	return resp, nil
}

func (s *Analytics) filteredVelocityMessages(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string][]db.TimingMessage, error) {
	out := make(map[string][]db.TimingMessage, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}

	scope, err := s.backend.MessageScope(ctx, sessionIDs, f, false)
	if err != nil {
		return nil, err
	}
	if scope == nil {
		return out, nil
	}
	return scope.TimingBySession(), nil
}
