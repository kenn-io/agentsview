package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpListsAllCommands(t *testing.T) {
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	help := out.String()
	for _, want := range []string{
		"serve",
		"sync",
		"prune",
		"update",
		"token-use",
		"import",
		"projects",
		"version",
		"pg",
		"pg push",
		"pg status",
		"pg serve",
		"--host",
		"--port",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q\n%s", want, help)
		}
	}
}

func TestRootHelpGroupsCompletionAtBottom(t *testing.T) {
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	help := out.String()
	for _, want := range []string{
		"Core Commands:",
		"Data Commands:",
		"Other Commands:",
		"completion             Generate the autocompletion script for the specified shell",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q\n%s", want, help)
		}
	}

	core := strings.Index(help, "Core Commands:")
	data := strings.Index(help, "Data Commands:")
	other := strings.Index(help, "Other Commands:")
	completion := strings.Index(help, "completion             Generate the autocompletion script for the specified shell")
	serve := strings.Index(help, "serve                  Start server")
	if !(core >= 0 && data > core && other > data) {
		t.Fatalf("unexpected group order\n%s", help)
	}
	if completion < other {
		t.Fatalf("completion not grouped at bottom\n%s", help)
	}
	if serve > other {
		t.Fatalf("serve command placed after other commands\n%s", help)
	}
}

func TestRootHelpDoesNotDuplicateCommandListings(t *testing.T) {
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	help := out.String()
	if strings.Contains(help, "agentsview serve [flags]") {
		t.Fatalf("usage duplicates command listings\n%s", help)
	}
	for _, want := range []string{
		"Usage:\n  agentsview [flags]\n  agentsview <command>",
		"Core Commands:",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q\n%s", want, help)
		}
	}
	if strings.Contains(help, "\nCommands:\n") {
		t.Fatalf("help should not include redundant Commands heading\n%s", help)
	}
}

func TestRootVersionFlag(t *testing.T) {
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "agentsview ") {
		t.Fatalf("version output = %q", got)
	}
}
