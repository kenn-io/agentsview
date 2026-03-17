package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestParseSyncFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
		check   func(t *testing.T, cfg SyncConfig)
	}{
		{
			name: "defaults",
			args: []string{},
			check: func(t *testing.T, cfg SyncConfig) {
				t.Helper()
				if cfg.Full {
					t.Error("Full should be false by default")
				}
			},
		},
		{
			name: "full flag",
			args: []string{"-full"},
			check: func(t *testing.T, cfg SyncConfig) {
				t.Helper()
				if !cfg.Full {
					t.Error("Full should be true")
				}
			},
		},
		{
			name:    "unexpected positional args",
			args:    []string{"full"},
			wantErr: "unexpected arguments",
		},
		{
			name:    "unknown flag",
			args:    []string{"--bogus"},
			wantErr: "flag provided but not defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseSyncFlags(tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q missing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestParseSyncFlagsHelp(t *testing.T) {
	_, err := parseSyncFlags([]string{"--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got %v", err)
	}
}
