<script lang="ts">
  import { formatDateTime, m } from "../../i18n/index.js";
  import type { Report } from "../../api/types.js";
  import type { ActivitySessionRow } from "../../api/generated/index";
  import { router } from "../../stores/router.svelte.js";
  import { XIcon } from "../../icons.js";
  import { TableHeaderCell } from "@kenn-io/kit-ui";

  let {
    report,
    filterIds = null,
    filterLabel = "",
    onClearFilter,
  }: {
    report: Report;
    filterIds?: string[] | null;
    filterLabel?: string;
    onClearFilter?: () => void;
  } = $props();

  // by_session is typed `any[] | null` by the codegen; cast to the
  // generated element model for field-level type safety.
  const allRows = $derived(
    (report.by_session ?? []) as ActivitySessionRow[],
  );

  // Page-local time-slot filter from the Concurrency chart: a non-null id list
  // restricts the table to the sessions active in the clicked slot. An empty
  // set (an idle slot was clicked) correctly yields no rows but still shows the
  // dismissible badge so the selection can be cleared.
  const filterSet = $derived(filterIds ? new Set(filterIds) : null);
  const rows = $derived(
    filterSet
      ? allRows.filter((r) => filterSet.has(r.session_id))
      : allRows,
  );

  type SortKey =
    | "agent_minutes"
    | "cost"
    | "first_active"
    | "project"
    | "agent";
  type SortDir = "asc" | "desc";

  let sortKey = $state<SortKey>("agent_minutes");
  let sortDir = $state<SortDir>("desc");

  function setSort(key: SortKey) {
    if (sortKey === key) {
      sortDir = sortDir === "asc" ? "desc" : "asc";
    } else {
      sortKey = key;
      // Numeric/time columns read best high-to-low first; text
      // columns alphabetically.
      sortDir =
        key === "project" || key === "agent" ? "asc" : "desc";
    }
  }

  function isUntimed(row: ActivitySessionRow): boolean {
    return row.agent_minutes === null;
  }

  function compare(
    a: ActivitySessionRow,
    b: ActivitySessionRow,
    key: SortKey,
  ): number {
    switch (key) {
      case "agent_minutes":
        return (a.agent_minutes ?? 0) - (b.agent_minutes ?? 0);
      case "cost":
        return a.cost - b.cost;
      case "first_active": {
        const av = a.first_active ?? "";
        const bv = b.first_active ?? "";
        return av < bv ? -1 : av > bv ? 1 : 0;
      }
      case "project":
        return a.project.localeCompare(b.project);
      case "agent":
        return a.agent.localeCompare(b.agent);
    }
  }

  function byKeyThenId(
    a: ActivitySessionRow,
    b: ActivitySessionRow,
    dir: number,
  ): number {
    const primary = compare(a, b, sortKey) * dir;
    if (primary !== 0) return primary;
    // Stable tiebreak: equal primary keys order by session_id
    // ascending regardless of direction, so toggling sortDir never
    // reorders peers.
    return a.session_id.localeCompare(b.session_id);
  }

  // Untimed rows only have a null value for the timing keys
  // (agent_minutes, first_active); their cost/project/agent are real.
  // Partition them to the bottom only when sorting by a timing key, so
  // a high-cost untimed session still participates in the cost sort.
  const sortedRows = $derived.by(() => {
    const dir = sortDir === "asc" ? 1 : -1;
    const partitionUntimed =
      sortKey === "agent_minutes" || sortKey === "first_active";
    if (!partitionUntimed) {
      return [...rows].sort((a, b) => byKeyThenId(a, b, dir));
    }
    const timed: ActivitySessionRow[] = [];
    const untimed: ActivitySessionRow[] = [];
    for (const row of rows) {
      if (isUntimed(row)) untimed.push(row);
      else timed.push(row);
    }
    timed.sort((a, b) => byKeyThenId(a, b, dir));
    // Keep appended untimed rows in a stable session_id order so they
    // don't jump around between renders.
    untimed.sort((a, b) => a.session_id.localeCompare(b.session_id));
    return [...timed, ...untimed];
  });

  function fmtCost(v: number): string {
    return `$${v.toFixed(2)}`;
  }

  function fmtMinutes(v: number | null): string {
    if (v === null) return "—";
    return Math.round(v).toLocaleString();
  }

  function rowModel(row: ActivitySessionRow): string {
    const models = (row.models ?? []) as string[];
    if (models.length > 1) return m.activity_mixed();
    return row.primary_model || "—";
  }

  // RFC3339 -> "HH:MM" in the viewer's local zone, matching the
  // server-side local day window.
  function fmtClock(ts: string | null): string {
    if (!ts) return "";
    const d = new Date(ts);
    if (Number.isNaN(d.getTime())) return "";
    return formatDateTime(d, {
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    });
  }

  function fmtWindow(
    first: string | null,
    last: string | null,
  ): string {
    const a = fmtClock(first);
    const b = fmtClock(last);
    if (!a || !b) return "—";
    return `${a}–${b}`;
  }

  interface Column {
    key: SortKey;
    label: string;
  }

  const sortColumns: Column[] = $derived([
    { key: "project", label: m.activity_project() },
    { key: "agent", label: m.activity_agent() },
    { key: "agent_minutes", label: m.activity_agent_min() },
    { key: "cost", label: m.activity_cost() },
    { key: "first_active", label: m.activity_window() },
  ]);

