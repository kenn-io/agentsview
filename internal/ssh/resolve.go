package ssh

import (
	"context"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/parser"
)

// resolveFilePrefix marks lines in the resolve script output that name
// an extra file (not an agent directory) to include in the transfer. It
// is not a valid agent type, so parseResolvedDirs routes it separately.
const resolveFilePrefix = "@file"

// resolveAgentFilePrefix marks lines that name an agent-scoped file to
// transfer without recursively archiving that agent's root directory.
const resolveAgentFilePrefix = "@agentfile"

// resolveForbiddenRootPrefix marks a root that must never be recursively
// transferred, even when another allowed provider root overlaps it.
const resolveForbiddenRootPrefix = "@forbidden"

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
			"av_aider_phys=$(av_phys_file \"$av_entry\") || continue; "+
			"printf '%%s\\000' \"%s:$av_aider_phys\"; "+
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
// Only includes file-backed agents whose local sources are resolved via their
// provider facade. For each agent with an EnvVar, the script checks the env var
// first and falls back to the default dir. Dirs (and files) that don't exist on
// the remote are skipped.
// resolveScriptPhysHelpers defines the script's path-canonicalization
// helpers. av_phys_dir prints a directory's physical path (symlinked
// ancestors resolved); av_phys_file does the same for a file path by
// physically resolving its dirname — including a bare filename (parent
// ".") and a root-level "/file" (parent "/"). av_phys_missing handles a
// path that need not exist at all: it physically resolves the longest
// existing ancestor and rejoins the missing tail literally, so a missing
// forbidden root under a symlinked (or relative) prefix still lands on
// the same spelling canonicalized targets use. Every emitter
// canonicalizes through them so forbidden-root comparisons and tar
// exclusions on the Go side compare one spelling per location: a target
// aliased into an excluded provider's tree via a symlink resolves to the
// same prefix as the forbidden root itself. av_phys_file is a pure path
// transform — existence checks stay with the emitters — so parents must
// exist but the file itself need not.
//
// Physical paths containing CR or LF are refused (return 1) rather than
// emitted: command substitution strips trailing newlines from pwd output,
// so a directory name ending in a newline would otherwise be emitted with
// a clean-looking but WRONG spelling that downstream exclusion would
// guard instead of the real subtree. The sentinel dance (pwd && printf x)
// preserves the true spelling long enough to detect the newline. Such
// paths are unrepresentable end to end anyway — tar -T input is
// newline-delimited and the record parser rejects them — so refusal here
// makes targets fail closed per path, and av_emit_forbidden_root turns
// refusal into a whole-script abort.
const resolveScriptPhysHelpers = "av_nl='\n'\n" +
	"av_cr=$(printf '\\r')\n" +
	"av_phys_dir() { " +
	"av_phys_out=$(CDPATH= cd -P -- \"$1\" 2>/dev/null && pwd && printf x) || return 1; " +
	"av_phys_out=${av_phys_out%??}; " +
	"case \"$av_phys_out\" in *\"$av_nl\"*|*\"$av_cr\"*) return 1;; esac; " +
	"printf '%s' \"$av_phys_out\"; " +
	"}\n" +
	"av_phys_file() { " +
	"case \"$1\" in *\"$av_nl\"*|*\"$av_cr\"*) return 1;; esac; " +
	"av_phys_parent=$(av_phys_dir \"$(dirname -- \"$1\")\") || return 1; " +
	"av_phys_base=$(basename -- \"$1\"); " +
	"case \"$av_phys_parent\" in " +
	"/) printf '%s' \"/$av_phys_base\";; " +
	"*) printf '%s' \"$av_phys_parent/$av_phys_base\";; esac; " +
	"}\n" +
	"av_phys_missing() { " +
	"case \"$1\" in *\"$av_nl\"*|*\"$av_cr\"*) return 1;; esac; " +
	"av_pm_path=\"$1\"; av_pm_tail=\"\"; " +
	"while [ ! -d \"$av_pm_path\" ]; do " +
	"av_pm_base=$(basename -- \"$av_pm_path\"); " +
	"if [ -n \"$av_pm_tail\" ]; then av_pm_tail=\"$av_pm_base/$av_pm_tail\"; " +
	"else av_pm_tail=\"$av_pm_base\"; fi; " +
	"av_pm_parent=$(dirname -- \"$av_pm_path\"); " +
	"[ \"$av_pm_parent\" = \"$av_pm_path\" ] && return 1; " +
	"av_pm_path=\"$av_pm_parent\"; " +
	"done; " +
	"av_pm_phys=$(av_phys_dir \"$av_pm_path\") || return 1; " +
	"if [ -z \"$av_pm_tail\" ]; then printf '%s' \"$av_pm_phys\"; return 0; fi; " +
	"case \"$av_pm_phys\" in " +
	"/) printf '%s' \"/$av_pm_tail\";; " +
	"*) printf '%s' \"$av_pm_phys/$av_pm_tail\";; esac; " +
	"}\n"

