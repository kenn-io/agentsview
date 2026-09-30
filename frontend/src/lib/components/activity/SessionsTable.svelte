<script lang="ts">
  import { formatDateTime, m } from "../../i18n/index.js";
  import type { Report } from "../../api/types.js";
  import type { ActivitySessionRow } from "../../api/generated/index";
  import { router } from "../../stores/router.svelte.js";
  import { XIcon } from "../../icons.js";
  import { Button, TableHeaderCell } from "@kenn-io/kit-ui";
  import { formatMoney } from "../../money.js";
  import type { ActivitySessionSort } from "../../api/activity-report.js";

  let {
    report,
    filterActive = false,
    filterLabel = "",
    loading = false,
    error = null,
    sortKey = "agent_minutes",
    sortDir = "desc",
    listVersion = 0,
    onClearFilter,
    onSort,
    onLoadMore,
    onRetry,
  }: {
    report: Report;
    filterActive?: boolean;
    filterLabel?: string;
    loading?: boolean;
    error?: string | null;
    sortKey?: ActivitySessionSort;
    sortDir?: "asc" | "desc";
    listVersion?: number;
    onClearFilter?: () => void;
    onSort?: (sort: ActivitySessionSort, direction: "asc" | "desc") => void;
    onLoadMore?: (cursor: string) => void;
    onRetry?: () => void;
  } = $props();

  // Start fetching the next page while this many rows are still below the
  // visible area, so on a reasonably fast connection it lands before the user
  // reaches the end. Counting rows instead of pixels keeps the same lead at
  // any interface zoom or text size.
  const LOAD_AHEAD_ROWS = 15;

  let scrollEl: HTMLDivElement | undefined = $state();

  // by_session is typed `any[] | null` by the codegen; cast to the
  // generated element model for field-level type safety.
  const rows = $derived(
    report.by_session ?? [],
  );

  // A failed request stops automatic loading so a persistent error cannot
  // turn into a request loop; the retry button resumes it.
  function loadMoreIfNearEnd() {
    const cursor = report.sessions_next_cursor;
    if (!scrollEl || !cursor || loading || error) return;
    const rowHeight =
      scrollEl.querySelector<HTMLElement>(".session-row")?.offsetHeight ?? 0;
    const remaining =
      scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
    if (remaining <= LOAD_AHEAD_ROWS * rowHeight) onLoadMore?.(cursor);
  }

  // Declared before the load check so a replaced list is back at the top
  // before that check measures how close the user is to the end.
  $effect(() => {
    void listVersion;
    if (scrollEl) scrollEl.scrollTop = 0;
  });

  // Re-check after every page lands or a load settles: the user may still be
  // at the bottom, or the rows may not fill the scroll box.
  $effect(() => {
    void rows.length;
    loadMoreIfNearEnd();
  });

  function setSort(key: ActivitySessionSort) {
    const direction = sortKey === key
      ? sortDir === "asc" ? "desc" : "asc"
      : key === "project" || key === "agent" ? "asc" : "desc";
    onSort?.(key, direction);
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
    key: ActivitySessionSort;
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
      {#if filterActive}
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
      {#if (report.sessions_total ?? rows.length) > 0}
        <span class="count">{m.activity_total_count({ count: report.sessions_total ?? rows.length })}</span>
      {/if}
    </div>
  </div>

  <!-- Same thin query bar as the Analytics and Quality pages: it marks a load
       without taking a row or moving the table. It stays hidden for the first
       half second, so a fast page just continues the scroll. -->
  <div class="table-frame" aria-busy={loading}>
    {#if loading}
      <div class="query-progress" aria-hidden="true"></div>
    {/if}
    {#if rows.length > 0}
      <div class="table-scroll" bind:this={scrollEl} onscroll={loadMoreIfNearEnd}>
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
            {#each rows as row (row.session_id)}
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
                    {#if row.is_subagent}
                      <span class="subagent-badge">{m.activity_subagent()}</span>
                    {:else if row.is_automated}
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
                <td class="col-num col-cost">{formatMoney(row.cost)}</td>
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
        {filterActive
          ? m.activity_no_sessions_selected_range()
          : m.shared_no_sessions_in_range()}
      </div>
    {/if}
  </div>
  {#if error}
    <div class="page-error">
      <span>{error}</span>
      <Button size="sm" onclick={() => onRetry?.()}>{m.shared_retry()}</Button>
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
    gap: var(--space-2);
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

  .table-frame {
    position: relative;
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .query-progress {
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    z-index: 2;
    height: 2px;
    overflow: hidden;
    background: color-mix(in srgb, var(--accent-blue) 16%, transparent);
    opacity: 0;
    animation: query-progress-reveal 150ms ease-out 500ms forwards;
  }

  @keyframes query-progress-reveal {
    to {
      opacity: 1;
    }
  }

  .query-progress::before {
    content: "";
    display: block;
    width: 38%;
    height: 100%;
    background: var(--accent-blue);
    border-radius: 999px;
    animation: query-progress 1s ease-in-out infinite;
  }

  @keyframes query-progress {
    0% {
      transform: translateX(-105%);
    }
    100% {
      transform: translateX(265%);
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .query-progress {
      animation-duration: 0s;
    }

    .query-progress::before {
      width: 100%;
      animation: none;
    }
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

  .auto-badge,
  .subagent-badge {
    flex-shrink: 0;
    padding: 1px 5px;
    border-radius: 999px;
    font-size: 9px;
    font-weight: 600;
    --badge-color: var(--accent-orange);
    color: var(--badge-color);
    background: color-mix(in srgb, var(--badge-color) 14%, transparent);
    border: 1px solid color-mix(in srgb, var(--badge-color) 35%, transparent);
  }

  .subagent-badge {
    --badge-color: var(--accent-violet);
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

  .page-error {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 8px;
    padding-top: 8px;
    font-size: 11px;
    color: var(--accent-red);
  }
</style>
