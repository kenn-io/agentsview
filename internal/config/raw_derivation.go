package config

import (
	"fmt"
	"strings"
)

// ValidateRawDerivation validates opt-in and bounded controls. Tenant binding
// remains enabled with the worker off so rollback preserves public identities.
func (p PGConfig) ValidateRawDerivation(requireAuth bool) error {
	if !p.RawDerivation && p.RawTenant == "" {
		return nil
	}
	if strings.TrimSpace(p.RawTenant) == "" || p.RawTenant != strings.TrimSpace(p.RawTenant) || len(p.RawTenant) > 128 {
		return fmt.Errorf("hosted raw projection requires raw_tenant")
	}
	if !requireAuth {
		return fmt.Errorf("hosted raw projection requires authentication")
	}
	if p.Schema == "" {
		return fmt.Errorf("hosted raw projection requires a configured schema")
	}
	for _, r := range p.Schema {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return fmt.Errorf("hosted raw projection schema is invalid")
		}
	}
	if p.RawPollSeconds < 0 || p.RawPollSeconds > 60 || p.RawAttemptSeconds < 0 || p.RawAttemptSeconds > 300 || p.RawMaxAttempts < 0 || p.RawMaxAttempts > 10 {
		return fmt.Errorf("hosted raw worker bounds: poll 1-60 seconds, attempt 1-300 seconds, attempts 1-10; zero selects defaults")
	}
	return nil
}
func (p PGConfig) RawWorkerBounds() (poll, attempt, attempts int) {
	poll, attempt, attempts = p.RawPollSeconds, p.RawAttemptSeconds, p.RawMaxAttempts
	if poll == 0 {
		poll = 5
	}
	if attempt == 0 {
		attempt = 60
	}
	if attempts == 0 {
		attempts = 5
	}
	return
}
