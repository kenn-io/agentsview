package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFrictionConfigTOMLLoad(t *testing.T) {
	assert.False(t, loadMinimalWithConfig(t, map[string]any{}).Friction.Enabled,
		"friction review is opt-in")
	cfg := loadMinimalWithConfig(t, map[string]any{
		"friction": map[string]any{"enabled": true},
	})
	assert.True(t, cfg.Friction.Enabled)
}
