---
title: AgentsView
description: Local-first desktop and web app for AI agent sessions
---

# AgentsView

A local-first desktop and web app for browsing, searching, and analyzing your past AI coding sessions. See where your agents' time and money actually go — across every project, model, and tool.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="/quickstart/">Get Started</a>
  <a class="md-button" href="https://github.com/kenn-io/agentsview">View on GitHub</a>
</p>

<figure class="hero-shot" data-lightbox>
  <img src="/assets/generated/screenshots/dashboard.png" alt="AgentsView analytics dashboard" loading="eager" />
</figure>

<div class="agent-matrix">
  <img src="/assets/static/agents/claude-code.svg" alt="Claude Code" data-agent="claude-code" width="696" height="95" />
  <img src="/assets/static/agents/codex.svg" alt="Codex" data-agent="codex" width="1146" height="320" />
  <img src="/assets/static/agents/gemini.svg" alt="Gemini" data-agent="gemini" width="344" height="127" />
  <img src="/assets/static/agents/copilot.svg" alt="Copilot" data-agent="copilot" width="419" height="95" />
  <img src="/assets/static/agents/cursor.svg" alt="Cursor" data-agent="cursor" width="800" height="190" />
  <img src="/assets/static/agents/opencode.svg" alt="OpenCode" data-agent="opencode" width="641" height="115" />
  <img src="/assets/static/agents/openhands.svg" alt="OpenHands" data-agent="openhands" width="550" height="65" />
  <img src="/assets/static/agents/amp.svg" alt="Amp" data-agent="amp" width="400" height="75" />
  <img src="/assets/static/agents/vscode-copilot.svg" alt="VS Code Copilot" data-agent="vscode-copilot" width="600" height="60" />
  <img src="/assets/static/agents/positron.svg" alt="Positron Assistant" data-agent="positron" width="500" height="65" />
  <img src="/assets/static/agents/openclaw.svg" alt="OpenClaw" data-agent="openclaw" width="500" height="65" />
  <img src="/assets/static/agents/pi.svg" alt="Pi" data-agent="pi" width="400" height="65" />
  <img src="/assets/static/agents/iflow.svg" alt="iFlow" data-agent="iflow" width="400" height="65" />
  <img src="/assets/static/agents/zencoder.svg" alt="Zencoder" data-agent="zencoder" width="500" height="65" />
  <img src="/assets/static/agents/kimi.svg" alt="Kimi" data-agent="kimi" width="400" height="65" />
  <img src="/assets/static/agents/warp.svg" alt="Warp" data-agent="warp" width="400" height="65" />
  <img src="/assets/static/agents/hermes.svg" alt="Hermes" data-agent="hermes" width="500" height="65" />
  <img src="/assets/static/agents/cortex-code.svg" alt="Cortex Code" data-agent="cortex-code" width="600" height="65" />
  <img src="/assets/static/agents/kiro.svg" alt="Kiro" data-agent="kiro" width="400" height="65" />
  <img src="/assets/static/agents/forge.png" alt="Forge" data-agent="forge" width="614" height="153" />
  <span class="agent-name" data-agent="aider">Aider</span>
  <span class="agent-name" data-agent="antigravity">Antigravity</span>
  <span class="agent-name" data-agent="cowork">Claude Cowork</span>
  <span class="agent-name" data-agent="commandcode">Command Code</span>
  <span class="agent-name" data-agent="deepseek-tui">DeepSeek TUI</span>
  <span class="agent-name" data-agent="gptme">gptme</span>
  <span class="agent-name" data-agent="kilo">Kilo</span>
  <span class="agent-name" data-agent="mimocode">MiMoCode</span>
  <span class="agent-name" data-agent="vibe">Mistral Vibe</span>
  <span class="agent-name" data-agent="omp">OhMyPi</span>
  <span class="agent-name" data-agent="piebald">Piebald</span>
  <span class="agent-name" data-agent="qclaw">QClaw</span>
  <span class="agent-name" data-agent="qwen">Qwen Code</span>
  <span class="agent-name" data-agent="qwenpaw">QwenPaw</span>
  <span class="agent-name" data-agent="reasonix">Reasonix</span>
  <span class="agent-name" data-agent="shelley">Shelley</span>
  <span class="agent-name" data-agent="visualstudio-copilot">Visual Studio Copilot</span>
  <span class="agent-name" data-agent="workbuddy">WorkBuddy</span>
  <span class="agent-name" data-agent="zed">Zed</span>
</div>

## Quick Start

**Download the desktop app (recommended):**

