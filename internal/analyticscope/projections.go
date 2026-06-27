package analyticscope

// Stats aggregates matched rows into MessageStats. The caller groups rows by
// session before calling.
func Stats(rows []ScopedMessage) MessageStats {
	var s MessageStats
	for _, row := range rows {
		s.Messages++
		switch row.Role {
		case "user":
			if !row.IsSystem {
				s.UserMessages++
			}
		case "assistant":
			s.AssistantMessages++
			if row.HasToolUse {
				s.ToolUseMessages++
			}
		}
		if row.HasThinking {
			s.ThinkingMessages++
		}
		if row.HasOutputTokens {
			s.OutputTokens += row.OutputTokens
			s.HasOutputTokens = true
		}
	}
	return s
}

// Timing projects matched rows into the velocity timing view, preserving order.
func Timing(rows []ScopedMessage) []TimingMessage {
	out := make([]TimingMessage, 0, len(rows))
	for _, row := range rows {
		out = append(out, TimingMessage{
			Role:          row.Role,
			Time:          row.LocalTime,
			Valid:         row.HasLocalTime,
			ContentLength: row.ContentLength,
		})
	}
	return out
}
