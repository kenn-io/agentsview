package ssh

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// resolveFilePrefix marks lines in the resolve script output that name
// an extra file (not an agent directory) to include in the transfer. It
// is not a valid agent type, so parseResolvedDirs routes it separately.
const resolveFilePrefix = "@file"

// buildResolveScript generates a shell script that echoes each file-based
// agent's resolved transfer target on the remote host. Output format:
// "agentType:path\n" per agent target, plus "@file:path\n" lines for sibling
// metadata files such as Codex's session_index.jsonl.
//
// Only includes agents where FileBased is true and DiscoverFunc
// is non-nil. For each agent with an EnvVar, the script checks
// the env var first and falls back to the default dir. Dirs (and
// files) that don't exist on the remote are skipped.
func buildResolveScript() string {
	var b strings.Builder
	for _, def := range parser.Registry {
		if !def.FileBased || def.DiscoverFunc == nil {
			continue
		}
		for _, rel := range def.DefaultDirs {
			// Aider's only default dir is "" (the bare home directory):
			// it has no central store and discovers history files by a
			// bounded, depth-capped walk of $HOME. The remote resolver emits
			// tar targets, so emitting the "" default would archive the
			// entire remote $HOME. Skip aider's bare-home default. When
			// AIDER_DIR explicitly scopes discovery to a code root, emit only
			// discovered .aider.chat.history.md files as tar targets instead
			// of the whole source tree. Local sync is unaffected: it walks
			// DefaultDirs via DiscoverFunc, not this script. The shell guard
			// below also drops AIDER_DIR if it is set to literal "$HOME" (or
			// "$HOME/"), so an unscoped override cannot reintroduce a
			// whole-home scan or tar.
			if def.Type == parser.AgentAider && rel == "" {
				if def.EnvVar != "" {
					fmt.Fprintf(&b,
						"dir=\"${%s:-}\"; "+
							"case \"$dir\" in \"\"|\"$HOME\"|\"$HOME/\") ;; "+
							"*) if [ -d \"$dir\" ]; then "+
							"find \"$dir\" -type f -name '%s' -print | sort | "+
							"while IFS= read -r f; do echo \"%s:$f\"; done; "+
							"fi;; esac\n",
						def.EnvVar, parser.AiderHistoryFileName(),
						string(def.Type),
					)
				}
				continue
			}
			defaultDir := "$HOME/" + rel
			dirExpr := defaultDir
			if def.EnvVar != "" {
				// env var overrides default
				dirExpr = fmt.Sprintf("${%s:-%s}", def.EnvVar, defaultDir)
			}
			fmt.Fprintf(&b,
				"dir=\"%s\"; [ -d \"$dir\" ] && echo \"%s:$dir\"\n",
				dirExpr, string(def.Type),
			)
			// Codex stores renameable session titles in
			// session_index.jsonl, which sits beside (not inside)
			// sessions/ and archived_sessions/. Emit it so renames
			// import on remote hosts too. ${dir%/*} is the parent.
			if def.Type == parser.AgentCodex {
				fmt.Fprintf(&b,
					"idx=\"${dir%%/*}/%s\"; "+
						"[ -f \"$idx\" ] && echo \"%s:$idx\"\n",
					parser.CodexSessionIndexFilename,
					resolveFilePrefix,
				)
			}
		}
	}
	// Ensure exit 0 — the last [ -d ]/[ -f ] test may fail if that
	// path doesn't exist, which would make sh exit non-zero.
	b.WriteString("true\n")
	return b.String()
}

// parseResolvedDirs parses script output into a map of agent type to transfer
// target paths plus a deduplicated list of extra files (lines tagged with
// resolveFilePrefix). Most agent targets are directories; Aider targets are
// individual .aider.chat.history.md files. Skips empty lines and entries with
// empty values.
func parseResolvedDirs(
	output string,
) (map[parser.AgentType][]string, []string) {
	dirs := make(map[parser.AgentType][]string)
	var extraFiles []string
	seenFile := make(map[string]struct{})
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || value == "" {
			continue
		}
		if key == resolveFilePrefix {
			if _, dup := seenFile[value]; dup {
				continue
			}
			seenFile[value] = struct{}{}
			extraFiles = append(extraFiles, value)
			continue
		}
		at := parser.AgentType(key)
		dirs[at] = append(dirs[at], value)
	}
	return dirs, extraFiles
}

// resolveDirs runs the resolve script on the remote host via SSH and
// returns the discovered agent directories plus extra sibling files
// (such as Codex's session_index.jsonl) to include in the transfer.
func resolveDirs(
	ctx context.Context,
	host, user string, port int, sshOpts []string,
) (map[parser.AgentType][]string, []string, error) {
	script := buildResolveScript()
	out, err := runSSH(ctx, host, user, port, sshOpts, script)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve dirs: %w", err)
	}
	dirs, extraFiles := parseResolvedDirs(string(out))
	return dirs, extraFiles, nil
}
