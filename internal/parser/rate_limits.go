package parser

import "time"

// RateLimitSnapshot keeps the windows from one provider observation together.
type RateLimitSnapshot struct {
	ObservedAt time.Time         `json:"observed_at"`
	Ordinal    int64             `json:"-"`
	LimitID    string            `json:"limit_id"`
	LimitName  string            `json:"limit_name,omitempty"`
	Primary    *RateLimitWindow  `json:"primary,omitempty"`
	Secondary  *RateLimitWindow  `json:"secondary,omitempty"`
	Credits    *RateLimitCredits `json:"credits,omitempty"`
}

type RateLimitWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes *int64   `json:"window_minutes"`
	ResetsAt      *int64   `json:"resets_at"`
}

type RateLimitCredits struct {
	HasCredits *bool   `json:"has_credits"`
	Unlimited  *bool   `json:"unlimited"`
	Balance    *string `json:"balance"`
}
