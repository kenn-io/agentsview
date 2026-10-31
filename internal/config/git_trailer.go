package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"go.kenn.io/agentsview/internal/gittrailer"
)

// GitTrailerConfig is a client-only preference. Installing hooks is the opt-in;
// an absent enabled setting leaves installed hooks enabled.
type GitTrailerConfig struct {
	Enabled      bool              `toml:"enabled"`
	SessionIDEnv map[string]string `toml:"session_id_env"`
}

// LoadGitTrailer reads only git_trailer from config.toml. It does not construct
// the application config, migrate files, discover archives, or write anything.
func LoadGitTrailer() (GitTrailerConfig, error) {
	cfg := GitTrailerConfig{Enabled: true}
	dir, err := ResolveDataDir()
	if err != nil {
		return cfg, errors.New("cannot locate git trailer configuration")
	}
	path := filepath.Join(dir, "config.toml")
	// Check before opening: a FIFO must not hang the invoking Git commit.
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, errors.New("cannot inspect git trailer configuration")
	}
	const maxBytes = 2 << 20
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return cfg, errors.New("git trailer configuration must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return cfg, errors.New("cannot read git trailer configuration")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return cfg, errors.New("git trailer configuration changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || len(data) > maxBytes {
		return cfg, errors.New("cannot read bounded git trailer configuration")
	}
	decoded := struct {
		GitTrailer GitTrailerConfig `toml:"git_trailer"`
	}{GitTrailer: cfg}
	if _, err := toml.Decode(string(data), &decoded); err != nil {
		return cfg, errors.New("cannot parse git trailer configuration")
	}
	cfg = decoded.GitTrailer
	for agent := range cfg.SessionIDEnv {
		if !gittrailer.ValidAgent(agent) {
			return cfg, errors.New("invalid git_trailer.session_id_env agent key")
		}
	}
	return cfg, nil
}
