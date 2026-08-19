// Package tui provides the full-screen terminal interface for agentsview.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Options configures a TUI process after the command resolves its daemon.
type Options struct {
	BaseURL   string
	Token     string
	ReadOnly  bool
	StatePath string
	Timezone  string
	Open      func(string) error
}

// Run starts the interactive terminal interface.
func Run(ctx context.Context, opts Options) error {
	if opts.BaseURL == "" {
		return errors.New("daemon URL is required")
	}
	info, err := os.Stdin.Stat()
	if err != nil {
		return fmt.Errorf("inspect terminal input: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("agentsview tui requires an interactive terminal")
	}
	if opts.Timezone == "" {
		opts.Timezone = time.Now().Location().String()
	}
	if opts.Open == nil {
		opts.Open = openLocation
	}
	client := NewClient(opts.BaseURL, opts.Token, opts.ReadOnly)
	settingsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	settings, err := client.Settings(settingsCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("load daemon settings: %w", err)
	}
	opts.ReadOnly = settings.ReadOnly
	client.readOnly = settings.ReadOnly
	program := tea.NewProgram(newModel(ctx, client, opts), tea.WithContext(ctx))
	_, err = program.Run()
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

func openLocation(location string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{location}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", location}
	default:
		name, args = "xdg-open", []string{location}
	}
	if err := exec.Command(name, args...).Start(); err != nil {
		return fmt.Errorf("open %q: %w", location, err)
	}
	return nil
}
