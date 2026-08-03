// Package bunmodel defines the persistence-only models shared by the storage
// adapters. Parser state, adapter bookkeeping, and API presentation fields do
// not belong here.
package bunmodel

import (
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// Timestamp is the canonical persistence timestamp. It accepts the native
// time values returned by PostgreSQL and DuckDB as well as the text forms
// already stored by SQLite, and normalizes every non-null value to UTC.
type Timestamp struct {
	time.Time
}

var timestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
}

// NewTimestamp returns a canonical UTC timestamp.
func NewTimestamp(value time.Time) Timestamp {
	return Timestamp{Time: value.UTC()}
}

// ParseTimestamp parses a supported persistent timestamp representation.
func ParseTimestamp(value string) (Timestamp, error) {
	for _, layout := range timestampLayouts {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return NewTimestamp(parsed), nil
		}
	}
	return Timestamp{}, fmt.Errorf("unsupported timestamp %q", value)
}

// Scan implements sql.Scanner.
func (t *Timestamp) Scan(src any) error {
	switch value := src.(type) {
	case nil:
		t.Time = time.Time{}
		return nil
	case time.Time:
		t.Time = value.UTC()
		return nil
	case string:
		parsed, err := ParseTimestamp(value)
		if err != nil {
			return err
		}
		*t = parsed
		return nil
	case []byte:
		return t.Scan(string(value))
	default:
		return fmt.Errorf("scanning timestamp from %T", src)
	}
}

// Value implements driver.Valuer.
func (t Timestamp) Value() (driver.Value, error) {
	return t.UTC(), nil
}

// ModelPricing is one model-pattern pricing row.
type ModelPricing struct {
	bun.BaseModel `bun:"table:model_pricing"`

	ModelPattern                     string `bun:"model_pattern,pk"`
	InputMicrodollarsPerMTok         int64  `bun:"input_microdollars_per_mtok,notnull,default:0"`
	OutputMicrodollarsPerMTok        int64  `bun:"output_microdollars_per_mtok,notnull,default:0"`
	CacheCreationMicrodollarsPerMTok int64  `bun:"cache_creation_microdollars_per_mtok,notnull,default:0"`
	CacheReadMicrodollarsPerMTok     int64  `bun:"cache_read_microdollars_per_mtok,notnull,default:0"`
	UpdatedAt                        string `bun:"updated_at,notnull,default:''"`
}

// ModelPricingBand overrides pricing above one input-token threshold.
type ModelPricingBand struct {
	bun.BaseModel `bun:"table:model_pricing_bands"`

	ModelPattern                     string `bun:"model_pattern,pk"`
	AboveInputTokens                 int64  `bun:"above_input_tokens,pk"`
	InputMicrodollarsPerMTok         int64  `bun:"input_microdollars_per_mtok,notnull,default:0"`
	OutputMicrodollarsPerMTok        int64  `bun:"output_microdollars_per_mtok,notnull,default:0"`
	CacheCreationMicrodollarsPerMTok int64  `bun:"cache_creation_microdollars_per_mtok,notnull,default:0"`
	CacheReadMicrodollarsPerMTok     int64  `bun:"cache_read_microdollars_per_mtok,notnull,default:0"`
	UpdatedAt                        string `bun:"updated_at,notnull,default:''"`
}
