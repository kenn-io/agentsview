// ABOUTME: `session get <id>` subcommand — prints session detail
// ABOUTME: in human or JSON format.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
)

func newSessionGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "get <id>",
		Short:        "Get session metadata and signals",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			detail, err := lookupSessionWithPrefixes(
				cmd.Context(), svc, args[0],
			)
			if err != nil {
				return err
			}
			if detail == nil {
				return fmt.Errorf("session %s not found", args[0])
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(detail)
			}
			return printSessionDetailHuman(cmd.OutOrStdout(), detail)
		},
	}
}

// resolveServiceSessionID returns the canonical session ID matching id,
// accommodating bare UUIDs by retrying with each registered agent
// prefix (codex:, copilot:, gemini:, ...) when the exact lookup
// misses. Stored IDs are prefixed for non-Claude agents, so a user
// copying a UUID from a session file name would otherwise see a
// confusing "not found" error. Returns an error whose message
// begins with "session not found:" when no match exists — callers
// get a clear failure instead of silent empty output.
func resolveServiceSessionID(
	ctx context.Context,
	svc service.SessionService,
	id string,
) (string, error) {
	detail, err := svc.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if detail != nil {
		return id, nil
	}
	if hostPrefix, legacyRaw, ok := splitLegacyTraeSessionID(id); ok {
		candidate, err, known := resolveTraeNamespacedSessionID(
			ctx, svc, hostPrefix, legacyRaw,
		)
		if err != nil {
			return "", err
		}
		if known {
			return candidate, nil
		}
		return "", fmt.Errorf("session not found: %s", id)
	}
	// If the user already supplied a known agent-prefixed ID or
	// a host-prefixed remote ID ("host~..."), don't second-guess
	// them — the exact lookup is authoritative. Some raw IDs
	// (Kimi/Kimi Code, OpenClaw) contain colons before the agent
	// prefix is added, so an arbitrary colon is not enough to
	// classify the input as canonical.
	if isCanonicalServiceSessionID(id) {
		return "", fmt.Errorf("session not found: %s", id)
	}
	traeCandidate, traeErr, traeKnown := resolveTraeNamespacedSessionID(
		ctx, svc, id,
	)
	if traeErr != nil {
		return "", traeErr
	}
	for _, def := range parser.Registry {
		if def.IDPrefix == "" {
			continue
		}
		candidate := def.IDPrefix + id
		detail, err := svc.Get(ctx, candidate)
		if err != nil {
			return "", err
		}
		if detail != nil {
			return candidate, nil
		}
	}
	if traeKnown {
		return traeCandidate, nil
	}
	return "", fmt.Errorf("session not found: %s", id)
}

func isCanonicalServiceSessionID(id string) bool {
	if strings.Contains(id, "~") {
		return true
	}
	_, rawID := parser.StripHostPrefix(id)
	for _, def := range parser.Registry {
		if def.IDPrefix != "" && strings.HasPrefix(rawID, def.IDPrefix) {
			return true
		}
	}
	return false
}

var traeSessionLookupNamespaces = []string{
	"workspaceStorage",
	"globalStorage",
}

func splitLegacyTraeSessionID(id string) (string, string, bool) {
	host, stripped := parser.StripHostPrefix(id)
	if !strings.HasPrefix(stripped, "trae:") ||
		strings.HasPrefix(stripped, "trae:workspaceStorage:") ||
		strings.HasPrefix(stripped, "trae:globalStorage:") {
		return "", "", false
	}
	raw := strings.TrimPrefix(stripped, "trae:")
	raw = strings.TrimPrefix(raw, "trae:")
	if raw == "" {
		return "", "", false
	}
	if host == "" {
		return "", raw, true
	}
	return host + "~", raw, true
}

func resolveTraeRawSuffixAmbiguity(id string, matches []string) error {
	workspaceID := string(parser.AgentTrae) + ":workspaceStorage:" + id
	globalID := string(parser.AgentTrae) + ":globalStorage:" + id
	workspaceMatch := ""
	globalMatch := ""
	for _, match := range matches {
		_, stripped := parser.StripHostPrefix(match)
		switch stripped {
		case workspaceID:
			if workspaceMatch == "" {
				workspaceMatch = match
			}
		case globalID:
			if globalMatch == "" {
				globalMatch = match
			}
		}
	}
	if workspaceMatch != "" && globalMatch != "" {
		return fmt.Errorf(
			"session %s is ambiguous; use %s or %s",
			id, workspaceMatch, globalMatch,
		)
	}
	return nil
}

