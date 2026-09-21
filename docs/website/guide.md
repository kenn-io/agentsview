# The session intelligence loop

Your agents already write the raw data. AgentsView turns it into a searchable
archive, and the archive into answers your agents can use on their next run.
Nine stops, five minutes.

## 01. Capture every session

The daemon discovers the session directories behind more than 60 agent formats,
including Claude Code, Codex, Cursor, Copilot, and Gemini, and syncs them into
one local SQLite archive with full-text indexes. It keeps watching, so the
archive stays current while you work. [Quick start](/docs/quickstart/).

## 02. Browse the full conversation

Read the prompts, responses, reasoning, and tool calls recorded by each agent.
Filter by project, agent, date, and message count, or press `Ctrl/Cmd+G` to open
a session by ID. The Resume menu lets supported agents, including Pi, pick up
where they left off. [Usage guide](/docs/usage/).

Use the opt-in [project workspace](/docs/data/#enable-the-project-workspace) to
correct project names with folder suggestions, transcript previews, and bulk
corrections.

## 03. Monitor the fleet

The Activity dashboard shows when agents ran, how much work overlapped, and what
it cost: peak concurrency with the exact moment it happened, active versus idle
time, and agent-minutes over any window. Click a timeline bucket to see exactly
which sessions were running in that slot. [Activity reference](/docs/activity/).

## 04. Meter tokens and cost

Usage reports show recorded tokens and estimated costs, including prompt-cache
writes and reads. Reports reuse saved archive data; the first request after an
upgrade or sync may need time to prepare its cache. The CLI answers in the
terminal (`agentsview usage daily`), the statusline shows today's spend inside
your editor, and one-shot capture reports recorded usage for a single Claude or
Codex CI run. [Token usage and costs](/docs/token-usage/).

## 05. Search by words or by meaning

Press `Ctrl/Cmd+K` to search, then choose a project and date range in the
palette. Select **All Projects** to widen the search without changing the
sidebar. Full-text search finds exact words; opt-in semantic and hybrid search
find related meaning and cite the matching conversation.
[Search controls](/docs/usage/#command-palette) ·
[Semantic search](/docs/semantic-search/).

## 06. Assess session health

Health scores point to tool failures, context pressure, and repeated loops in
the transcript. Open **Analysis** to see Session Vitals: measured tool execution
and time that cannot be assigned to a phase. Click an activity row to inspect
its transcript entry. [Session intelligence](/docs/session-intelligence/).

## 07. Keep what the sessions learned

Recall (experimental) collects reusable lessons from your archive. Browse each
entry and follow its evidence links to the source messages. Generated Insights
add model-written reports over an explicit session scope.
[Recall reference](/docs/recall/).

## 08. Give your agents the archive

The loop closes when agents read their own history. The MCP server exposes
session history as assistant tools, the REST API and CLI serve scripts and
hooks, and SSE streams live messages. An agent can check what a previous run
tried before repeating it. [MCP server](/docs/mcp/) ·
[Session API](/docs/session-api/).

## 09. Extend beyond one machine

Push each machine's archive to PostgreSQL for a merged team view, use ClickHouse
for a remote dashboard, mirror into DuckDB for analytical queries, read source
files through the filesystem or S3, or keep original files in hosted storage.
SQLite on your disk remains the local archive of record.
[PostgreSQL sync](/docs/pg-sync/) · [ClickHouse sync](/docs/clickhouse-sync/) ·
[DuckDB mirror](/docs/duckdb/) · [Hosted raw sync](/docs/hosted-raw-sync/).

## Next

Install AgentsView and import the sessions already on your machine.
[Run the quickstart](/docs/quickstart/) or [open the docs](/docs/).
