package config

import (
	"fmt"
	"os"
)

// ValidationScope selects the sections consumed by a command. Zero validates
// everything, including deployment environment, for server startup and reloads.
// Parsing and unknown-key handling are independent of this scope.
type ValidationScope uint16

const (
	ScopeLocal ValidationScope = 1 << iota
	ScopeServer
	ScopeVector
	ScopeRecall
	ScopeInsights
	ScopePG
	ScopeClickHouse
	ScopeDeployment
)

func (s ValidationScope) uses(section ValidationScope) bool {
	return s == 0 || s&section != 0
}

func (c *Config) validateSection(scope ValidationScope, section string, validate func() error) error {
	if err := validate(); err != nil {
		if c.validationScope.uses(scope) {
			return err
		}
		fmt.Fprintf(os.Stderr, "warning: invalid %s configuration: %v\n", section, err)
	}
	return nil
}

func (c *Config) validateSections() error {
	for _, section := range []struct {
		scope    ValidationScope
		name     string
		validate func() error
	}{
		{ScopeServer, "proxy", func() error { return normalizeProxyConfig(&c.Proxy) }},
		// The public URL joins the allowed origins, so one bad value is one warning.
		{ScopeServer, "public_url/public_origins", c.resolvePublicConfig},
		{ScopeServer, "daemon_idle_timeout", func() error {
			if c.DaemonIdleTimeout < 0 {
				return fmt.Errorf("invalid daemon_idle_timeout: %s", c.DaemonIdleTimeout)
			}
			return nil
		}},
		{ScopeVector, "[vector]", c.Vector.Validate},
		{ScopeRecall, "[recall.extract]", c.Recall.Extract.Validate},
		{ScopeInsights, "[insights]", c.Insights.Validate},
		{ScopeServer, "[friction]", c.Friction.Validate},
		{ScopeServer, "[kata]", c.Kata.Validate},
		{ScopePG, "[pg]", func() error { return c.pgConfigErr }},
		{ScopeClickHouse, "[clickhouse]", func() error { return c.clickHouseConfigErr }},
	} {
		if err := c.validateSection(section.scope, section.name, section.validate); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) resolvePublicConfig() error {
	value, err := resolvePublicURL(c.PublicURL, c.Proxy)
	if err != nil {
		return fmt.Errorf("invalid public url: %w", err)
	}
	c.PublicURL = value
	origins, err := normalizePublicOrigins(c.PublicOrigins)
	if err != nil {
		return fmt.Errorf("invalid public origins: %w", err)
	}
	if c.PublicURL != "" {
		origins, err = normalizePublicOrigins(append(origins, c.PublicURL))
		if err != nil {
			return fmt.Errorf("invalid public url: %w", err)
		}
	}
	c.PublicOrigins = origins
	return nil
}

// targetValidationError distinguishes invalid target layouts from TOML syntax,
// unknown fields and type errors, which remain fatal for every command.
type targetValidationError struct{ error }

func invalidTargetLayout(format string, args ...any) error {
	return &targetValidationError{fmt.Errorf(format, args...)}
}