// resolveTraeNamespacedSessionID restores raw and legacy-canonical lookup for
// namespaced Trae sessions when exactly one namespace matches, and rejects the
// ambiguous dual-match case so callers do not silently pick a namespace.
func resolveTraeNamespacedSessionID(
	ctx context.Context,
	svc service.SessionService,
	args ...string,
) (string, error, bool) {
	hostPrefix := ""
	id := ""
	switch len(args) {
	case 1:
		id = args[0]
	case 2:
		hostPrefix = args[0]
		id = args[1]
	default:
		return "", fmt.Errorf("invalid trae session lookup"), true
	}
	var match string
	for _, ns := range traeSessionLookupNamespaces {
		candidate := hostPrefix + string(parser.AgentTrae) + ":" + ns + ":" + id
		detail, err := svc.Get(ctx, candidate)
		if err != nil {
			return "", err, true
		}
		if detail == nil {
			continue
		}
		if match != "" {
			return "", fmt.Errorf(
				"session %s is ambiguous; use %strae:workspaceStorage:%s or %strae:globalStorage:%s",
				id, hostPrefix, id, hostPrefix, id,
			), true
		}
		match = candidate
	}
	if match == "" {
		matches, err := svc.FindSessionIDsByPartial(ctx, id, 64)
		if err != nil {
			return "", err, true
		}
		filtered := make([]string, 0, len(matches))
		for _, candidate := range matches {
			if hostPrefix != "" && !strings.HasPrefix(candidate, hostPrefix) {
				continue
			}
			_, stripped := parser.StripHostPrefix(candidate)
			switch stripped {
			case string(parser.AgentTrae) + ":workspaceStorage:" + id,
				string(parser.AgentTrae) + ":globalStorage:" + id:
				filtered = append(filtered, candidate)
			}
		}
		if len(filtered) == 0 {
			return "", nil, false
		}
		if err := resolveTraeRawSuffixAmbiguity(id, filtered); err != nil {
			return "", err, true
		}
		return filtered[0], nil, true
	}
	return match, nil, true
}

// lookupSessionWithPrefixes fetches a session detail, trying agent
// prefixes for bare UUIDs. Preserved as a thin wrapper around
// resolveServiceSessionID + svc.Get so `session get` can keep its
// existing "return nil on not-found" semantics (which render the
// "session %s not found" error at the command boundary).
func lookupSessionWithPrefixes(
	ctx context.Context,
	svc service.SessionService,
	id string,
) (*service.SessionDetail, error) {
	resolved, err := resolveServiceSessionID(ctx, svc, id)
	if err != nil {
		if strings.HasPrefix(err.Error(), "session not found:") {
			return nil, nil
		}
		return nil, err
	}
	return svc.Get(ctx, resolved)
}

// printSessionDetailHuman writes a compact key/value summary of
// the session's core fields. Optional *string/*int fields render
// as "-" when nil.
func printSessionDetailHuman(w io.Writer, s *service.SessionDetail) error {
	label := func(name string) string {
		return fmt.Sprintf("%-14s", name+":")
	}
	name := s.ID
	if s.DisplayName != nil && *s.DisplayName != "" {
		name = *s.DisplayName
	}
	fmt.Fprintf(w, "%s %s\n", label("ID"), sanitizeTerminal(s.ID))
	fmt.Fprintf(w, "%s %s\n", label("Name"), sanitizeTerminal(name))
	fmt.Fprintf(w, "%s %s\n", label("Project"), sanitizeTerminal(s.Project))
	fmt.Fprintf(w, "%s %s\n", label("Agent"), sanitizeTerminal(s.Agent))
	fmt.Fprintf(w, "%s %s\n", label("Machine"), sanitizeTerminal(s.Machine))
	fmt.Fprintf(w, "%s %s\n",
		label("Started At"), sanitizeTerminal(derefStringOrDash(s.StartedAt)))
	fmt.Fprintf(w, "%s %s\n",
		label("Ended At"), sanitizeTerminal(derefStringOrDash(s.EndedAt)))
	fmt.Fprintf(w, "%s %d/%d\n",
		label("Messages"), s.UserMessageCount, s.MessageCount)
	if s.Outcome != "" {
		fmt.Fprintf(w, "%s %s [%s]\n", label("Outcome"),
			sanitizeTerminal(s.Outcome), sanitizeTerminal(s.OutcomeConfidence))
	}
	if s.HealthScore != nil {
		grade := "-"
		if s.HealthGrade != nil && *s.HealthGrade != "" {
			grade = *s.HealthGrade
		}
		fmt.Fprintf(w, "%s %d (%s)\n",
			label("Health"), *s.HealthScore, sanitizeTerminal(grade))
	} else {
		fmt.Fprintf(w, "%s -\n", label("Health"))
	}
	if s.SecretLeakCount > 0 {
		fmt.Fprintf(w, "%s %d\n", label("Secrets"), s.SecretLeakCount)
	}
	return nil
}

// derefStringOrDash returns *p or "-" when p is nil or empty.
func derefStringOrDash(p *string) string {
	if p == nil || *p == "" {
		return "-"
	}
	return *p
}
