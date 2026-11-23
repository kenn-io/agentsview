# The session intelligence loop

Your agents already write the raw session data. AgentsView turns it into a
searchable archive, and turns that archive into answers your agents can use on
their next run. Nine stops, five minutes.

Updated for [v0.48.0](/docs/changelog/#0480) with Chrome Sync for ChatGPT,
Gemini Apps, and Grok web, word-order and image-text search, a prompt jump
picker, and one-command agent setup.

## 01. Capture every session

The daemon finds session directories for more than 60 agent formats, including
Claude Code, Codex, Cursor, Copilot, Gemini, and Factory Droid, and syncs them
into a single local SQLite archive with full-text indexes. It keeps watching
those directories, so the archive stays up to date while you work.
[Quick start](/docs/quickstart/).

You can also sync Claude.ai, ChatGPT, Gemini Apps, and Grok web through Chrome,
or turn on Codex Cloud on one machine to bring in chats stored on OpenAI's
servers. Muse Code, Muse Personal, Direct API, and fx sessions sync as well.

## 02. Browse the full conversation

Read the prompts, responses, reasoning, and tool calls each agent recorded.
Filter sessions by project, agent, date, and message count, press `Ctrl/Cmd+G`
to open a session by ID, or press `t` inside a session to search typed prompts
and jump straight to one. From the Resume menu, supported agents, including Pi,
can pick up where they left off. [Usage guide](/docs/usage/).

To fix project names, turn on the opt-in
[project workspace](/docs/data/#enable-the-project-workspace) for folder
suggestions, transcript previews, and bulk corrections.

Find related work using exact labels and pull-request links, including pull
requests named in prompts. Launcher links group independently started workers
under their manager and fold their costs into its total.

## 03. Monitor the fleet

The Activity dashboard shows when your agents ran, how much work overlapped, and
what it cost: peak concurrency with the exact moment it happened, active versus
idle time, and agent-minutes over any window. Click a timeline bucket to see
which sessions ran in that slot. [Activity reference](/docs/activity/).

Switch to user-message or assistant-message frequency to compare prompts against
output. In Analytics, compare message roles in Absolute or Percent views and
check Git totals for the selected dates.

## 04. Meter tokens and cost

Usage reports track recorded tokens and estimated costs, including prompt-cache
writes and reads. They reuse saved archive data, though the first request after
an upgrade or sync may take a moment to prepare its cache. You can check usage
from the terminal (`agentsview report usage daily`), see today's spend in your
editor through the statusline, and use one-shot capture to report recorded usage
for a single Claude or Codex CI run. Total Input includes prompt-cache reads and
writes, while Uncached Input keeps those tokens separate. For the cost chart,
choose Smooth, Lines, or Bars. [Token usage and costs](/docs/token-usage/).

Select a project, model, or agent in the cost breakdown to narrow the page. Open
hermes-cron to compare individual jobs. Model aliases combine variants after
each variant has been priced.

## 05. Search by words or by meaning

Press `Ctrl/Cmd+K` to search, then pick a project and date range in the palette.
To widen the search without changing the sidebar, select **All Projects**.
Full-text search matches words in any order, ranks exact matches first, and can
index text inside tool-result images; you can also turn on search by meaning
from the palette or Settings to find related ideas and cite the matching
conversation. [Search controls](/docs/usage/#command-palette) ·
[Semantic search](/docs/semantic-search/).

## 06. Assess session health

Health scores flag tool failures, context pressure, and repeated loops in your
transcripts. Open **Analysis** to inspect Session Vitals, which show measured
tool execution alongside time that cannot be assigned to a phase, and click any
activity row to jump to its transcript entry. Open the health badge to view tool
sequences that trace errors, retries, switches, and recovery back to recorded
calls with no model charges.
[Session intelligence](/docs/session-intelligence/).

Tool Usage tracks empty-result, repeat-call, and recovery rates and links
directly to the contributing sessions. For a model assessment that cites the
calls it judged, choose Analyze tool effectiveness from a session's actions
menu.

Turn on the [Friction Log](/docs/quality/#friction-log) for a daily review of
corrections, tool errors, and recurring problems, each linked to its message,
and file recurring problems as Kata issues.

## 07. Keep what the sessions learned

Recall (experimental) pulls reusable lessons from your archive. Browse each
entry and follow its evidence links back to the source messages. You can approve
useful entries or archive rejected ones, and those decisions carry across later
extraction changes. Generated Insights add model-written reports over an
explicit session scope. [Recall reference](/docs/recall/).

## 08. Give your agents the archive

Your agents can also read their own history. The MCP server exposes session
history as assistant tools, the REST API and CLI serve scripts and hooks, and
SSE streams live messages. An agent can check what a previous run tried before
repeating it. Run
[`agentsview agent setup`](/docs/commands/#agentsview-agent-setup) to install
conversation search skills and register the MCP server for Claude Code and Codex
in one step, or install AgentsView Memory from the Claude Code plugin
marketplace. [MCP server](/docs/mcp/) · [Session API](/docs/session-api/).

Read-only MCP tools also expose file edits, tool calls, child sessions, Activity
reports, tool usage, cost comparisons, and offline guide search. To find the
session and prompt behind a line of code, run
[`agentsview why file.go:120-140`](/docs/commands/#agentsview-why).

## 09. Extend beyond one machine

Push each machine's archive to PostgreSQL for a merged team view, use ClickHouse
for a remote dashboard, read source files from the filesystem or S3, or keep
original files in hosted storage. Hosted raw sync can also parse uploaded
sources directly into PostgreSQL without a local SQLite archive or a separate
push. [PostgreSQL sync](/docs/pg-sync/) ·
[ClickHouse sync](/docs/clickhouse-sync/) ·
[Hosted raw sync](/docs/hosted-raw-sync/).

SSH sync pulls sessions from remote Linux and macOS hosts without needing a
manually installed daemon. Archive capture and seed import preserve a retired
computer's history and original files, and you should keep new local sessions in
a separate data directory.

## Next

Install AgentsView and import the sessions already on your machine.
[Run the quickstart](/docs/quickstart/) or [open the docs](/docs/).
