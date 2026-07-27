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

			cfg := mustLoadConfig(cmd)

			detail, err := lookupSessionWithPrefixes(
				cmd.Context(), svc, &cfg, args[0],
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
// misses, and walking the Codebuff/Freebuff storage layer when the
// prefix loop misses for a bare timestamp. Stored IDs are prefixed
// for non-Claude agents, so a user copying a UUID from a session
// file name would otherwise see a confusing "not found" error.
//
// Codebuff and Freebuff share the directory layout
// "<root>/<project>/chats/<timestamp>/" but the canonical ID is
// "<agent>:<project>:<timestamp>" — a bare timestamp cannot be
// resolved purely by the prefix loop (it would build
// "codebuff:<timestamp>" without the project segment and miss
// the canonical row, and Freebuff is intentionally absent from
// parser.Registry). The bare-ID resolver walks codebuff/
// freebuff roots to find candidate (agent, project) locations
// for rawID and either selects the lone match, or surfaces an
// explicit ambiguity error so the user can disambiguate.
//
// Returns an error whose message begins with "session not found:"
// when no match exists — callers get a clear failure instead of
// silent empty output. The ambiguity error wraps an explicit
// list of candidate canonical IDs.
func resolveServiceSessionID(
	ctx context.Context,
	svc service.SessionService,
	cfg *config.Config,
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
	// Prefix loop missed. Walk the codebuff/freebuff storage
	// layer to map a bare timestamp back to its canonical ID.
	// The prefix loop cannot build the right canonical ID for
	// these agents (canonical is "<agent>:<project>:<timestamp>"
	// with the project segment missing, and Freebuff is
	// intentionally absent from parser.Registry). Walking the
	// storage layer localizes the fix to the Codebuff/Freebuff
	// agents and avoids adding a generic SourceSessionID lookup
	// query to every storage backend.
	candidates := resolveCodebuffFamilyCandidates(cfg, id)
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("session not found: %s", id)
	case 1:
		return candidates[0].CanonicalID(), nil
	default:
		ids := make([]string, len(candidates))
		for i, m := range candidates {
			ids[i] = m.CanonicalID()
		}
		return "", fmt.Errorf(
			"ambiguous session id %q: matches %d canonical sessions: %s. "+
				"Re-run with one of the canonical IDs to disambiguate",
			id, len(candidates), strings.Join(ids, ", "),
		)
	}
}

// resolveCodebuffFamilyCandidates walks the configured codebuff
// and freebuff storage roots for a directory named rawID under
// any project's chats/ subdirectory. Returns one match per
// (agent, project) candidate; an empty list means the bare ID
// is unknown to the on-disk storage.
//
// codebuffRoots / freebuffRoots come from cfg.ResolveDirs so env
// vars (CODEBUFF_DIR / FREEBUFF_DIR) and config.toml overrides
// are honored — cli callers see the same directories the parser
// sees.
func resolveCodebuffFamilyCandidates(
	cfg *config.Config, rawID string,
) []parser.CodebuffFamilyMatch {
	if cfg == nil {
		return nil
	}
	return parser.FindCodebuffFreebuffMatches(
		[]parser.CodebuffFamilyRoots{
			{Agent: parser.AgentCodebuff,
				Roots: cfg.ResolveDirs(parser.AgentCodebuff)},
			{Agent: parser.AgentFreebuff,
				Roots: cfg.ResolveDirs(parser.AgentFreebuff)},
		},
		rawID,
	)
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
// "session %s not found" error at the command boundary). cfg
// supplies the codebuff/freebuff storage roots so the resolver
// can map a bare timestamp back to its canonical ID; nil cfg
// disables the bare-ID fallback path.
func lookupSessionWithPrefixes(
	ctx context.Context,
	svc service.SessionService,
	cfg *config.Config,
	id string,
) (*service.SessionDetail, error) {
	resolved, err := resolveServiceSessionID(ctx, svc, cfg, id)
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
