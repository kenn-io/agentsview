package testutil

// TrendsSessionWindowCase describes one session and its date-filtered count.
type TrendsSessionWindowCase struct {
	Project, Started, Created, Timestamp, Timezone string
	Model                                          string
	Hour, Want                                     int
}

// TrendsSessionWindowCases supplies the shared SQLite and mirror fixture.
func TrendsSessionWindowCases() []TrendsSessionWindowCase {
	return []TrendsSessionWindowCase{
		{Project: "old", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-01-01T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 0},
		{Project: "resumed", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 1},
		{Project: "resumed-model", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T09:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 9, Want: 1},
		{Project: "pending", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T20:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 20, Want: 1},
		{Project: "pending-cleared", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T20:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 20, Want: 0},
		{Project: "null-message", Started: "2024-06-05T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 1},
		{Project: "null-old", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 0},
		{Project: "null-old-created-recent", Started: "2024-01-01T09:00:00Z", Created: "2024-06-05T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 0},
		{Project: "imported", Started: "2024-01-01T09:00:00Z", Created: "2024-06-05T09:00:00Z", Timestamp: "2024-01-01T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 0},
		{Project: "empty-start", Created: "2024-06-05T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 1},
		{Project: "east-edge", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-04T10:00:00Z", Timezone: "Pacific/Kiritimati", Hour: 0, Want: 1},
		{Project: "west-edge", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-06T11:59:59Z", Timezone: "Etc/GMT+12", Hour: 23, Want: 1},
	}
}

// TrendsWindowMessage supplies chronology and text shared by all backend tests.
type TrendsWindowMessage struct {
	Role, Content, Timestamp, Model string
}

func (c TrendsSessionWindowCase) Messages() []TrendsWindowMessage {
	messages := []TrendsWindowMessage{{Role: "user", Content: "seam", Timestamp: c.Timestamp}}
	if c.Project == "resumed" || c.Project == "resumed-model" {
		messages = append([]TrendsWindowMessage{{Role: "user", Content: "seam seam", Timestamp: c.Started}}, messages...)
	}
	if c.Project == "pending-cleared" {
		messages = append(messages, TrendsWindowMessage{Role: "assistant", Content: "seam seam", Timestamp: "2024-06-06T15:00:00Z", Model: "other"})
	}
	if c.Model != "" {
		messages = append(messages, TrendsWindowMessage{Role: "assistant", Content: "seam seam", Timestamp: "2024-06-07T15:00:00Z", Model: c.Model})
	}
	return messages
}