func buildResolveScript() string {
	var b strings.Builder
	b.WriteString(
		resolveScriptPhysHelpers +
			"av_emit_agent_file() { " +
			"agent=\"$1\"; " +
			"[ -f \"$2\" ] || return 0; " +
			"file=$(av_phys_file \"$2\") || return 0; " +
			"printf '%s\\000' \"" + resolveAgentFilePrefix + ":$agent:$file\"; " +
			"}\n" +
			"av_emit_windsurf_target() { " +
			"target=\"$1\"; " +
			"case \"$target\" in */) target=\"${target%/}\";; esac; " +
			"workspace=\"$target\"; " +
			"case \"$workspace\" in */workspaceStorage) ;; " +
			"*) workspace=\"$workspace/workspaceStorage\";; esac; " +
			"[ -d \"$workspace\" ] || return; " +
			"av_windsurf_root_emitted=0; " +
			"for av_windsurf_ws in \"$workspace\"/*; do " +
			"[ -d \"$av_windsurf_ws\" ] || continue; " +
			"av_windsurf_db=\"$av_windsurf_ws/" + parser.WindsurfStateDBName + "\"; " +
			"[ -f \"$av_windsurf_db\" ] || continue; " +
			"if [ \"$av_windsurf_root_emitted\" -eq 0 ]; then " +
			"target=$(av_phys_dir \"$target\") || return 0; " +
			"printf '%s\\000' \"" + string(parser.AgentWindsurf) + ":$target\"; " +
			"av_windsurf_root_emitted=1; " +
			"fi; " +
			"for av_windsurf_file in \"$av_windsurf_db\" \"$av_windsurf_db-wal\" \"$av_windsurf_ws/workspace.json\"; do " +
			"av_emit_agent_file \"" + string(parser.AgentWindsurf) + "\" \"$av_windsurf_file\"; " +
			"done; " +
			"done; " +
			"}\n" +
			// RooCode's root is VSCode's whole globalStorage extension
			// directory, which also holds settings/mcp_settings.json
			// (MCP env vars, API keys), caches, and checkpoints. Emit
			// only discovered per-task session files, never the raw
			// directory, mirroring remotesync.resolveRooCodeTarget.
			"av_emit_roocode_target() { " +
			"target=\"$1\"; " +
			"case \"$target\" in */) target=\"${target%/}\";; esac; " +
			"av_roo_tasks=\"$target/tasks\"; " +
			"[ -d \"$av_roo_tasks\" ] || return; " +
			"av_roocode_root_emitted=0; " +
			"for av_roo_task in \"$av_roo_tasks\"/*; do " +
			"[ -d \"$av_roo_task\" ] || continue; " +
			"case \"${av_roo_task##*/}\" in _*) continue;; esac; " +
			"av_roo_history=\"$av_roo_task/history_item.json\"; " +
			"[ -f \"$av_roo_history\" ] || continue; " +
			"if [ \"$av_roocode_root_emitted\" -eq 0 ]; then " +
			"target=$(av_phys_dir \"$target\") || return 0; " +
			"printf '%s\\000' \"" + string(parser.AgentRooCode) + ":$target\"; " +
			"av_roocode_root_emitted=1; " +
			"fi; " +
			"av_emit_agent_file \"" + string(parser.AgentRooCode) + "\" \"$av_roo_history\"; " +
			"av_emit_agent_file \"" + string(parser.AgentRooCode) + "\" \"$av_roo_task/ui_messages.json\"; " +
			"done; " +
			"}\n" +
			// Kilo Legacy's root is VSCode's whole globalStorage
			// extension directory, which can contain MCP settings,
			// API credentials, caches, and other unrelated data. Emit
			// only discovered per-task session files, never the raw
			// directory, mirroring remotesync.resolveKiloLegacyTarget.
			"av_emit_kilo_legacy_target() { " +
			"target=\"$1\"; " +
			"case \"$target\" in */) target=\"${target%/}\";; esac; " +
			"av_kl_tasks=\"$target/tasks\"; " +
			"[ -d \"$av_kl_tasks\" ] || return; " +
			"av_kilo_legacy_root_emitted=0; " +
			"for av_kl_task in \"$av_kl_tasks\"/*; do " +
			"[ -d \"$av_kl_task\" ] || continue; " +
			"[ -L \"$av_kl_task\" ] && continue; " +
			"case \"${av_kl_task##*/}\" in _*|.*) continue;; esac; " +
			"av_kl_metadata=\"$av_kl_task/task_metadata.json\"; " +
			"[ -f \"$av_kl_metadata\" ] || continue; " +
			"if [ \"$av_kilo_legacy_root_emitted\" -eq 0 ]; then " +
			"target=$(av_phys_dir \"$target\") || return 0; " +
			"printf '%s\\000' \"" + string(parser.AgentKiloLegacy) + ":$target\"; " +
			"av_kilo_legacy_root_emitted=1; " +
			"fi; " +
			"av_emit_agent_file \"" + string(parser.AgentKiloLegacy) + "\" \"$av_kl_metadata\"; " +
			"av_emit_agent_file \"" + string(parser.AgentKiloLegacy) + "\" \"$av_kl_task/ui_messages.json\"; " +
			"av_emit_agent_file \"" + string(parser.AgentKiloLegacy) + "\" \"$av_kl_task/api_conversation_history.json\"; " +
			"done; " +
			"}\n" +
			// Poolside's root is the application-data directory, which
			// may contain config, caches, or credentials. Only the
			// trajectories/ subdirectory is parsed, so only it must be
			// archived during remote sync, mirroring
			// remotesync.resolvePoolsideTarget.
			"av_emit_poolside_target() { " +
			"target=\"$1\"; " +
			"case \"$target\" in */) target=\"${target%/}\";; esac; " +
			"case \"${target##*/}\" in " +
			"trajectories) [ -d \"$target\" ] && " +
			"target=$(av_phys_dir \"$target\") && " +
			"printf '%s\\000' \"" + string(parser.AgentPoolside) + ":$target\";; " +
			"*) av_poolside_traj=\"$target/trajectories\"; " +
			"[ -d \"$av_poolside_traj\" ] && " +
			"av_poolside_traj=$(av_phys_dir \"$av_poolside_traj\") && " +
			"printf '%s\\000' \"" + string(parser.AgentPoolside) + ":$av_poolside_traj\";; " +
			"esac; " +
			"}\n" +
			// Provider-specific narrowing keys on the override's literal
			// basename (a symlink named "trajectories" or
			// "workspaceStorage" is meaningful as spelled), so
			// canonicalization happens after narrowing, at each
			// emitter's root-emission printf — never before dispatch.
			"av_emit_target() { " +
			"agent=\"$1\"; " +
			"target=\"$2\"; " +
			"if [ \"$agent\" = \"" + string(parser.AgentWindsurf) + "\" ]; then " +
			"av_emit_windsurf_target \"$target\"; " +
			"return; " +
			"fi; " +
			"if [ \"$agent\" = \"" + string(parser.AgentRooCode) + "\" ]; then " +
			"av_emit_roocode_target \"$target\"; " +
			"return; " +
			"fi; " +
			"if [ \"$agent\" = \"" + string(parser.AgentKiloLegacy) + "\" ]; then " +
			"av_emit_kilo_legacy_target \"$target\"; " +
			"return; " +
			"fi; " +
			"if [ \"$agent\" = \"" + string(parser.AgentPoolside) + "\" ]; then " +
			"av_emit_poolside_target \"$target\"; " +
			"return; " +
			"fi; " +
			"[ -d \"$target\" ] || return 0; " +
			"target=$(av_phys_dir \"$target\") || return 0; " +
			"printf '%s\\000' \"$agent:$target\"; " +
			"}\n" +
			"av_emit_extra_file() { " +
			"[ -f \"$1\" ] || return 0; " +
			"file=$(av_phys_file \"$1\") || return 0; " +
			"printf '%s\\000' \"" + resolveFilePrefix + ":$file\"; " +
			"}\n" +
			// A forbidden root that exists is emitted by its physical
			// spelling so it shares a prefix with canonicalized targets;
			// a missing one resolves through its longest existing
			// ancestor (av_phys_missing) so a root spelled via a
			// symlinked or relative prefix still guards the physical
			// subtree where its contents would appear.
			// A forbidden root whose physical path cannot be represented
			// (av_phys_dir and av_phys_missing refuse CR/LF spellings)
			// aborts the whole resolve: skipping the record would
			// silently drop the exclusion boundary, and emitting a
			// newline path would let the record parser reject it later —
			// aborting here fails closed at the source.
			"av_emit_forbidden_root() { " +
			"dir=\"$1\"; [ -n \"$dir\" ] || dir=\"$2\"; " +
			"[ -n \"$dir\" ] || return 0; " +
			"if [ -d \"$dir\" ]; then dir=$(av_phys_dir \"$dir\") || exit 1; " +
			"else dir=$(av_phys_missing \"$dir\") || exit 1; fi; " +
			"printf '%s\\000' \"" + resolveForbiddenRootPrefix + ":$dir\"; " +
			"}\n" +
			"av_has_hermes_transcript() { " +
			"av_hermes_transcript_dir=\"$1\"; " +
			"[ -d \"$av_hermes_transcript_dir\" ] || return 1; " +
			"for av_hermes_transcript in \"$av_hermes_transcript_dir\"/*.jsonl \"$av_hermes_transcript_dir\"/session_*.json; do " +
			"[ -f \"$av_hermes_transcript\" ] && return 0; done; return 1; " +
			"}\n" +
			"av_emit_hermes_target() { " +
			"target=\"$1\"; " +
			"av_hermes_allow_flat=\"${2:-1}\"; " +
			"while [ \"$target\" != \"/\" ] && [ \"${target%/}\" != \"$target\" ]; do target=\"${target%/}\"; done; " +
			"av_hermes_parent=\"${target%/*}\"; av_hermes_grandparent=\"${av_hermes_parent%/*}\"; " +
			"if [ \"${av_hermes_parent##*/}\" = profiles ] && [ \"${av_hermes_grandparent##*/}\" = .hermes ]; then av_hermes_allow_flat=0; fi; " +
			"if [ \"$av_hermes_allow_flat\" -eq 0 ]; then " +
			"av_hermes_root=\"$target\"; av_hermes_sessions=\"$target/sessions\"; " +
			"else case \"$target\" in " +
			"*/sessions) av_hermes_root=\"${target%/*}\"; av_hermes_sessions=\"$target\";; " +
			"*/state.db) av_hermes_root=\"${target%/*}\"; av_hermes_sessions=\"$av_hermes_root/sessions\";; " +
			"*) av_hermes_root=\"$target\"; av_hermes_sessions=\"$target/sessions\";; " +
			"esac; fi; " +
			"av_hermes_state=\"$av_hermes_root/state.db\"; " +
			"if [ -d \"$av_hermes_sessions\" ]; then " +
			"av_emit_target \"" + string(parser.AgentHermes) + "\" \"$av_hermes_sessions\"; " +
			"for av_hermes_file in \"$av_hermes_state\" \"$av_hermes_state-wal\" \"$av_hermes_state-shm\" \"$av_hermes_state-journal\"; do " +
			"av_emit_extra_file \"$av_hermes_file\"; done; " +
			"elif [ -f \"$av_hermes_state\" ]; then " +
			"av_hermes_state_phys=$(av_phys_file \"$av_hermes_state\") && " +
			"printf '%s\\000' \"" + string(parser.AgentHermes) + ":$av_hermes_state_phys\"; " +
			"for av_hermes_file in \"$av_hermes_state-wal\" \"$av_hermes_state-shm\" \"$av_hermes_state-journal\"; do " +
			"av_emit_extra_file \"$av_hermes_file\"; done; " +
			"elif [ \"$av_hermes_allow_flat\" -eq 1 ] && av_has_hermes_transcript \"$target\"; then " +
			"av_emit_target \"" + string(parser.AgentHermes) + "\" \"$target\"; fi; " +
			"}\n" +
			"av_emit_hermes_profiles() { " +
			"av_hermes_profiles=\"$1\"; " +
			"for av_hermes_prof in \"$av_hermes_profiles\"/*; do " +
			"[ -L \"$av_hermes_prof\" ] && continue; " +
			"[ -d \"$av_hermes_prof\" ] || continue; " +
			"av_emit_hermes_target \"$av_hermes_prof\" 0; " +
			"done; " +
			"}\n" +
			"av_emit_hermes_dir() { " +
			"dir=\"$1\"; [ -n \"$dir\" ] || dir=\"$2\"; " +
			"while [ \"$dir\" != \"/\" ] && [ \"${dir%/}\" != \"$dir\" ]; do dir=\"${dir%/}\"; done; " +
			"av_hermes_parent=\"${dir%/*}\"; " +
			"if [ \"${dir##*/}\" = profiles ] && [ \"${av_hermes_parent##*/}\" = .hermes ]; then " +
			"av_emit_hermes_profiles \"$dir\"; return; fi; " +
			"av_emit_hermes_target \"$dir\"; " +
			"}\n" +
			"av_emit_dir() { " +
			"dir=\"$1\"; " +
			"[ -n \"$dir\" ] || dir=\"$2\"; " +
			"av_emit_target \"$3\" \"$dir\"; " +
			"}\n" +
			"av_emit_rooted_dir() { " +
			"dir=\"$1\"; " +
			"root=\"$2\"; " +
			"[ -z \"$dir\" ] && [ -n \"$root\" ] && dir=\"$root$3\"; " +
			"[ -n \"$dir\" ] || dir=\"$4\"; " +
			"av_emit_target \"$5\" \"$dir\"; " +
			"}\n" +
			"av_emit_codex_index() { " +
			"idx=\"${dir%/*}/" + parser.CodexSessionIndexFilename + "\"; " +
			"av_emit_extra_file \"$idx\"; " +
			"}\n",
	)
	for _, def := range parser.Registry {
		if def.RemoteSyncExcluded {
			for _, rel := range def.DefaultDirs {
				fmt.Fprintf(&b,
					"av_emit_forbidden_root \"%s\" \"$HOME/%s\"\n",
					remoteEnvExpansion(def.EnvVar), rel,
				)
			}
			continue
		}
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
			if def.Type == parser.AgentHermes {
				fmt.Fprintf(&b,
					"av_emit_hermes_dir \"%s\" \"%s\"\n",
					remoteEnvExpansion(def.EnvVar), defaultDir,
				)
				continue
			}
			if def.DefaultRootEnvVar != "" {
				rootTail := remoteDefaultRootTail(rel)
				rootSuffix := ""
				if rootTail != "" {
					rootSuffix = "/" + rootTail
				}
				fmt.Fprintf(&b,
					"av_emit_rooted_dir \"%s\" \"%s\" \"%s\" \"%s\" %s\n",
					remoteEnvExpansion(def.EnvVar),
					remoteEnvExpansion(def.DefaultRootEnvVar),
					rootSuffix, defaultDir, string(def.Type),
				)
			} else {
				fmt.Fprintf(&b,
					"av_emit_dir \"%s\" \"%s\" %s\n",
					remoteEnvExpansion(def.EnvVar), defaultDir,
					string(def.Type),
				)
			}
			// Codex stores renameable session titles in
			// session_index.jsonl, which sits beside (not inside)
			// sessions/ and archived_sessions/. Emit it so renames
			// import on remote hosts too. ${dir%/*} is the parent.
			if def.Type == parser.AgentCodex {
				b.WriteString("av_emit_codex_index\n")
			}
		}
		// Hermes named defaults are replacements, not additions, when the
		// sessions override is set. Each emitted profile includes its state DB
		// and live SQLite companions as well as transcript sessions.
		if def.Type == parser.AgentHermes {
			fmt.Fprintf(&b,
				"if [ -z \"%s\" ]; then "+
					"av_emit_hermes_profiles \"$HOME/.hermes/profiles\"; fi\n",
				remoteEnvExpansion(def.EnvVar),
			)
		}
	}
	// Ensure exit 0 — the last [ -d ]/[ -f ] test may fail if that
	// path doesn't exist, which would make sh exit non-zero.
	b.WriteString("true\n")
	return b.String()
}

