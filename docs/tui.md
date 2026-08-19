---
title: Terminal Interface
description: Browse and manage the complete AgentsView experience from a terminal
---

The `agentsview tui` command opens a full-screen terminal interface over the
same HTTP API and event stream as the web application. It does not open the
SQLite archive directly.

## Start The TUI

For the local archive, run:

```bash
agentsview tui
```

AgentsView resolves the local daemon and starts it when needed. Use an explicit
daemon URL for a forwarded, remote, PostgreSQL-backed, or DuckDB-backed server:

```bash
agentsview tui --server http://127.0.0.1:8085
agentsview tui --server https://agents.example.com \
  --server-token-file /path/to/token
```

The local config token is not sent to an explicit `--server` URL. Supply that
server's token with `--server-token-file`.

## Views

The navigation pane provides these views:

- **Sessions** includes list filters, full-text search, paging, transcript
  paging, in-session search, Markdown rendering, health signals, token and cost
  usage, activity, timing, and session actions.
- **Dashboard** includes all analytics data used by the web dashboard: summary,
  activity, daily heatmap, projects, hour of week, session shape, velocity,
  tools, skills, health signals, and top sessions.
- **Usage** includes daily cost, cache efficiency, prior-period comparison,
  model/project/agent attribution, pairwise comparisons, and top sessions.
- **Activity** includes concurrency, active and agent time, usage, project,
  model, agent, and session breakdowns.
- **Trends**, **Insights**, **Pinned**, **Trash**, and **Recent edits** provide
  the matching web workflows, including search, paging, publishing, restore,
  and deletion actions.
- **Settings** shows daemon and agent-directory state and manages appearance,
  terminal launch mode, authentication, GitHub publishing, worktree mappings,
  embedding generations, sync, remote sync, and archive imports.

The layout adapts to the terminal width. Wide terminals show navigation, the
session list, and the transcript together. Medium terminals show two panes.
Narrow terminals show the focused pane.

## Keys

| Key                 | Action                                      |
| ------------------- | ------------------------------------------- |
| `Tab` / `Shift+Tab` | Change the focused pane                     |
| `j`, `k`, arrows    | Move the current selection                  |
| `Enter`             | Open the selected view or session           |
| `/`                 | Search the active list or enter trend terms |
| `:`                 | Enter a command                             |
| `n`, `p`            | Page forward or back                        |
| `[` / `]`           | Move between transcript search matches      |
| `r`                 | Reload the active view                      |
| `s`                 | Star the selected session                   |
| `d`                 | Move the selected session to trash          |
| `?`                 | Show the complete in-app reference          |
| `q` / `Ctrl+C`      | Quit                                        |

When the transcript pane is focused, `n` loads the next message page before it
pages the session list.

## Commands

Commands start with `:`. Empty values clear string filters.

```text
:project NAME                 :exclude-project NAME
:agent NAME                   :exclude-agent NAME
:machine NAME                 :model NAME
:exclude-model NAME           :branch TOKEN
:date YYYY-MM-DD              :from YYYY-MM-DD
:to YYYY-MM-DD                :active-since RFC3339
:min-messages N               :max-messages N
:min-user-messages N          :min-failures N
:outcome VALUE                :health GRADES
:termination VALUE            :sort SPEC
:has-secret on|off            :starred on|off
:one-shot on|off              :automated on|off
:children on|off
:activity-preset day|week|month|custom
:activity-date YYYY-MM-DD    :activity-bucket 5m|15m|1h|1d|1w
:activity-automation all|interactive|automated
:granularity day|week|month  :insight-type TYPE
```

Session and transcript actions:

```text
:find QUERY                   :rename NAME
:star                         :unstar
:pin NOTE                     :unpin
:open-session                 :resume-session
:publish-session              :publish-secret
:export-html [PATH]           :export-markdown [PATH]
:delete                       :restore
:delete-permanent
```

Report and global actions:

```text
:terms TERM1,TERM2
:compare model|LEFT|RIGHT
:compare project|LEFT|RIGHT
:generate-insight type|from|to|project
:publish-insight ID           :publish-insight-secret ID
:delete-insight ID
:export-insight-html ID[|PATH]
:export-insight-markdown ID[|PATH]
:sync                         :resync
:sync-remote HOST [force]
:import-claude PATH           :import-chatgpt PATH
:github-token                 :require-auth on|off
```

Settings actions:

```text
:theme auto|dark|light        :contrast on|off
:layout default|compact|stream|skim
:thinking on|off              :tools on|off
:terminal auto|clipboard
:terminal custom|BINARY|ARGUMENTS
:worktree-add layout|path|project[|enabled]
:worktree-update id|layout|path|project|enabled
:worktree-delete ID           :worktrees-apply
:embeddings-build
:embeddings-activate ID
:embeddings-retire ID [force]
```

Destructive commands require confirmation. The TUI stores non-secret display
and filter state in `tui-state.json` under the AgentsView data directory. It
does not store command values such as the GitHub token in that file. The
GitHub token command opens a masked input.

## Terminal Rendering

The TUI sanitizes daemon text before terminal rendering and renders Markdown
with terminal-safe styles. The terminal controls the actual glyph size.

Images and Mermaid diagrams have no portable inline terminal representation.
Use `:open-link URL` to hand an HTTP URL or absolute local path to the operating
system. Omitting the path from `:export-html` or `:export-markdown` writes a
temporary file and opens it with the same handoff.

## Language

The TUI selects English, Simplified Chinese, Traditional Chinese, or Korean
common navigation labels from `LC_ALL`, `LC_MESSAGES`, or `LANG`. It has no
separate language selector. The web application's language setting remains
independent.

## Read-Only Daemons

PostgreSQL- and DuckDB-backed daemons can be read-only. All report, search, and
transcript views remain available. The TUI marks the connection as read-only
and rejects archive mutations.
