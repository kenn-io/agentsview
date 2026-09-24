package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// FrictionKataConfig is the Friction Log filing policy. It takes effect only
// on the agentsview filing hub with [kata] enabled (spec §13.0).
type FrictionKataConfig struct {
	AutoFile           bool     `json:"-" toml:"auto_file"`
	ReopenOnRecurrence bool     `json:"-" toml:"reopen_on_recurrence"`
	Kinds              []string `json:"-" toml:"kinds"`
}

var frictionFileableKinds = []string{"correction", "error", "workaround", "deferral", "pattern", "frustration", "interruption"}

// DefaultFrictionKataConfig keeps jilog's five kinds; frustration and
// interruption are opt-in (spec §6.8).
func DefaultFrictionKataConfig() FrictionKataConfig {
	return FrictionKataConfig{ReopenOnRecurrence: true, Kinds: []string{"correction", "error", "workaround", "deferral", "pattern"}}
}

func (c FrictionKataConfig) Validate() error {
	seen := map[string]bool{}
	for _, k := range c.Kinds {
		if !slices.Contains(frictionFileableKinds, k) {
			return fmt.Errorf("[friction.kata] kinds entry %q is not a friction kind", k)
		}
		if seen[k] {
			return fmt.Errorf("[friction.kata] kinds entry %q repeats", k)
		}
		seen[k] = true
	}
	return nil
}

func mergeFrictionKataTOML(dst *FrictionKataConfig, src FrictionKataConfig, meta toml.MetaData) {
	if meta.IsDefined("friction", "kata", "auto_file") {
		dst.AutoFile = src.AutoFile
	}
	if meta.IsDefined("friction", "kata", "reopen_on_recurrence") {
		dst.ReopenOnRecurrence = src.ReopenOnRecurrence
	}
	if meta.IsDefined("friction", "kata", "kinds") {
		kinds := make([]string, 0, len(src.Kinds))
		for _, k := range src.Kinds {
			kinds = append(kinds, strings.TrimSpace(k))
		}
		dst.Kinds = kinds
	}
}
