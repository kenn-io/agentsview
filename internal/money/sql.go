package money

import (
	"database/sql/driver"
	"fmt"
	"strconv"
)

// Value stores Money as an exact SQL INTEGER.
func (m Money) Value() (driver.Value, error) {
	return m.Microdollars, nil
}

// Scan reads an exact SQL INTEGER into Money. Floating-point database values
// are rejected so a stale schema cannot silently reintroduce rounding.
func (m *Money) Scan(src any) error {
	if m == nil {
		return fmt.Errorf("scanning money into nil destination")
	}
	switch value := src.(type) {
	case int64:
		m.Microdollars = value
		return nil
	case []byte:
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return fmt.Errorf("scanning money: %w", err)
		}
		m.Microdollars = parsed
		return nil
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("scanning money: %w", err)
		}
		m.Microdollars = parsed
		return nil
	default:
		return fmt.Errorf("scanning money from %T: integer required", src)
	}
}
