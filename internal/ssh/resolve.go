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
// Only includes file-based agents that have on-disk sources to resolve via
// their provider facade. For each agent with an EnvVar, the script checks the
// env var first and falls back to the default dir. Dirs (and files) that don't
// exist on the remote are skipped.
func buildResolveScript() string {
	var b strings.Builder
	for _, def := range parser.Registry {
		if !resolveAgentHasOnDiskSource(def) {
			continue
		}
		// Aider has no central store and no safe default root: it writes
		// one .aider.chat.history.md per repository, so after the opt-in
		// change it carries no DefaultDirs and the DefaultDirs loop below
		// never runs for it. Handle it independently so an explicitly
		// configured remote AIDER_DIR still resolves history files. Remote
		// sync emits only discovered .aider.chat.history.md files as tar
		// targets, never the configured code root or the remote $HOME. The
		// shell guard in buildAiderResolveSnippet also drops AIDER_DIR set
		// to literal "$HOME" (or "$HOME/"), so an unscoped override cannot
		// reintroduce a whole-home scan or tar. Local sync is unaffected:
		// it discovers via its provider facade, not this script.
		if def.Type == parser.AgentAider {
			if def.EnvVar != "" {
				b.WriteString(buildAiderResolveSnippet(def.EnvVar))
			}
			continue
		}
		for _, rel := range def.DefaultDirs {
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

// resolveAgentHasOnDiskSource reports whether a file-based agent has
// on-disk sources the resolve script should probe via its provider facade.
// Provider-authoritative agents have a configurable directory, so they must
// stay in the remote resolve set.
func resolveAgentHasOnDiskSource(def parser.AgentDef) bool {
	if !def.FileBased {
		return false
	}
	switch parser.ProviderMigrationModes()[def.Type] {
	case parser.ProviderMigrationProviderAuthoritative:
		_, ok := parser.ProviderFactoryByType(def.Type)
		return ok
	default:
		return false
	}
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
