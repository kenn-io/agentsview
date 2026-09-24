package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"go.kenn.io/agentsview/internal/pathutil"
)

// FrictionNanoClawConfig is [friction.nanoclaw]. There is no default data
// directory: NanoClaw support is active only when the user sets one (D33).
type FrictionNanoClawConfig struct {
	DataDir string   `json:"data_dir,omitempty" toml:"data_dir"`
	DB      string   `json:"db,omitempty" toml:"db"`
	Include []string `json:"include,omitempty" toml:"include"`
	Exclude []string `json:"exclude,omitempty" toml:"exclude"`
}

// Active reports whether NanoClaw dimensions are configured.
func (c FrictionNanoClawConfig) Active() bool { return c.DataDir != "" }

// ResolvedPaths expands a leading ~ and makes both paths absolute. DB
// defaults to <data_dir>/v2.db (nanoclaw.rs:82).
func (c FrictionNanoClawConfig) ResolvedPaths() (dataDir, dbPath string, err error) {
	dataDir, err = absHomePath(c.DataDir)
	if err != nil {
		return "", "", fmt.Errorf("[friction.nanoclaw] data_dir: %w", err)
	}
	if c.DB == "" {
		return dataDir, filepath.Join(dataDir, "v2.db"), nil
	}
	dbPath, err = absHomePath(c.DB)
	if err != nil {
		return "", "", fmt.Errorf("[friction.nanoclaw] db: %w", err)
	}
	return dataDir, dbPath, nil
}

func absHomePath(p string) (string, error) {
	expanded, err := pathutil.ExpandHome(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}

// Validate checks [friction.nanoclaw].
func (c FrictionNanoClawConfig) Validate() error {
	if !c.Active() {
		switch {
		case c.DB != "":
			return errors.New("[friction.nanoclaw] db requires data_dir")
		case len(c.Include) > 0 || len(c.Exclude) > 0:
			return errors.New("[friction.nanoclaw] include and exclude require data_dir")
		}
		return nil
	}
	if slices.Contains(c.Include, "") {
		return errors.New("[friction.nanoclaw] include entries must be non-empty")
	}
	if slices.Contains(c.Exclude, "") {
		return errors.New("[friction.nanoclaw] exclude entries must be non-empty")
	}
	return nil
}

// mergeFrictionNanoClawTOML copies [friction.nanoclaw] when the file defines
// it, trimming whitespace like the rest of the [friction] merge. Empty list
// entries are kept so Validate can reject them.
func (c *Config) mergeFrictionNanoClawTOML(file FrictionConfig, meta toml.MetaData) {
	if !meta.IsDefined("friction", "nanoclaw") {
		return
	}
	trim := func(values []string) []string {
		if values == nil {
			return nil
		}
		out := make([]string, len(values))
		for i, v := range values {
			out[i] = strings.TrimSpace(v)
		}
		return out
	}
	c.Friction.NanoClaw = FrictionNanoClawConfig{
		DataDir: strings.TrimSpace(file.NanoClaw.DataDir),
		DB:      strings.TrimSpace(file.NanoClaw.DB),
		Include: trim(file.NanoClaw.Include),
		Exclude: trim(file.NanoClaw.Exclude),
	}
}
