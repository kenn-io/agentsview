package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

func readConfigFile(t *testing.T, dir string) Config {
	t.Helper()
	var fileCfg Config
	if _, err := toml.DecodeFile(
		filepath.Join(dir, configFileName), &fileCfg,
	); err != nil {
		t.Fatalf("parsing config file: %v", err)
	}
	return fileCfg
}

func TestCursorSecret_GeneratedAndPersisted(t *testing.T) {
	dir := setupTestEnv(t)

	// First load: should generate a secret
	cfg1, err := LoadMinimal()
	if err != nil {
		t.Fatalf("first load failed: %v", err)
	}
	if cfg1.CursorSecret == "" {
		t.Fatal("cursor secret was not generated")
	}
	if cfg1.DataDir != dir {
		t.Fatalf(
			"DataDir = %q, want %q", cfg1.DataDir, dir,
		)
	}

	// Verify file existence and content
	fileCfg := readConfigFile(t, dir)

	if fileCfg.CursorSecret != cfg1.CursorSecret {
		t.Errorf(
			"file secret = %q, want %q",
			fileCfg.CursorSecret, cfg1.CursorSecret,
		)
	}

	// Second load: should read the same secret
	cfg2, err := LoadMinimal()
	if err != nil {
		t.Fatalf("second load failed: %v", err)
	}
	if cfg2.CursorSecret != cfg1.CursorSecret {
		t.Errorf(
			"second load got %q, want %q",
			cfg2.CursorSecret, cfg1.CursorSecret,
		)
	}
}

func TestCursorSecret_RegeneratedIfMissing(t *testing.T) {
	dir := setupTestEnv(t)

	initialContent := "cursor_secret = \"\"\n"
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(initialContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadMinimal()
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if cfg.CursorSecret == "" {
		t.Fatal("cursor secret should have been regenerated")
	}

	// Verify it was updated in the file
	fileCfg := readConfigFile(t, dir)
	if fileCfg.CursorSecret == "" {
		t.Error("cursor secret was not updated in the file")
	}
}

func TestCursorSecret_LoadErrorOnInvalidConfig(t *testing.T) {
	dir := setupTestEnv(t)

	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte("[invalid toml = ="), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := LoadMinimal()
	if err == nil {
		t.Fatal("expected error loading invalid config")
	}
}

func TestCursorSecret_PreservesOtherFields(t *testing.T) {
	dir := setupTestEnv(t)

	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte("github_token = \"my-token\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadMinimal()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.CursorSecret == "" {
		t.Error("cursor secret not generated")
	}
	if cfg.GithubToken != "my-token" {
		t.Errorf(
			"github_token = %q, want %q",
			cfg.GithubToken, "my-token",
		)
	}

	// Verify file content has both
	fileCfg := readConfigFile(t, dir)

	if fileCfg.CursorSecret == "" {
		t.Error("cursor_secret missing in file")
	}
	if fileCfg.GithubToken != "my-token" {
		t.Error("github_token lost/changed in file")
	}
}
