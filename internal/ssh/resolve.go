package ssh

import (
	"context"
	"fmt"
	"path"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// resolveFilePrefix marks lines in the resolve script output that name
// an extra file (not an agent directory) to include in the transfer. It
// is not a valid agent type, so parseResolvedDirs routes it separately.
const resolveFilePrefix = "@file"

const resolveRecordSep = "\x00"

func aiderSkipDirCasePattern() string {
	return strings.Join(parser.AiderDiscoverySkipDirNames(), "|")
}

func buildAiderResolveSnippet(envVar string) string {
	return fmt.Sprintf(
		"av_aider_walk() { "+
			"[ \"$av_aider_files\" -ge %d ] && return; "+
			"[ \"$av_aider_dirs\" -ge %d ] && return; "+
			"for av_entry in \"$1\"/* \"$1\"/.[!.]* \"$1\"/..?*; do "+
			"[ -e \"$av_entry\" ] || continue; "+
			"[ -L \"$av_entry\" ] && continue; "+
			"av_base=${av_entry##*/}; "+
			"if [ -d \"$av_entry\" ]; then "+
			"case \"$av_base\" in %s) continue;; esac; "+
			"[ \"$2\" -ge %d ] && continue; "+
			"av_aider_dirs=$((av_aider_dirs + 1)); "+
			"av_aider_walk \"$av_entry\" $(($2 + 1)); "+
			"[ \"$av_aider_files\" -ge %d ] && return; "+
			"[ \"$av_aider_dirs\" -ge %d ] && return; "+
			"elif [ -f \"$av_entry\" ] && [ \"$av_base\" = '%s' ]; then "+
			"printf '%%s\\000' \"%s:$av_entry\"; "+
			"av_aider_files=$((av_aider_files + 1)); "+
			"[ \"$av_aider_files\" -ge %d ] && return; "+
			"fi; "+
			"done; "+
			"}; "+
			"dir=\"${%s:-}\"; "+
			"case \"$dir\" in \"\"|\"$HOME\"|\"$HOME/\") ;; "+
			"*) if [ -d \"$dir\" ]; then "+
			"av_aider_files=0; av_aider_dirs=1; "+
			"av_aider_walk \"$dir\" 0; "+
			"fi;; esac\n",
		parser.AiderDiscoveryMaxFiles(),
		parser.AiderDiscoveryMaxDirs(),
		aiderSkipDirCasePattern(),
		parser.AiderDiscoveryMaxWalkDepth(),
		parser.AiderDiscoveryMaxFiles(),
		parser.AiderDiscoveryMaxDirs(),
		parser.AiderHistoryFileName(),
		string(parser.AgentAider),
		parser.AiderDiscoveryMaxFiles(),
		envVar,
	)
}

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
					b.WriteString(buildAiderResolveSnippet(def.EnvVar))
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
				"dir=\"%s\"; [ -d \"$dir\" ] && "+
					"printf '%%s\\000' \"%s:$dir\"\n",
				dirExpr, string(def.Type),
			)
			// Codex stores renameable session titles in
			// session_index.jsonl, which sits beside (not inside)
			// sessions/ and archived_sessions/. Emit it so renames
			// import on remote hosts too. ${dir%/*} is the parent.
			if def.Type == parser.AgentCodex {
				fmt.Fprintf(&b,
					"idx=\"${dir%%/*}/%s\"; "+
						"[ -f \"$idx\" ] && "+
						"printf '%%s\\000' \"%s:$idx\"\n",
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
// target paths plus a deduplicated list of extra files (records tagged with
// resolveFilePrefix). Generated resolver output is NUL-delimited so remote
// paths containing newlines cannot inject extra records; newline-delimited input
// is accepted only for older tests and defensive compatibility. Most agent
// targets are directories; Aider targets are individual .aider.chat.history.md
// files. Skips empty records, empty values, and values containing record
// separators.
func parseResolvedDirs(
	output string,
) (map[parser.AgentType][]string, []string) {
	dirs := make(map[parser.AgentType][]string)
	var extraFiles []string
	seenFile := make(map[string]struct{})
	for _, record := range resolveOutputRecords(output) {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		key, value, ok := strings.Cut(record, ":")
		if !ok || invalidResolvedPath(value) {
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
		if at == parser.AgentAider &&
			path.Base(value) != parser.AiderHistoryFileName() {
			continue
		}
		dirs[at] = append(dirs[at], value)
	}
	return dirs, extraFiles
}

func resolveOutputRecords(output string) []string {
	if strings.Contains(output, resolveRecordSep) {
		return strings.Split(output, resolveRecordSep)
	}
	return strings.Split(output, "\n")
}

func invalidResolvedPath(value string) bool {
	return value == "" || strings.ContainsAny(value, "\x00\r\n")
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
