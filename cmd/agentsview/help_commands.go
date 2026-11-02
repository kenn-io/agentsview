package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type commandHelpText struct {
	long     string
	examples string
	extra    string
}

const omittedCommonFlagsAnnotation = "agentsview.omitted_common_flags"

func markOmittedCommonFlagsForHelp(cmd *cobra.Command, names ...string) {
	if len(names) == 0 {
		return
	}
	if cmd.Annotations == nil {
		cmd.Annotations = make(map[string]string)
	}
	cmd.Annotations[omittedCommonFlagsAnnotation] = strings.Join(names, " ")
}

// Existing command descriptions own their detailed limits. This table fills
// missing concept explanations and examples without replacing those contracts.
var agentCommandHelp = map[string]commandHelpText{
	"serve": {
		examples: "  agentsview serve --no-browser\n  agentsview serve --background",
	},
	"serve status": {
		long:     "Show whether a server for this data directory is running and print its URL. The result is human text, including when no server is running; there is no JSON status flag.",
		examples: "  agentsview serve status",
	},
	"serve stop": {
		examples: "  agentsview serve stop",
	},
	"serve restart": {
		examples: "  agentsview serve restart",
	},
	"daemon": {
		examples: "  agentsview daemon status\n  agentsview daemon start",
	},
	"daemon start": {
		long:     "Start the writable server in the background using saved configuration, without opening a browser. A compatible running daemon is reused. This is also the explicit startup step when automatic startup is disabled.",
		examples: "  agentsview daemon start",
	},
	"daemon status": {
		long:     "Print the writable daemon's status and URL for this data directory. No running daemon is a successful status query with human output, not a failing health check. This command has no --json flag.",
		examples: "  agentsview daemon status",
	},
	"daemon stop": {
		long:     "Stop only the writable daemon, including its web UI, source sync, and file watchers. Read-only PostgreSQL and DuckDB servers are left running.",
		examples: "  agentsview daemon stop",
	},
	"daemon restart": {
		long:     "Restart the writable background server using the saved configuration. Use this after changing settings or when a running daemon needs replacement; read-only mirror servers are separate.",
		examples: "  agentsview daemon restart",
	},
	"report stats": {
		long: "Summarize sessions, activity, and outcomes within a bounded time window. " +
			"--since and --until accept Nd, Nh, YYYY-MM-DD, or RFC3339; --since defaults to 28d. " +
			"Use an IANA --timezone for calendar grouping. One-shot and automated sessions are excluded unless included explicitly. " +
			"--include-project and --exclude-project are repeatable. GitHub outcome enrichment is opt-in and may contact GitHub.",
	},
	"session health": {
		extra: "The list is bounded by --limit and includes one-shot and automated sessions. Use a full ID from session list when a displayed short ID is ambiguous.",
	},
	"project list": {
		long: "List archived projects and their session counts. Use a returned project name with session list --project or analytics filters to narrow later queries.",
	},
	"openapi": {
		long:     "Print the HTTP API's OpenAPI 3.1 schema from this binary without contacting a running server. JSON is the default; --yaml selects YAML. Use this to inspect API operations and response shapes.",
		examples: "  agentsview openapi > openapi.json\n  agentsview openapi --yaml > openapi.yaml",
	},
	"source import": {
		long: "Import an exported conversation file into the local archive using an explicit --type. Supported formats are claude-ai, chatgpt, and gemini-apps. Claude.ai can instead use --type claude-ai --sync with the configured Chrome integration. --replace grants replacement permission only for the named archived sessions.",
	},
	"update": {
		long:     "Check for a newer release and, for supported direct installations, install it after confirmation. --check only reports availability. Package-managed installations must be upgraded through their package manager.",
		examples: "  agentsview update --check\n  agentsview update",
	},
	"kata": {
		long:     "Inspect the optional Kata issue-tracker connection. Use status to check whether the configured instance is reachable and ready.",
		examples: "  agentsview kata status",
	},
	"mcp": {
		examples: "  agentsview mcp\n  agentsview mcp --profile memory\n  agentsview mcp status",
	},
	"session": {
		long: "Find sessions, inspect transcripts and tool calls, search content, and read usage. " +
			"Start with list and copy a full opaque ID into later commands. " +
			"Local reads use the daemon; --server URL selects an explicit remote daemon and --pg selects configured PostgreSQL. " +
			"Those backend selectors are mutually exclusive. --server-token-file or AGENTSVIEW_SERVER_TOKEN supplies remote authentication. " +
			"Most read commands offer human or JSON output; watch and export have dedicated stream formats.",
		examples: "  agentsview session list --since 7d --json\n  agentsview session get <id> --json\n  agentsview session list --server http://127.0.0.1:8080 --json",
	},
	"session list": {
		long: "Find sessions with metadata filters and stable cursor pagination. " +
			"One-shot, automated, and child sessions are excluded by default; use the corresponding --include-* flags to include them. " +
			"--since accepts 12h, 14d, 2w, 3m (months), 1y, or YYYY-MM-DD; --active-since accepts RFC3339 instead. " +
			"Choose comma-separated --sort keys from the flag help, optionally with :asc or :desc. " +
			"JSON includes full session IDs and a next_cursor. The cursor does not retain filters: repeat the same backend, filters, and sort (including --reverse) on every page. Changing the sort rejects the cursor; changing filters changes the selection. Machine filters accept exact IDs, aliases, or a unique whole display name.",
		examples: "  agentsview session list --since 2026-10-01 --project example-project --sort recent --limit 20 --json\n  agentsview session list --since 2026-10-01 --project example-project --sort recent --limit 20 --cursor <cursor> --json\n  agentsview session list --agent codex --sort messages:desc --limit 20 --json\n  agentsview session list --agent codex --sort messages:desc --limit 20 --cursor <cursor> --json",
	},
	"session get": {
		long: "Read one session's metadata, outcome, and health signals. Copy its full ID from list or search. " +
			"Bare provider IDs are retried with known prefixes, while a canonical agent-prefixed or host-prefixed ID is authoritative. " +
			"Bare Codebuff/Freebuff timestamps use --machine (default local) to select their source. Explicit --server and --pg reads do not support bare timestamp lookup; use a canonical stored ID there.",
		examples: "  agentsview session get <id> --json",
	},
	"session messages": {
		long: "Read a bounded transcript page by zero-based message ordinal. " +
			"Use --from and --limit with --direction asc or desc, or center a window with --around and optional --before/--after. " +
			"--before/--after require --around; an around window cannot also use --from or an explicit --direction. " +
			"For descending pages, omitting --from selects the newest page; an explicit --from 0 means ordinal zero.",
		examples: "  agentsview session messages <id> --from 0 --limit 20 --json\n  agentsview session messages <id> --around 40 --before 5 --after 5 --json\n  agentsview session messages <id> --direction desc --limit 20 --json",
	},
	"session tool-calls": {
		long:     "List the tool calls recorded in one session with their transcript ordinal, timestamp, tool name, and category. Use messages at the corresponding ordinal to inspect the surrounding conversation.",
		examples: "  agentsview session tool-calls <id> --json",
	},
	"session search": {
		long: "Search archived message and tool content with redacted snippets. " +
			"The default is substring search across available sources; --regex selects RE2 expressions. " +
			"--in accepts messages, thinking, tool_input, and tool_result; thinking is supported for substring/regex on the documented stores. " +
			"Choose at most one of --regex, --fts, --semantic, or --hybrid. " +
			"FTS and semantic/hybrid modes search messages only; semantic modes need an embedding index. " +
			"--context adds bounded surrounding messages; JSON returns a numeric next_cursor for further matches; repeat the query, search mode, backend, and filters when continuing. " +
			"Time filters use the session-list --since grammar. --reveal explicitly disables secret redaction.",
		examples: "  agentsview session search 'connection reset' --since 14d --in messages,tool_result --json\n  agentsview session search 'connection AND reset' --fts --json\n  agentsview session search 'why the build failed' --hybrid --scope top --json",
	},
	"session usage": {
		examples: "  agentsview session usage <id> --json\n  agentsview session usage <id> --own-only --no-sync --json",
		extra:    "Exit 0 means token data, cost, or plan figures are present. Missing sessions use exit 2. A report without any of those figures is printed before a silent exit 3.",
	},
	"session usage-batch": {
		long:     "Read usage for several IDs through one backend selection, preserving argument order and duplicates. Each result carries its own exit_code: 0 for figures, 2 for missing, 3 for no figures, or 1 for a per-ID failure. Per-ID failures are report data and do not make the overall command fail; backend setup or output failures do.",
		examples: "  agentsview session usage-batch <id> <other-id> --json\n  agentsview session usage-batch <id> <other-id> --no-sync --own-only --json",
	},
	"session watch": {
		long:     "Subscribe to one session and emit newline-delimited JSON events as it changes, until interrupted or the subscription ends. This is a stream, not a single JSON document; --json and --format are rejected. The session must already resolve before the subscription starts.",
		examples: "  agentsview session watch <id>",
	},
	"session export": {
		long:     "Export provider-specific source content for one locally archived session. Aider emits the selected run as Markdown; Windsurf and Trae reconstruct session JSON; supported shared or database sources emit only the selected session as JSONL. Other file sources are copied as bytes. Local source access is required, and usage-only archives cannot export it. --server, --pg, --json, and --format are rejected. Use the top-level export group for normalized archive reports.",
		examples: "  agentsview session export <id> > session-export.out",
	},
	"session sync": {
		long:     "Refresh one session by source path or ID and return its metadata. This is a write operation and requires a writable backend. An explicit remote --server interprets source paths on that server's host; --pg and read-only daemons cannot perform the refresh.",
		examples: "  agentsview session sync <id> --json\n  agentsview session sync ./session.jsonl --json",
	},
	"report usage daily": {
		long: "Summarize daily token usage and cost, optionally broken down by model. " +
			"The default window is the last 30 days; --all removes that default. " +
			"--since and --until accept Nd, Nh, YYYY-MM-DD, or RFC3339; a relative --since is measured back from --until when supplied. " +
			"Use an IANA --timezone for date boundaries. --no-sync skips on-demand source refresh; --offline reads the local archive with fallback pricing. " +
			"Plan-only sessions and partly captured sessions may be reported separately from priced token totals.",
		examples: "  agentsview report usage daily --since 28d --breakdown --json\n  agentsview report usage daily --since 2026-10-01 --until 2026-10-07 --timezone UTC --offline --json",
	},
	"report usage statusline": {
		long:     "Print today's cost in one short line, or a JSON document with the date and exact microdollar amount. Today follows the local system timezone. --agent narrows the total; --no-sync skips refresh and --offline reads the local archive with fallback pricing.",
		examples: "  agentsview report usage statusline\n  agentsview report usage statusline --agent codex --json",
	},
	"source cursor sync-usage": {
		long:     "Fetch Cursor organization usage through the configured admin API and store it in the local archive. This is an ingestion command, not a cost display. It requires a Cursor admin API key; --since and --until use YYYY-MM-DD dates. Explicit --email or --user-id overrides the configured member selection.",
		examples: "  agentsview source cursor sync-usage --since 2026-10-01 --until 2026-10-07",
	},
	"report activity": {
		long: "Report activity totals, peak concurrency, breakdowns, and a page of sessions. " +
			"Use --preset day, week, or month with an optional --date YYYY-MM-DD; custom ranges require --preset custom and RFC3339 --from/--to bounds. " +
			"Use an IANA --timezone and --bucket 5m, 15m, 1h, 1d, or 1w. " +
			"JSON includes the detailed bucket timeline and session continuation values. " +
			"For daemon paging, keep the returned report ID and cursor together so subsequent pages use the same report generation. --offline reads local archived data with fallback pricing.",
	},
	"recall": {
		long:     "Build and inspect accepted knowledge derived from past conversations. Recall entries are curated records, separate from raw transcript search. Query or brief reads accepted entries; import brings reviewed records into the archive, while extract manages a configured extraction recipe.",
		examples: "  agentsview recall list --json\n  agentsview recall query 'deployment troubleshooting' --json",
	},
	"recall extract": {
		long:     "Manage extraction recipes that propose recalled knowledge from session content. Inspect status and preview candidates before choosing an active recipe or running extraction; accepted recall records remain a separate reviewed layer.",
		examples: "  agentsview recall extract status\n  agentsview recall extract preview --session <id>",
	},
	"insight": {
		long:     "Read stored Activity Insights or ask a configured analysis agent to generate a new one through the daemon. Listing and retrieving inspect stored results; generation can invoke an external agent and needs the corresponding setup.",
		examples: "  agentsview insight list --json\n  agentsview insight get <id> --json",
	},
	"friction": {
		long:     "Build and read heuristic session-review digests, findings, and recurring patterns. Stored digests summarize observed friction rather than proving an outcome; use the linked sessions to inspect evidence. Generation and stored-result queries are separate subcommands.",
		examples: "  agentsview friction findings --json\n  agentsview friction patterns --json",
	},
	"memory": {
		long:     "Integrate recalled conversation knowledge into an agent's session-start lifecycle. The session-start action selects local, hosted-contributor, or hosted-reader mode and uses bounded lifecycle work; it is intended for the configured hook or plugin integration.",
		examples: "  agentsview memory session-start --help",
	},
	"skills": {
		long:     "Inspect or install AgentsView's skill files for coding-agent harnesses. Listing describes available files and their install state; installation writes the selected harness integration. Explicit server options bake connection details into generated skill examples.",
		examples: "  agentsview skills list\n  agentsview skills install --help",
	},
	"secrets": {
		long:     "Inspect redacted secret findings or run the full scanner over archived session content. Normal sync uses fast definite rules; a full scan also includes candidate rules. Findings are redacted by default, and revealing values requires an explicit option.",
		examples: "  agentsview secrets list --json\n  agentsview secrets scan --help",
	},
	"index embeddings": {
		long: "Build and manage the embedding generations used by semantic and hybrid search. Configure an embedding provider and the intended storage backend before building. Listing inspects generations; activation and retirement control which generation serves queries.",
	},
	"export": {
		long:     "Export normalized reports from the local archive, distinct from session export's provider-specific source content. Sessions, conversations, and closed UTC reporting hours or date ranges have their own schemas and pagination rules. Preserve cursor/reset and conversation-revision errors when resuming an export.",
		examples: "  agentsview export sessions --help\n  agentsview export conversations --help",
	},
	"export conversations": {
		long: "Export stored conversation changes and retrieve message text in bounded, revision-checked chunks. " +
			"First walk changes, which lists message IDs and revisions without exporting their text. " +
			"Use cursor to continue an unfinished walk, or checkpoint only after a walk completes. " +
			"Then request message chunks using the returned session ID, message ID, revision, and database ID; keep the same text mode. " +
			"These commands emit their own JSON documents without --json. Archive reconciliation and changed message revisions use their dedicated export exit codes.",
		examples: "  agentsview export conversations changes --text-mode stored --limit 100\n  agentsview export conversations changes --text-mode stored --limit 100 --cursor <cursor>\n  agentsview export conversations message <session-id> <message-id> --text-mode stored --revision <revision> --database-id <database-id> --max-bytes 65536",
	},
	"export conversations changes": {
		long: "List changed messages without their text. Continue a partial walk with the returned cursor; after the walk finishes, retain its checkpoint for the next changes walk. " +
			"--cursor and --checkpoint are mutually exclusive. Keep the text mode consistent with later message retrieval, and preserve reconciliation errors when the archive generation changes.",
		examples: "  agentsview export conversations changes --text-mode stored --limit 100\n  agentsview export conversations changes --text-mode stored --limit 100 --checkpoint <checkpoint>",
	},
	"export conversations message": {
		long: "Read one bounded chunk of the exact message revision named by a changes result. " +
			"Supply its session ID, message ID, --revision, and --database-id, using the same --text-mode. " +
			"Continue with the returned UTF-8 byte offset rather than calculating character offsets. A changed revision or archive generation requires handling the export error before retrying.",
		examples: "  agentsview export conversations message <session-id> <message-id> --text-mode stored --revision <revision> --database-id <database-id> --max-bytes 65536",
	},
	"doctor": {
		long:     "Inspect configuration and archive health when sync or memory behavior is unexpected. The sync and memory reports focus on their own subsystem; read the selected report before changing sources or rebuilding data.",
		examples: "  agentsview doctor sync\n  agentsview doctor memory --help",
	},
	"replica pg": {
		long: "Mirror archived SQLite data to PostgreSQL and serve the mirror through a read-only API. Configure a PostgreSQL target before pushing or inspecting status. Hosted provisioning, vector maintenance, and the optional push service have separate commands; mirror reads do not ingest local provider sources.",
	},
	"replica pg vectors": {
		long:     "Inspect PostgreSQL embedding generations and explicitly remove a generation's stored vectors. This operates on the configured PostgreSQL mirror; it is separate from building the local embedding index.",
		examples: "  agentsview replica pg vectors list",
	},
	"replica pg service": {
		long:     "Install and manage the optional background service that continuously pushes local archive changes to PostgreSQL. Service installation changes the host's service manager; status and logs inspect the existing service.",
		examples: "  agentsview replica pg service status\n  agentsview replica pg service install --help",
	},
	"replica duckdb": {
		long: "Build a disposable DuckDB mirror from the persistent SQLite archive, inspect sync status, or serve read-only queries from that mirror. Quack exposes a DuckDB file over its separate remote protocol. Configure the intended path or connection before using these commands.",
	},
	"replica duckdb quack": {
		long:     "Expose a local DuckDB mirror through the Quack remote protocol. This listener is separate from the AgentsView HTTP API and requires its own configured authentication and bind settings.",
		examples: "  agentsview replica duckdb quack serve --help",
	},
	"replica clickhouse": {
		long: "Mirror local archived data to ClickHouse and serve read-only queries against that mirror. Configure a ClickHouse target before push, status, or serving. The optional service keeps pushing local changes in the background.",
	},
	"replica clickhouse service": {
		long:     "Install and manage the optional background service that pushes archive changes to ClickHouse. Check status or logs before restarting it; installation and removal change the host's service manager.",
		examples: "  agentsview replica clickhouse service status\n  agentsview replica clickhouse service install --help",
	},
	"archive": {
		long:     "Retain and verify original provider sources so they can be reparsed or moved with their archive identities. Captures, accepted generations, backups, and restores have distinct commands. Read each command's stopping and destination rules before moving a live archive.",
		examples: "  agentsview archive sources\n  agentsview archive verify --help",
	},
	"db": {
		long:     "Maintain the persistent local SQLite archive. Compaction, retained-image migration or removal, and machine adoption have different safety and shutdown requirements. Inspect the chosen command's help and keep the original archive when preparing a new image.",
		examples: "  agentsview db compact --help\n  agentsview db migrate --help",
	},
	"source raw": {
		long: "Capture and upload provider-owned source artifacts to the configured raw-sync server, keeping durable local progress. Inspect status before starting the continuous watcher or a bounded backfill. This moves original sources, rather than only derived session summaries.",
	},
	"capture": {
		long:     "Capture one non-interactive Claude or Codex execution and produce a recoverable usage result. run launches the exact producer command after --; report recovers or replays its capture. Producer exit codes and capture reporting failures keep their dedicated exit contracts.",
		examples: "  agentsview capture run --help\n  agentsview capture report --help",
	},
	"source chrome": {
		long: "Set up the Chrome native-host integration used by Claude.ai synchronization. Setup installs integration files; importing conversations is performed through the import command with the Claude.ai sync option.",
	},
	"signing": {
		long:     "Prepare private signing-key files and durable replay state for native HTTP request signing. Commands create files explicitly and do not print the signing secret. Keep key and replay-state paths private when configuring the corresponding service.",
		examples: "  agentsview signing keygen --help\n  agentsview signing init-replay --help",
	},
}

