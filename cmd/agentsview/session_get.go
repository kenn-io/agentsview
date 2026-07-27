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
	"go.kenn.io/agentsview/internal/config"
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

			id := args[0]
			// Pre-flight bare Codebuff/Freebuff timestamp resolution
			// before handing id off to the standard resolver. The
			// prefix loop in resolveServiceSessionID cannot build
			// the right canonical ID for these agents (canonical is
			// "<agent>:<project>:<timestamp>" with the project
			// segment missing, and Freebuff is intentionally absent
			// from parser.Registry). Walking the storage layer here
			// localizes the fix to this command and keeps the
			// resolver's signature unchanged so other commands and
			// callers see no framework impact.
			if !isCanonicalServiceSessionID(id) {
				cfg := mustLoadConfig(cmd)
				resolved, err := resolveBareCodebuffID(
					cmd.Context(), svc, &cfg, id,
				)
				if err != nil {
					return err
				}
				if resolved != "" {
					id = resolved
				}
			}

			detail, err := lookupSessionWithPrefixes(
				cmd.Context(), svc, id,
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
//
// Bare Codebuff/Freebuff timestamps are pre-resolved by
// resolveBareCodebuffID at the call site (see newSessionGetCommand),
// so this function does not need to know about them.
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
	// If the user already supplied a known agent-prefixed ID or
	// a host-prefixed remote ID ("host~..."), don't second-guess
	// them — the exact lookup is authoritative. Some raw IDs
	// (Kimi/Kimi Code, OpenClaw) contain colons before the agent
	// prefix is added, so an arbitrary colon is not enough to
	// classify the input as canonical.
	if isCanonicalServiceSessionID(id) {
		return "", fmt.Errorf("session not found: %s", id)
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
	return "", fmt.Errorf("session not found: %s", id)
}

// resolveBareCodebuffID maps a bare on-disk Codebuff/Freebuff
// timestamp to its canonical ID by walking the configured
// codebuff/freebuff storage layer. For each candidate location on
// disk, the helper tries BOTH AgentCodebuff and AgentFreebuff
// prefixes against the session service; whichever yields a row
// wins. The dual lookup is required because Freebuff is
// intentionally absent from parser.Registry, so Freebuff's
// storage is reachable only through the shared codebuff roots
// list and a single-prefix probe would mis-classify Freebuff
// sessions as Codebuff. Zero matches fall through to the standard
// resolver path; one match returns its canonical ID; multiple
// matches return an explicit ambiguity error listing every valid
// canonical ID.
func resolveBareCodebuffID(
	ctx context.Context,
	svc service.SessionService,
	cfg *config.Config,
	rawID string,
) (string, error) {
	if cfg == nil {
		return "", nil
	}
	locations := parser.FindCodebuffFreebuffMatches(
		[]parser.CodebuffFamilyRoots{
			{Agent: parser.AgentCodebuff,
				Roots: cfg.ResolveDirs(parser.AgentCodebuff)},
			{Agent: parser.AgentFreebuff,
				Roots: cfg.ResolveDirs(parser.AgentFreebuff)},
		},
		rawID,
	)
	tryBoth := func(project string) string {
		for _, agent := range []parser.AgentType{
			parser.AgentCodebuff, parser.AgentFreebuff,
		} {
			candidateID := strings.Join(
				[]string{string(agent), project, rawID}, ":",
			)
			if detail, err := svc.Get(ctx, candidateID); err == nil &&
				detail != nil {
				return candidateID
			}
		}
		return ""
	}
	switch len(locations) {
	case 0:
		return "", nil
	case 1:
		return tryBoth(locations[0].ProjectHint), nil
	default:
		var valid []string
		for _, loc := range locations {
			if v := tryBoth(loc.ProjectHint); v != "" {
				valid = append(valid, v)
			}
		}
		switch len(valid) {
		case 1:
			return valid[0], nil
		default:
			return "", fmt.Errorf(
				"ambiguous session id %q: matches %d canonical sessions: %s. "+
					"Re-run with one of the canonical IDs to disambiguate",
				rawID, len(valid), strings.Join(valid, ", "),
			)
		}
	}
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
