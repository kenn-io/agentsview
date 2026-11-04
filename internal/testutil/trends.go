package testutil

// TrendsSessionWindowCase describes one session and its date-filtered count.
type TrendsSessionWindowCase struct {
	Project, Started, Created, Timestamp, Timezone string
	Model                                          string
	Hour, Want                                     int
	Before, After                                  []TrendsWindowMessage
}

// TrendsSessionWindowCases supplies the shared SQLite and mirror fixture.
func TrendsSessionWindowCases() []TrendsSessionWindowCase {
	return []TrendsSessionWindowCase{
		{Project: "old", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-01-01T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 0},
		{Project: "resumed", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 1, Before: []TrendsWindowMessage{{Role: "user", Content: "seam seam", Timestamp: "2024-01-01T09:00:00Z"}}},
		{Project: "resumed-model", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T09:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 9, Want: 1, Before: []TrendsWindowMessage{{Role: "user", Content: "seam seam", Timestamp: "2024-01-01T09:00:00Z"}}, After: []TrendsWindowMessage{{Role: "assistant", Content: "seam seam", Timestamp: "2024-06-07T15:00:00Z", Model: "gpt-4o"}}},
		{Project: "pending", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T20:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 20, Want: 1, After: []TrendsWindowMessage{{Role: "assistant", Content: "seam seam", Timestamp: "2024-06-07T15:00:00Z", Model: "gpt-4o"}}},
		{Project: "pending-cleared", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T20:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 20, Want: 0, After: []TrendsWindowMessage{{Role: "assistant", Content: "seam seam", Timestamp: "2024-06-06T15:00:00Z", Model: "other"}, {Role: "assistant", Content: "seam seam", Timestamp: "2024-06-07T15:00:00Z", Model: "gpt-4o"}}},
		{Project: "old-assistants-null-reply", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T09:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 9, Want: 1,
			Before: []TrendsWindowMessage{
				{Role: "assistant", Content: "seam seam", Timestamp: "2024-01-01T09:00:00Z", Model: "other"},
				{Role: "assistant", Content: "seam seam", Timestamp: "2024-01-01T10:00:00Z", Model: "gpt-4o"},
			},
			After: []TrendsWindowMessage{{Role: "assistant", Content: "seam seam", Model: "gpt-4o"}}},
		{Project: "old-assistants-null-clears", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timestamp: "2024-06-05T09:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 9, Want: 0,
			Before: []TrendsWindowMessage{{Role: "assistant", Content: "seam seam", Timestamp: "2024-01-01T09:00:00Z", Model: "gpt-4o"}},
			After: []TrendsWindowMessage{
				{Role: "assistant", Content: "seam seam", Model: "other"},
				{Role: "assistant", Content: "seam seam", Timestamp: "2024-06-07T15:00:00Z", Model: "gpt-4o"},
			}},
		{Project: "null-message", Started: "2024-06-05T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 1},
		{Project: "null-message-model", Started: "2024-06-05T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 9, Want: 1, After: []TrendsWindowMessage{{Role: "assistant", Content: "seam seam", Timestamp: "2024-06-07T15:00:00Z", Model: "gpt-4o"}}},
		{Project: "null-old", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timezone: "UTC", Hour: 9, Want: 0},
		{Project: "null-old-model", Started: "2024-01-01T09:00:00Z", Created: "2024-01-01T09:00:00Z", Timezone: "UTC", Model: "gpt-4o", Hour: 9, Want: 0, After: []TrendsWindowMessage{{Role: "assistant", Content: "seam seam", Timestamp: "", Model: "gpt-4o"}}},
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
	messages := append([]TrendsWindowMessage(nil), c.Before...)
	messages = append(messages, TrendsWindowMessage{Role: "user", Content: "seam", Timestamp: c.Timestamp})
	return append(messages, c.After...)
}