func applyCLIHelp(root *cobra.Command) {
	var visit func(*cobra.Command)
	visit = func(cmd *cobra.Command) {
		if cmd.Hidden {
			return
		}
		path := commandPath(root, cmd)
		if help, ok := agentCommandHelp[path]; ok {
			if cmd.Long == "" {
				cmd.Long = help.long
			} else if help.extra != "" {
				cmd.Long += "\n\n" + help.extra
			}
			if cmd.Example == "" {
				cmd.Example = help.examples
			}
		}
		if cmd.Annotations == nil {
			cmd.Annotations = make(map[string]string)
		}
		cmd.Annotations["agentsview.common_flags"] = commonCLIFlagsHelp(cmd)
		for _, child := range cmd.Commands() {
			visit(child)
		}
	}
	visit(root)
	root.SetUsageTemplate(compactCLIUsageTemplate())
}

// Store rendered, command-local help metadata instead of a global template
// callback or mutable invocation context. The command tree is complete here.
func commonCLIFlagsHelp(cmd *cobra.Command) string {
	var names []string
	var owners []string
	omitted := make(map[string]struct{})
	for name := range strings.FieldsSeq(cmd.Annotations[omittedCommonFlagsAnnotation]) {
		omitted[name] = struct{}{}
	}
	cmd.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		if _, omit := omitted[flag.Name]; omit {
			return
		}
		names = append(names, "--"+flag.Name)
		for parent := cmd.Parent(); parent != nil; parent = parent.Parent() {
			if parent.PersistentFlags().Lookup(flag.Name) == nil {
				continue
			}
			if !slices.Contains(owners, parent.CommandPath()) {
				owners = append(owners, parent.CommandPath())
			}
			break
		}
	})
	if len(names) == 0 {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "  %s\n", strings.Join(names, ", "))
	for _, owner := range owners {
		fmt.Fprintf(&out, "  See %s --help for defaults and descriptions.\n", owner)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// Replace only Cobra's inherited-flags section so other help stays upstream-owned.
func compactCLIUsageTemplate() string {
	const inherited = `{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}`
	const compact = `{{if .HasAvailableInheritedFlags}}{{with (index .Annotations "agentsview.common_flags")}}

Common flags:
{{.}}{{end}}{{end}}`
	template := (&cobra.Command{}).UsageTemplate()
	if strings.Count(template, inherited) != 1 {
		panic("Cobra inherited-flags usage template changed")
	}
	return strings.Replace(template, inherited, compact, 1)
}
