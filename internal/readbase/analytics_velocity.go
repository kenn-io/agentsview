package readbase

import (
	"context"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

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
	sessionInfo := make(map[string]db.VelocitySession, len(sessions))
	for _, sess := range sessions {
		sessionIDs = append(sessionIDs, sess.ID)
		sessionInfo[sess.ID] = db.VelocitySession{
			Agent:        sess.Agent,
			MessageCount: sess.MessageCount,
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
			info.MessageCount = stats[sid].Messages
			sessionInfo[sid] = info
		}
	}

	var sessionMsgs map[string][]db.TimingMessage
	if strings.TrimSpace(f.Model) != "" {
		scope, err := s.ResolveMessageScope(ctx, sessionIDs, f, false)
		if err != nil {
			return db.VelocityResponse{}, err
		}
		sessionMsgs = scope.TimingBySession()
	} else {
		sessionMsgs, err = s.backend.VelocityMessages(
			ctx, f, AnalyticsLocation(f.Timezone),
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
		toolCounts, err = s.backend.VelocityToolCounts(ctx, f)
	}
	if err != nil {
		return db.VelocityResponse{}, err
	}

	return db.BuildVelocityResponse(sessionIDs, sessionInfo, sessionMsgs, toolCounts), nil
}