func remoteEnvExpansion(envVar string) string {
	if envVar == "" {
		return ""
	}
	return "${" + envVar + ":-}"
}

// BuildResolveScriptForTest exposes the SSH resolver script to
// internal/remotesync parity tests.
func BuildResolveScriptForTest() string {
	return buildResolveScript()
}

func remoteDefaultRootTail(rel string) string {
	cleaned := path.Clean(rel)
	if _, tail, ok := strings.Cut(cleaned, "/"); ok && tail != "" {
		return tail
	}
	return ""
}

// resolveAgentHasOnDiskSource reports whether a file-backed agent has local
// sources the resolve script should probe via the provider facade.
func resolveAgentHasOnDiskSource(def parser.AgentDef) bool {
	if def.RemoteSyncExcluded {
		return false
	}
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

// parseResolvedTargets parses script output into deduplicated agent root
// paths, agent-scoped files, extra files (records tagged with
// resolveFilePrefix), and forbidden roots (records tagged with
// resolveForbiddenRootPrefix). Generated resolver output is
// NUL-delimited so remote paths containing newlines cannot inject extra
// records; newline-delimited input is accepted only for older tests and
// defensive compatibility, and only that legacy mode trims whitespace —
// NUL-delimited records are preserved exactly, so a path with leading or
// trailing whitespace keeps the spelling every downstream comparison and
// tar exclusion depends on. Most agent targets are directories; Aider
// targets are individual .aider.chat.history.md files. Skips empty
// records, empty values, and values containing record separators — except
// forbidden-root records, which fail the parse instead: silently dropping
// an exclusion boundary would archive the excluded provider's state.
func parseResolvedTargets(
	output string,
) (map[parser.AgentType][]string, map[parser.AgentType][]string, []string, []string, error) {
	dirs := make(map[parser.AgentType][]string)
	files := make(map[parser.AgentType][]string)
	var extraFiles []string
	var forbiddenRoots []string
	seenDir := make(map[parser.AgentType]map[string]struct{})
	seenFile := make(map[string]struct{})
	seenAgentFile := make(map[parser.AgentType]map[string]struct{})
	seenForbiddenRoot := make(map[string]struct{})
	nulDelimited := strings.Contains(output, resolveRecordSep)
	for _, record := range resolveOutputRecords(output) {
		if !nulDelimited {
			record = strings.TrimSpace(record)
		}
		if record == "" {
			continue
		}
		key, value, ok := strings.Cut(record, ":")
		if key == resolveForbiddenRootPrefix {
			if !ok || invalidResolvedPath(value) {
				return nil, nil, nil, nil, fmt.Errorf(
					"resolve output: forbidden-root record %q is empty or "+
						"unrepresentable; refusing to sync without its "+
						"exclusion boundary", record)
			}
			cleaned := path.Clean(value)
			if _, dup := seenForbiddenRoot[cleaned]; dup {
				continue
			}
			seenForbiddenRoot[cleaned] = struct{}{}
			forbiddenRoots = append(forbiddenRoots, cleaned)
			continue
		}
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
		if key == resolveAgentFilePrefix {
			agent, pathValue, ok := strings.Cut(value, ":")
			if !ok || invalidResolvedPath(pathValue) {
				continue
			}
			at := parser.AgentType(agent)
			if at == "" {
				continue
			}
			seen, ok := seenAgentFile[at]
			if !ok {
				seen = make(map[string]struct{})
				seenAgentFile[at] = seen
			}
			if _, dup := seen[pathValue]; dup {
				continue
			}
			seen[pathValue] = struct{}{}
			files[at] = append(files[at], pathValue)
			continue
		}
		at := parser.AgentType(key)
		if at == parser.AgentAider &&
			path.Base(value) != parser.AiderHistoryFileName() {
			continue
		}
		seen, ok := seenDir[at]
		if !ok {
			seen = make(map[string]struct{})
			seenDir[at] = seen
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		dirs[at] = append(dirs[at], value)
	}
	return dirs, files, extraFiles, forbiddenRoots, nil
}

func parseResolvedDirs(
	output string,
) (map[parser.AgentType][]string, []string, error) {
	dirs, _, extraFiles, _, err := parseResolvedTargets(output)
	return dirs, extraFiles, err
}

// ParseResolvedTargetsWithFilesForTest exposes SSH resolver output parsing
// to internal/remotesync parity tests, discarding forbidden roots that
// those tests don't assert on.
func ParseResolvedTargetsWithFilesForTest(
	output string,
) (map[parser.AgentType][]string, map[parser.AgentType][]string, []string, error) {
	dirs, files, extraFiles, _, err := parseResolvedTargets(output)
	return dirs, files, extraFiles, err
}

func resolveOutputRecords(output string) []string {
	if strings.Contains(output, resolveRecordSep) {
		return strings.Split(output, resolveRecordSep)
	}
	return strings.Split(output, "\n")
}

// invalidResolvedPath rejects resolved-path spellings that downstream
// exclusion machinery cannot represent faithfully: NUL/CR/LF collide
// with the record and tar -T framings, and non-UTF-8 bytes are mangled
// by both escapeTarExcludeGlob's rune iteration and the Python
// snapshot's JSON marshaling, so a boundary spelled with them would
// silently guard the wrong subtree. Target records are dropped per
// path; forbidden-root records fail the whole sync.
func invalidResolvedPath(value string) bool {
	return value == "" ||
		strings.ContainsAny(value, "\x00\r\n") ||
		!utf8.ValidString(value)
}

// resolveDirs runs the resolve script on the remote host via SSH and
// returns the discovered agent directories plus extra sibling files
// (such as Codex's session_index.jsonl) to include in the transfer.
func resolveDirs(
	ctx context.Context,
	host, user string, port int, sshOpts []string,
) (map[parser.AgentType][]string, map[parser.AgentType][]string, []string, []string, error) {
	script := buildResolveScript()
	out, err := runSSHScript(ctx, host, user, port, sshOpts, script)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("resolve dirs: %w", err)
	}
	dirs, files, extraFiles, forbiddenRoots, err := parseResolvedTargets(string(out))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("resolve dirs: %w", err)
	}
	return dirs, files, extraFiles, forbiddenRoots, nil
}
