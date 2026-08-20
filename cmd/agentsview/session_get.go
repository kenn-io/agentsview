// ABOUTME: `session get <id>` subcommand — prints session detail
// ABOUTME: in human or JSON format.
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
)

func newSessionGetCommand() *cobra.Command {
	cmd := &cobra.Command{
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
			if resolved, err := resolveCodebuffBareID(cmd, svc, id); err != nil {
				return err
			} else if resolved != "" {
				id = resolved
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
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), detail)
			}
			return printSessionDetailHuman(cmd.OutOrStdout(), detail)
		},
	}
	// The machine filter is local to `session get` only. session
	// list / search / export already register their own --machine
	// flags with different roles (list/search filter; export writes
	// a machine label for the export artifact); declaring a shared
	// persistent flag would either collide with those or be a silent
	// no-op for them. Keep this scope narrow.
	cmd.Flags().String(
		"machine", "local",
		"Filter bare Codebuff/Freebuff timestamp resolution to a single "+
			"machine identity. Use 'local' (default) to match this "+
			"archive's own sessions, a machine name to match by exact "+
			"sessions.machine value, or '*' to accept any. Does not "+
			"affect canonical IDs or agents other than Codebuff/Freebuff.",
	)
	return cmd
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
// resolveCodebuffBareID at the call site (see newSessionGetCommand),
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
// prefixes against the session service, gated by machineFilter so
// remote-synced records don't masquerade as local matches. The
// dual lookup is required because Freebuff is intentionally
// absent from parser.Registry, so Freebuff's storage is reachable
// only through the shared codebuff roots list and a single-prefix
// probe would mis-classify Freebuff sessions as Codebuff. Zero
// matches fall through to the standard resolver path; one match
// returns its canonical ID; multiple matches whose machine passes
// the filter return an explicit ambiguity error listing every
// valid canonical ID.
//
// machineFilter is the user's --machine flag value. "" or "local"
// defers to cfg.LocalMachineName (the local archive's identity,
// runtime-derived from os.Hostname by config.LoadPFlags); "*"
// accepts any machine; any other non-empty string is matched
// exactly against detail.Machine. The caller is responsible for
// parsing the flag into one of these categories.
func resolveBareCodebuffID(
	ctx context.Context,
	svc service.SessionService,
	cfg *config.Config,
	rawID string,
	machineFilter string,
) (string, error) {
	if cfg == nil {
		return "", nil
	}
	localMachine := cfg.LocalMachineName
	locations := parser.FindCodebuffFreebuffMatches(
		[]parser.CodebuffFamilyRoots{
			{Agent: parser.AgentCodebuff,
				Roots: cfg.ResolveDirs(parser.AgentCodebuff)},
			{Agent: parser.AgentFreebuff,
				Roots: cfg.ResolveDirs(parser.AgentFreebuff)},
		},
		rawID,
	)

	var (
		valid     []string
		seen      = make(map[string]struct{})
		lookupErr error
	)

	// Phase 1: Unprefixed probes using filesystem project hints.
	// Each location supplies a project name; try both codebuff and
	// freebuff prefixes against the session service. This phase
	// depends on a local Codebuff/Freebuff directory on disk and
	// produces no candidates when len(locations)==0.
	for _, agent := range []parser.AgentType{
		parser.AgentCodebuff, parser.AgentFreebuff,
	} {
		for _, loc := range locations {
			candidate := strings.Join(
				[]string{string(agent), loc.ProjectHint, rawID}, ":",
			)
			detail, err := svc.Get(ctx, candidate)
			if err != nil {
				if lookupErr == nil {
					lookupErr = err
				}
				continue
			}
			if detail == nil {
				continue
			}
			if !codebuffMachineMatches(
				detail.Machine, machineFilter, localMachine,
			) {
				continue
			}
			if _, dup := seen[candidate]; dup {
				continue
			}
			seen[candidate] = struct{}{}
			valid = append(valid, candidate)
		}
	}

	// Phase 2: Archived and remote-synced sessions found in the
	// database. This runs independently of the filesystem walk (even
	// when len(locations)==0) so sessions with no on-disk copy —
	// local archive rows whose source directory was deleted, or
	// remote rows imported via pg push or remotesync — can still be
	// resolved. The filter requires a codebuff or freebuff agent
	// marker (with or without a host prefix), the exact timestamp
	// suffix, and a machine match; the seen map dedupes rows Phase 1
	// already surfaced so an on-disk session is not double-counted
	// as ambiguous with its own archive row.
	//
	// Canonical Codebuff/Freebuff IDs carry the form
	// [host~]codebuff:<project>:<timestamp> — the project segment
	// separates the agent prefix from the timestamp. An
	// agent-prefixed query ("codebuff:"+rawID) cannot match because
	// the prefix isn't contiguous with the timestamp. Search by the
	// raw timestamp alone so codebuff:<project>:<timestamp> rows are
	// found. The post-fetch guards below (suffix, agent marker,
	// machine) keep the result set specific.
	ids, findErr := svc.FindSessionIDsByPartial(
		ctx, rawID, 200,
	)
	if findErr != nil && lookupErr == nil {
		lookupErr = fmt.Errorf(
			"lookup archived sessions for %q: %w",
			rawID, findErr,
		)
	}
	for _, id := range ids {
		if !strings.HasSuffix(id, ":"+rawID) {
			continue
		}
		// Guard against non-codebuff/freebuff sessions whose ID
		// happens to contain the timestamp.
		if !strings.HasPrefix(id, "codebuff:") &&
			!strings.HasPrefix(id, "freebuff:") &&
			!strings.Contains(id, "~codebuff:") &&
			!strings.Contains(id, "~freebuff:") {
			continue
		}
		detail, err := svc.Get(ctx, id)
		if err != nil {
			if lookupErr == nil {
				lookupErr = err
			}
			continue
		}
		if detail == nil {
			continue
		}
		if !codebuffMachineMatches(
			detail.Machine, machineFilter, localMachine,
		) {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		valid = append(valid, id)
	}

	// Single zero/one/many decision over both phases. Fail closed on
	// any candidate lookup error: a probe that errored could hide a
	// second match, so returning the lone successful candidate may
	// resolve an ID that is genuinely ambiguous.
	if lookupErr != nil {
		return "", lookupErr
	}
	switch len(valid) {
	case 0:
		return "", nil
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

// codebuffMachineMatches reports whether a row's machine column
// passes the user's --machine filter. "" or "local" defers to
// localMachine (cfg.LocalMachineName); "*" matches anything; any
// other non-empty value is matched exactly. This keeps the resolver
// decoupled from cobra while still honouring the rule that
// unspecified / "local" mean "this archive's own sessions".
func codebuffMachineMatches(
	rowMachine, filter, localMachine string,
) bool {
	switch filter {
	case "*":
		return true
	case "", "local":
		return rowMachine == localMachine
	default:
		return rowMachine == filter
	}
}

// resolveCodebuffBareID attempts to resolve a bare Codebuff/Freebuff
// timestamp to its canonical ID for the local archive. Returns
// ("", nil) when the ID is already canonical, when the input is
// not a Codebuff/Freebuff timestamp shape (so the generic prefix
// resolver still handles it), or when no local match is found;
// returns an explicit error for Codebuff/Freebuff timestamp inputs
// on remote stores so the user is pointed at `session list` and
// the canonical ID formats.
//
// Remote stores (`--server`, `--pg`) cannot safely resolve a bare
// timestamp back to a canonical ID: a timestamp is intrinsically
// ambiguous across machines and projects, mixing `host~`-prefixed
// rows from PG push with local aliases would silently pick one or
// fire an ambiguous error the user can't act on without
// `session list` anyway. The contract for remote reads is "pass
// the canonical ID". A user with only a timestamp must run
// `session list --machine=...` to find one first.
//
// Bare IDs that are NOT Codebuff/Freebuff timestamps (Codex /
// Copilot / Gemini UUIDs, etc.) reach register-prefix retry via
// resolveServiceSessionID unchanged — the Codebuff-specific error
// is reserved for inputs whose shape identifies them as
// Codebuff/Freebuff timestamps.
func resolveCodebuffBareID(
	cmd *cobra.Command, svc service.SessionService, id string,
) (string, error) {
	if isCanonicalServiceSessionID(id) {
		return "", nil
	}
	// Distinguish a Codebuff/Freebuff timestamp from a bare UUID
	// for another agent. The Codebuff-specific remote error must
	// fire ONLY on the timestamp shape; otherwise it short-
	// circuits generic bare-ID resolution against --server/--pg.
	// Syntactic (parseCodebuffSessionDate) — no FS walk, which
	// keeps --server/--pg cheap and avoids depending on
	// configured local codebuff roots.
	if !parser.IsCodebuffTimestamp(id) {
		return "", nil
	}
	remote, _ := cmd.Flags().GetString("server")
	if remote != "" || pgReadRequested(cmd) {
		return "", errBareCodebuffRemoteUnsupported(id)
	}
	cfg := mustLoadConfig(cmd)
	machineFlag, _ := cmd.Flags().GetString("machine")
	return resolveBareCodebuffID(
		cmd.Context(), svc, &cfg, id, machineFlag,
	)
}

// errBareCodebuffRemoteUnsupported builds the error returned when
// a non-canonical ID hits the resolver against a remote transport.
// The message point at `session list` and lists every canonical ID
// shape (local and remote-synced, codebuff and freebuff) so the
// user can pick the right invocation without trial and error.
func errBareCodebuffRemoteUnsupported(id string) error {
	return fmt.Errorf(
		"%q is not a canonical session ID and cannot be resolved "+
			"against a remote store (--server or --pg). "+
			"Run `agentsview session list` to find the canonical ID, "+
			"then pass one of:\n"+
			"  codebuff:<project>:<ts>          # local session\n"+
			"  freebuff:<project>:<ts>          # local freebuff session\n"+
			"  host~codebuff:<project>:<ts>     # remote-synced session\n"+
			"  host~freebuff:<project>:<ts>     # remote-synced freebuff",
		id,
	)
} // isCanonicalServiceSessionID reports whether id is already in the
// canonical "agent:..." or "host~..." form that resolveServiceSessionID
// can look up directly.
//
// Freebuff is intentionally absent from parser.Registry (it shares
// the Codebuff provider and would double-discover); mirror the
// special case parser.AgentByPrefix applies so `freebuff:<proj>:<ts>`
// inputs pass through unchanged instead of being misclassified as
// bare timestamps by resolveCodebuffBareID.
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
	return strings.HasPrefix(rawID, string(parser.AgentFreebuff)+":")
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
