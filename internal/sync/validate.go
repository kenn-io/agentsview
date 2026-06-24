package sync

import (
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const (
	maxModelLen        = db.MaxModelLen
	maxPlausibleTokens = db.MaxPlausibleTokens
)

type validationStats = db.ValidationStats

func validateAndSanitize(
	s *db.Session, msgs []db.Message, events []db.UsageEvent,
) validationStats {
	return db.ValidateAndSanitize(s, msgs, events)
}

func sanitizeMessage(m *db.Message) validationStats {
	return db.SanitizeMessage(m)
}

func sanitizeUsageEvent(ev *db.UsageEvent) validationStats {
	return db.SanitizeUsageEvent(ev)
}

func sanitizeSession(s *db.Session) validationStats {
	return db.SanitizeSession(s)
}

func clampModel(p *string) bool {
	return db.ClampModel(p)
}

func clampedTokens(v int) int {
	return db.ClampParsedTokens(v)
}

func blankImplausibleTimestampPtr(p *string) (*string, bool) {
	return db.BlankImplausibleTimestampPtr(p)
}

func parseStoredTimestamp(s string) (time.Time, bool) {
	return db.ParseStoredTimestamp(s)
}
