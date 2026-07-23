<script lang="ts">
  import { formatDateTime, m } from "../../i18n/index.js";
  import { formatNumber } from "../../utils/format.js";
  import type { DbProjectInventory, DbProjectInventoryRow } from "../../api/generated/index";
  import { TableHeaderCell, TextInput } from "@kenn-io/kit-ui";

  interface Props {
    inventory: DbProjectInventory;
    selectedKey: string;
    onSelect: (key: string) => void;
  }

  let { inventory, selectedKey, onSelect }: Props = $props();

  // `projects` is typed `any[] | null` by the codegen; cast to the generated
  // element model for field-level type safety.
  const allRows = $derived((inventory.projects ?? []) as DbProjectInventoryRow[]);

  let filterText = $state("");

  // export.SafeProjectDisplayLabel legitimately returns "" for absolute-path
  // and URL-scheme projects; display, filtering, and label sorting fall back
  // to a localized label so the row stays visible and findable.
  function displayLabel(row: DbProjectInventoryRow): string {
    return row.label || m.shared_unknown();
  }

  type SortKey =
    | "label"
    | "sessions"
    | "machines"
    | "agents"
    | "distinct_cwds"
    | "first_activity"
    | "last_activity"
    | "rules_targeting";
  type SortDir = "asc" | "desc";

  let sortKey = $state<SortKey>("sessions");
  let sortDir = $state<SortDir>("desc");

  function setSort(key: SortKey) {
    if (sortKey === key) {
      sortDir = sortDir === "asc" ? "desc" : "asc";
    } else {
      sortKey = key;
      // Numeric columns read best high-to-low first; the project label
      // reads best alphabetically.
      sortDir = key === "label" ? "asc" : "desc";
    }
  }

  function compare(a: DbProjectInventoryRow, b: DbProjectInventoryRow, key: SortKey): number {
    switch (key) {
      case "label":
        return displayLabel(a).localeCompare(displayLabel(b));
      case "sessions":
        return a.sessions - b.sessions;
      case "machines":
        return a.machines - b.machines;
      case "agents":
        return a.agents - b.agents;
      case "distinct_cwds":
        return a.distinct_cwds - b.distinct_cwds;
      case "first_activity":
        return (a.first_activity ?? "").localeCompare(b.first_activity ?? "");
      case "last_activity":
        return (a.last_activity ?? "").localeCompare(b.last_activity ?? "");
      case "rules_targeting":
        return a.enabled_rules_targeting - b.enabled_rules_targeting;
    }
  }

  const filteredRows = $derived.by(() => {
    const needle = filterText.trim().toLowerCase();
    if (!needle) return allRows;
    return allRows.filter((row) => displayLabel(row).toLowerCase().includes(needle));
  });

  const sortedRows = $derived.by(() => {
    const dir = sortDir === "asc" ? 1 : -1;
    return [...filteredRows].sort((a, b) => {
      const primary = compare(a, b, sortKey) * dir;
      if (primary !== 0) return primary;
      // Stable tiebreak: equal primary keys order by label ascending
      // regardless of direction, so toggling sortDir never reorders peers.
      return displayLabel(a).localeCompare(displayLabel(b));
    });
  });

  function fmtDate(ts: string | null | undefined): string {
    if (!ts) return "—";
    const d = new Date(ts);
    if (Number.isNaN(d.getTime())) return "—";
    return formatDateTime(d, { year: "numeric", month: "short", day: "numeric" });
  }

  function selectRow(key: string) {
    onSelect(key);
  }

  function onRowKeydown(event: KeyboardEvent, key: string) {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    selectRow(key);
  }

  interface Column {
    key: SortKey;
    label: string;
    numeric?: boolean;
  }

  const columns: Column[] = $derived([
    { key: "sessions", label: m.data_col_sessions(), numeric: true },
    { key: "machines", label: m.data_col_machines(), numeric: true },
    { key: "agents", label: m.data_col_agents(), numeric: true },
    { key: "distinct_cwds", label: m.data_col_cwds(), numeric: true },
    { key: "first_activity", label: m.data_col_first_activity() },
    { key: "last_activity", label: m.data_col_last_activity() },
    { key: "rules_targeting", label: m.data_col_rules(), numeric: true },
  ]);
</script>

<div class="inventory-table">
  <TextInput
    block
    ariaLabel={m.data_filter_projects()}
    placeholder={m.data_filter_projects()}
    bind:value={filterText}
  />

  {#if sortedRows.length > 0}
    <div class="table-scroll">
      <table class="table">
        <thead>
          <tr>
            <TableHeaderCell
              label={m.data_col_project()}
              sortable
              sortDirection={sortKey === "label" ? sortDir : null}
              onsort={() => setSort("label")}
            />
            {#each columns as col}
              <TableHeaderCell
                class="sort-{col.key}"
                label={col.label}
                sortable
                numeric={col.numeric}
                sortDirection={sortKey === col.key ? sortDir : null}
                onsort={() => setSort(col.key)}
              />
            {/each}
          </tr>
        </thead>
        <tbody>
          {#each sortedRows as row (row.project_key)}
            <tr
              tabindex="0"
              class="project-row"
              class:selected={row.project_key === selectedKey}
              aria-selected={row.project_key === selectedKey}
              data-project-key={row.project_key}
              onclick={() => selectRow(row.project_key)}
              onkeydown={(e) => onRowKeydown(e, row.project_key)}
            >
              <td class="col-project" title={displayLabel(row)}>
                <span class="label-text">{displayLabel(row)}</span>
                {#if row.enabled_rules_targeting > 0}
                  <span
                    class="rule-badge"
                    title={m.data_rules_targeting({ count: row.enabled_rules_targeting })}
                  >{row.enabled_rules_targeting}</span>
                {/if}
                {#if row.recorded_as_original}
                  <span class="original-badge" title={m.data_recorded_original()}></span>
                {/if}
              </td>
              <td class="col-num">{formatNumber(row.sessions)}</td>
              <td class="col-num">{formatNumber(row.machines)}</td>
              <td class="col-num">{formatNumber(row.agents)}</td>
              <td class="col-num">{formatNumber(row.distinct_cwds)}</td>
              <td>{fmtDate(row.first_activity)}</td>
              <td>{fmtDate(row.last_activity)}</td>
              <td class="col-num">
                {row.enabled_rules_targeting > 0 ? formatNumber(row.enabled_rules_targeting) : "—"}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {:else}
    <div class="empty">{m.data_no_matches()}</div>
  {/if}
</div>

<style>
  .inventory-table {
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: 0;
  }

  .table-scroll {
    max-height: 480px;
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

  .project-row:last-child td {
    border-bottom: none;
  }

  .project-row:hover {
    background: var(--bg-surface-hover);
    cursor: pointer;
  }

  .project-row.selected {
    background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
  }

  .project-row:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .col-project {
    max-width: 260px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .label-text {
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .rule-badge {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 14px;
    height: 14px;
    padding: 0 4px;
    margin-left: 6px;
    border-radius: 999px;
    font-size: 9px;
    font-weight: 600;
    color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 14%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent-blue) 35%, transparent);
  }

  .original-badge {
    display: inline-block;
    width: 6px;
    height: 6px;
    margin-left: 6px;
    border-radius: 999px;
    background: var(--accent-orange);
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
