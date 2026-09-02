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

Every session renders as a complete transcript: user prompts, assistant
responses, thinking blocks, and tool calls, with filters by project, agent,
date, and message count. Subagent trees, resume chains, and edited files stay
connected to their parent session. [Usage guide](/docs/usage/).

## 03. Monitor the fleet

The Activity dashboard shows when agents ran, how much work overlapped, and what
it cost: peak concurrency with the exact moment it happened, active versus idle
time, and agent-minutes over any window. Click a timeline bucket to see exactly
which sessions were running in that slot. [Activity reference](/docs/activity/).

## 04. Meter tokens and cost

Usage reports over the whole archive come back in under a second, priced from
LiteLLM and OpenRouter rates with cache-aware accounting. The CLI answers in the
terminal (`agentsview usage daily`), the statusline shows today's spend inside
your editor, and one-shot capture meters a single CI run exactly.
[Token usage and costs](/docs/token-usage/).

## 05. Search by words or by meaning

Full-text search finds the conversation where you discussed a specific function
or error, even months later. Opt-in semantic and hybrid search match by meaning
over conversation units, cite the unit behind every result, and can pull the
surrounding context on demand. [Semantic search](/docs/semantic-search/).

## 06. Assess session health

Session intelligence classifies outcomes and scores health from the transcript
itself: tool failures, context pressure, and loop signals. Deterministic quality
rules turn recurring patterns into recommendations, each backed by the source
sessions that triggered it. [Session intelligence](/docs/session-intelligence/).

## 07. Keep what the sessions learned

Recall (experimental) extracts durable, provenance-linked knowledge from the
archive and keeps it browsable: every entry carries evidence links back to its
source transcripts. Generated Insights add model-written reports over an
explicit session scope. [Recall reference](/docs/recall/).

## 08. Give your agents the archive

The loop closes when agents read their own history. The MCP server exposes
session history as assistant tools, the REST API and CLI serve scripts and
hooks, and SSE streams live messages. An agent can check what a previous run
tried before repeating it. [MCP server](/docs/mcp/) ·
[Session API](/docs/session-api/).

## 09. Extend beyond one machine

Push each machine's archive to PostgreSQL for a merged team view, mirror into
DuckDB for analytical queries, read source files through the filesystem or S3,
or keep original files in hosted raw custody. SQLite on your disk remains the
local archive of record. [PostgreSQL sync](/docs/pg-sync/) ·
[DuckDB mirror](/docs/duckdb/) · [Hosted raw sync](/docs/hosted-raw-sync/).

## Next

Installation takes under a minute and the first sync uses the sessions already
on your machine. [Run the quickstart](/docs/quickstart/) or
[open the docs](/docs/).