Download the latest `.dmg` (macOS), `.exe` (Windows), or
`.AppImage` (Linux) from
[GitHub Releases](https://github.com/kenn-io/agentsview/releases) or via homebrew: `brew install --cask agentsview`.
The desktop app is fully bundled and includes built-in
auto-update.

**Install via pip** — or run instantly with `uvx`:

```bash
pip install agentsview    # install permanently
uvx agentsview            # or run without installing
```

**Install via shell script:**

```bash
curl -fsSL https://agentsview.io/install.sh | bash
```

**Windows (PowerShell):**

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://agentsview.io/install.ps1 | iex"
```

```bash
agentsview serve              # Start server
agentsview serve --port 9090  # Custom port
agentsview serve --no-browser # Disable browser auto-open
agentsview serve --background # Run in the background
```

!!! note
    The desktop app and CLI share the same data directory
    (`~/.agentsview/`), so you can use one or both — they are
    fully complementary.

## Fast Token Usage & Cost Reports

If you've been reaching for
[`ccusage`](https://github.com/ryoppippi/ccusage) to see how
much you spent on Claude Code yesterday, try
[`agentsview usage`](/token-usage/) instead. It reads from the
same pre-indexed SQLite database that powers the rest of
AgentsView, so reports come back in well under a second even on
large histories. It reports on token-bearing sessions from Claude
Code, Codex, Copilot CLI, OpenCode-format tools, Pi, Gemini,
Qwen Code, OpenClaw/QClaw, Hermes, WorkBuddy, Forge, Piebald,
Antigravity, Zed, VS Code Copilot, Visual Studio Copilot,
gptme, Mistral Vibe, and more as parser coverage expands.

```bash
agentsview usage daily          # last 30 days, terminal table
agentsview usage daily --all    # full history, JSON-friendly
agentsview usage statusline     # $9.61 today
```

On a 22,000-session local database, `agentsview usage daily`
runs **80–220× faster** than `npx ccusage@latest daily` (see
[benchmarks](/token-usage/#how-it-compares-to-ccusage)). On
smaller databases the absolute gap is smaller, but reports
still come back sub-second. See
[Token Usage & Costs](/token-usage/) for the full write-up.

## See When Your Agents Are Working

The [**Activity**](/activity/) dashboard turns timestamped session
data into a clear picture of *when* your agents ran, how much work
overlapped, and what it cost. See peak concurrency and the exact
moment it happened, active versus idle time, agent-minutes across
concurrent sessions, and total cost — scoped to any day, week,
month, or custom range and filterable by project, agent, and
machine.

![AgentsView Activity dashboard](/assets/generated/screenshots/activity-page.png)

Click any bucket in the concurrency timeline to see exactly which
sessions were running in that slot, overlay token or cost trends
over the bars, and break activity down by project, model, or agent.
The same report is available from the CLI, with `--json` for
scripting:

```bash
agentsview activity report --preset day
agentsview activity report --preset week --json
```

See [Activity](/activity/) for the full reference.

## What It Does

AgentsView reads the session files that your
[AI coding agents](/configuration/#session-discovery) leave on
your machine and gives you a local-first desktop and web app to
work with them. By default everything stays on your machine.
Optionally, [PostgreSQL sync](/pg-sync/) can push session data
to a shared database for team or multi-machine setups.

<div class="grid cards" markdown>

-   **AI-Powered Insights**

    Generate summaries and analysis of your coding sessions
    using Claude, Codex, Copilot, or Gemini. Get daily
    activity digests, multi-day analyses, and
    recommendations — scoped by project or across everything.

-   **Browse Sessions**

    Scroll through every session across all your projects.
    See the full conversation: user prompts, assistant
    responses, thinking blocks, and tool calls. Filter by
    project, agent, date, or message count.

-   **Search Everything**

    Full-text search across all message content. Find that
    one conversation where you discussed a specific function,
    error message, or design decision — even months later.

-   **Analyze Your Usage**

    Activity heatmaps, tool usage breakdowns, velocity
    metrics, session-health analytics, per-project stats, and
    session distribution charts. Understand how you use agents
    over time.

-   **Activity & Concurrency**

    See when your agents were actually working, how much ran
    in parallel, and what it cost — peak concurrency, active
    versus idle time, agent-minutes, and cost over any time
    window, on the [Activity](/activity/) page.

-   **Token Usage & Costs**

    A sub-second [`agentsview usage`](/token-usage/) CLI for
    daily spend reports and a today's-cost status line. A
    `ccusage` alternative for token-bearing sessions across
    multiple agents — including Claude Code, Codex, Copilot CLI,
    VS Code Copilot, and Zed — that runs 80–220× faster on large
    session histories.

-   **Live Sync**

    Watches your session directories for changes and
    streams new messages in real time. Start a coding
    session in one window, watch it appear in AgentsView
    in another.

-   **Multi-Agent Support**

    Works with [dozens of AI coding session sources](/configuration/#session-discovery)
    including Claude Code, Codex, Copilot, Cursor, Gemini,
    OpenHands, Aider, Claude Cowork, DeepSeek TUI, gptme,
    Kilo, MiMoCode, Mistral Vibe, OhMyPi, QwenPaw, Reasonix,
    Shelley, and Visual Studio Copilot. Auto-discovers session
    directories so there's nothing to configure.

-   **Import Chat History**

    Import your [Claude.ai and ChatGPT](/chat-import/)
    conversations — including images. Upload a zip export
    and browse everything in one place alongside your
    agent coding sessions.

-   **Runs Locally**

    SQLite database, embedded web frontend, no cloud
    services, no accounts. Install the desktop app or
    a single binary and run it.

</div>

## How It Works

<img src="/assets/static/architecture.svg" alt="AgentsView architecture: agent sessions sync into SQLite with FTS5 search, served via REST API, SSE events, and embedded Svelte SPA" style="width: 100%; max-width: 960px; margin: 1.5rem auto; display: block;" />

AgentsView watches your agent session directories for changes,
parses JSONL files from each agent format, and stores structured
data in SQLite with full-text search indexes. The embedded web
frontend provides browsing, search, and analytics over the
REST API.