</script>

<div class="sessions-table">
  <div class="sessions-header">
    <h3 class="chart-title">{m.activity_sessions()}</h3>
    <div class="header-meta">
      {#if filterSet}
        <button
          type="button"
          class="filter-badge"
          onclick={() => onClearFilter?.()}
          title={m.activity_clear_time_filter()}
        >
          <span>{m.activity_active_filter({ label: filterLabel })}</span>
          <span class="filter-badge-x">
            <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
          </span>
        </button>
      {/if}
      {#if rows.length > 0}
        <span class="count">{m.activity_total_count({ count: rows.length })}</span>
      {/if}
    </div>
  </div>

  {#if rows.length > 0}
    <div class="table-scroll">
      <table class="table">
        <thead>
          <tr>
            <TableHeaderCell label={m.activity_session()} />
            <TableHeaderCell label={m.activity_model()} />
            {#each sortColumns as col}
              <TableHeaderCell
                class="sort-{col.key}"
                label={col.label}
                sortable
                numeric={col.key === "agent_minutes" || col.key === "cost"}
                sortDirection={sortKey === col.key ? sortDir : null}
                onsort={() => setSort(col.key)}
              />
            {/each}
          </tr>
        </thead>
        <tbody>
          {#each sortedRows as row (row.session_id)}
            <tr class="session-row" data-session-id={row.session_id}>
              <td class="col-session">
                <div class="session-cell">
                  <a
                    class="session-link"
                    href={router.buildSessionHref(row.session_id)}
                    title={row.title || row.session_id}
                    onclick={(e) => {
                      if (
                        e.metaKey ||
                        e.ctrlKey ||
                        e.shiftKey ||
                        e.altKey ||
                        e.button !== 0
                      )
                        return;
                      e.preventDefault();
                      router.navigateToSession(row.session_id);
                    }}
                  >
                    {row.title || row.session_id}
                  </a>
                  {#if row.is_automated}
                    <span class="auto-badge" title={m.activity_automated_session()}>{m.activity_auto()}</span>
                  {/if}
                </div>
              </td>
              <td class="col-model">{rowModel(row)}</td>
              <td class="col-project" title={row.project}>
                {row.project}
              </td>
              <td class="col-agent">{row.agent}</td>
              <td class="col-num col-minutes">
                {fmtMinutes(row.agent_minutes)}
              </td>
              <td class="col-num col-cost">{fmtCost(row.cost)}</td>
              <td class="col-window">
                {fmtWindow(row.first_active, row.last_active)}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {:else}
    <div class="empty">
      {filterSet
        ? m.activity_no_sessions_selected_slot()
        : m.shared_no_sessions_in_range()}
    </div>
  {/if}
</div>

<style>
  .sessions-table {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .sessions-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .count {
    font-size: 10px;
    color: var(--text-muted);
  }

  .header-meta {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .filter-badge {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    height: 20px;
    padding: 0 6px 0 8px;
    border: 1px solid var(--accent-blue);
    border-radius: 999px;
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
    color: var(--accent-blue);
    font-size: 10px;
    font-weight: 600;
    cursor: pointer;
    white-space: nowrap;
  }

  .filter-badge:hover {
    background: color-mix(in srgb, var(--accent-blue) 22%, transparent);
  }

  .filter-badge-x {
    display: inline-flex;
    align-items: center;
    flex-shrink: 0;
  }

  .table-scroll {
    max-height: 360px;
    overflow-y: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
  }

  .table {
    width: 100%;
    border-collapse: collapse;
    font-size: 11px;
  }

  /* The header cells come from kit-ui TableHeaderCell; the local table
     shell keeps them pinned while the body scrolls. */
  .table :global(thead th) {
    position: sticky;
    top: 0;
    z-index: 1;
  }

  .col-num {
    text-align: right;
    font-family: var(--font-mono);
  }

  tbody td {
    padding: 5px 8px;
    border-bottom: 1px solid var(--border-muted);
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .session-row:last-child td {
    border-bottom: none;
  }

  .session-row:hover {
    background: var(--bg-surface-hover);
  }

  .col-session {
    max-width: 240px;
  }

  .session-cell {
    display: flex;
    align-items: center;
    gap: 6px;
    max-width: 240px;
  }

  .session-link {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--accent-blue);
    text-decoration: none;
  }

  .auto-badge {
    flex-shrink: 0;
    padding: 1px 5px;
    border-radius: 999px;
    font-size: 9px;
    font-weight: 600;
    color: var(--accent-orange);
    background: color-mix(in srgb, var(--accent-orange) 14%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent-orange) 35%, transparent);
  }

  .session-link:hover {
    text-decoration: underline;
  }

  .col-project {
    max-width: 140px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
