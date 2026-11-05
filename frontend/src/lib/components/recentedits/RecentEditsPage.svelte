<script lang="ts">
  import { SearchInput, TableHeaderCell, VirtualTable } from "@kenn-io/kit-ui";
  import { MediaQuery } from "svelte/reactivity";
  import { PencilIcon, RefreshCwIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import { RecentEditsService } from "../../api/generated/index";
  import { isAbortError } from "../../api/runtime.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { formatCount, formatRelativeTime } from "../../utils/format.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import LibraryPageHeader from "../shared/LibraryPageHeader.svelte";
  import StateNotice from "../shared/StateNotice.svelte";
  import { LatestRead } from "../../utils/latest-read.js";
  import {
    loadColumnWidths,
    mergeColumnWidths,
    saveColumnWidths,
  } from "./columnWidths.js";

  interface Edit {
    session_id: string;
    ordinal: number;
    call_index: number;
    tool_use_id?: string;
    tool_name: string;
    category: string;
    timestamp?: string;
  }

  interface FileRow {
    project: string;
    file_path: string;
    edit_count: number;
    last_edited_at?: string;
    last_session_id: string;
    edits: Edit[];
    edits_truncated: boolean;
  }

  const PAGE = 50;
  let files = $state<FileRow[]>([]);
  let hasMore = $state(false);
  let loading = $state(true);
  let expanded = $state<Set<string>>(new Set());
  let lastProject = $state<string | null>(null);
  let search = $state("");
  // Monotonic request id: only the latest in-flight load() applies its
  // response. Plain (non-reactive) — it gates control flow, not rendering.
  let requestSeq = 0;
  // Debounce timer for the file-path search box; plain, gates control flow.
  let searchTimer: ReturnType<typeof setTimeout> | undefined;
  const pageRead = new LatestRead();

  // Phones stack the project above the path in one File column, so the
  // file name keeps the row's width.
  const stacked = new MediaQuery("max-width: 520px");
  const columns = $derived(
    stacked.current
      ? ["file", "edits", "time"]
      : ["project", "file", "edits", "time"],
  );
  // Starting widths for columns nobody resized. The project column takes a
  // quarter of the list up to 220px, and the file column what is left.
  const DEFAULT_WIDTHS: Record<string, number> = { edits: 72, time: 104 };
  const STACKED_WIDTHS: Record<string, number> = { edits: 52, time: 76 };
  // Widths the user set by dragging or fitting a column edge, kept across
  // visits. Plain: the table owns the live widths once it has sized them.
  let keptWidths = loadColumnWidths();
  const resizedColumns = new Set<string>();

  // The toolbar spans the list's width, and it is laid out before the
  // first rows arrive, when the table sizes its columns.
  let listWidth = $state(0);

  function columnWidth(column: string): number | undefined {
    const widths = stacked.current
      ? STACKED_WIDTHS
      : {
          ...DEFAULT_WIDTHS,
          project: Math.round(Math.min(220, Math.max(100, listWidth * 0.25))),
          ...keptWidths,
        };
    if (column !== "file") return widths[column];
    if (widths.file !== undefined) return widths.file;
    // The file column starts with what the other columns leave inside the
    // list's border, so the table opens without sideways scrolling.
    if (listWidth <= 0) return undefined;
    const others = columns
      .filter((c) => c !== "file")
      .reduce((sum, c) => sum + (widths[c] ?? 0), 0);
    return Math.max(160, listWidth - 2 - others);
  }

  // A drag, key press, or double-click on a header's resize handle marks
  // that column as user-sized, so only its width is kept.
  function noteResize(event: Event) {
    const handle = (event.target as Element | null)?.closest?.(".kit-th__resize");
    const cell = handle?.closest("th");
    const column = cell ? columns[cell.cellIndex] : undefined;
    if (column) resizedColumns.add(column);
  }

  function keepWidths(widths: number[]) {
    // Phone widths fit the phone; they are not kept for wider windows.
    if (stacked.current) return;
    keptWidths = mergeColumnWidths(keptWidths, columns, widths, resizedColumns);
    saveColumnWidths(keptWidths);
  }

  type ListRow =
    | { kind: "file"; file: FileRow }
    | { kind: "edits"; file: FileRow };

  // An expanded file's edits render as a full-width row right below it.
  const rows = $derived(
    files.flatMap((file): ListRow[] =>
      expanded.has(key(file))
        ? [{ kind: "file", file }, { kind: "edits", file }]
        : [{ kind: "file", file }],
    ),
  );

  // The last few characters of a long project name, kept visible when the
  // name truncates, so date-prefixed names stay distinguishable.
  const PROJECT_TAIL_CHARS = 8;
  function projectTail(project: string): string {
    const chars = Array.from(project);
    return chars.length > PROJECT_TAIL_CHARS * 2
      ? chars.slice(-PROJECT_TAIL_CHARS).join("")
      : "";
  }

  function key(f: FileRow) {
    return JSON.stringify([f.project, f.file_path]);
  }

  function shortPath(p: string) {
    const parts = p.split("/");
    return parts.slice(-2).join("/");
  }

  async function load(reset = false) {
    const seq = ++requestSeq;
    const signal = pageRead.begin();
    const project = sessions.filters.project || undefined;
    const searchTerm = search.trim() || undefined;
    // Derive the page offset from what is already loaded so a failed page
    // never advances past unfetched groups; each page returns exactly PAGE
    // groups while has_more holds.
    const requestOffset = reset ? 0 : files.length;
    loading = true;
    if (reset) {
      files = [];
    }
    try {
      const res = (await RecentEditsService.getApiV1RecentEdits({
          limit: PAGE,
          offset: requestOffset,
          project,
          search: searchTerm,
        }, { signal }));
      // A newer project change, refresh, or load-more supersedes this one.
      if (seq !== requestSeq || !pageRead.isCurrent(signal)) return;
      files = reset
        ? (res.files ?? [])
        : [...files, ...(res.files ?? [])];
      hasMore = res.has_more ?? false;
    } catch (e) {
      if (isAbortError(e) || !pageRead.isCurrent(signal)) return;
      // leave current list; empty state covers first load
    } finally {
      if (pageRead.finish(signal)) loading = false;
    }
  }

  function toggle(f: FileRow) {
    const k = key(f);
    const next = new Set(expanded);
    if (next.has(k)) {
      next.delete(k);
    } else {
      next.add(k);
    }
    expanded = next;
  }

  function jump(e: Edit) {
    ui.scrollToOrdinal(e.ordinal, e.session_id);
    router.navigateToSession(e.session_id);
  }

  function loadMore() {
    load(false);
  }

  function refresh() {
    load(true);
  }

  // Debounce the file-path search box: collapse rapid keystrokes into one
  // reset load so paging restarts from the top of the filtered results.
  function scheduleSearch() {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => load(true), 250);
  }

  // Drop a pending debounced search if the page unmounts mid-typing.
  $effect(() => () => {
    clearTimeout(searchTimer);
    pageRead.cancel();
  });

  // Initial load, and reload when the header's project filter changes.
  // No SSE subscription: feed only reloads on open, explicit refresh,
  // project change, or load-more.
  $effect(() => {
    const proj = sessions.filters.project;
    if (proj !== lastProject) {
      lastProject = proj;
      load(true);
    }
  });
</script>

<!-- Long names truncate in the middle: the head shrinks with an ellipsis
     and the distinguishing tail stays visible. -->
{#snippet projectName(project: string, tail: string)}
  <span class="re-project" title={project}
    ><span class="re-project-head">{project.slice(0, project.length - tail.length)}</span
    ><span class="re-project-tail">{tail}</span></span
  >
{/snippet}

<div class="recent-edits-page">
  <LibraryPageHeader title={m.recent_edits_title()}>
    {#snippet actions()}
      <button
        class="re-refresh"
        onclick={refresh}
        disabled={loading}
        title={m.recent_edits_refresh()}
      >
        <RefreshCwIcon size="12" aria-hidden="true" />
        {m.recent_edits_refresh()}
      </button>
    {/snippet}
  </LibraryPageHeader>

  <div class="re-toolbar" bind:clientWidth={listWidth}>
    <ProjectTypeahead
      projects={sessions.projects}
      value={sessions.filters.project}
      onselect={(v) => sessions.setProjectFilter(v)}
    />
    <SearchInput
      class="re-search"
      bind:value={search}
      oninput={scheduleSearch}
      placeholder={m.recent_edits_search_placeholder()}
      ariaLabel={m.recent_edits_search_placeholder()}
      clearLabel={m.recent_edits_clear_search()}
      block
    />
  </div>

  {#if loading && files.length === 0}
    <div class="re-loading">{m.recent_edits_loading()}</div>
  {:else if files.length === 0}
    <StateNotice class="re-empty" description={m.recent_edits_empty()}>
      {#snippet icon()}
        <PencilIcon size="16" strokeWidth="2" />
      {/snippet}
    </StateNotice>
  {:else}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div
      class="re-list"
      onpointerdowncapture={noteResize}
      onkeydowncapture={noteResize}
      ondblclickcapture={noteResize}
    >
      <VirtualTable
        items={rows}
        rowKey={(row) => `${row.kind}:${key(row.file)}`}
        scroll="page"
        zebra={false}
        unsorted
        estimateRowHeight={33}
        ariaLabel={m.recent_edits_title()}
        oncolumnwidthschange={keepWidths}
      >
        {#snippet header()}
          {#if !stacked.current}
            <TableHeaderCell
              class="re-project-col"
              label={m.recent_edits_col_project()}
              width={columnWidth("project")}
              minWidth={80}
              maxWidth={640}
            />
          {/if}
          <TableHeaderCell
            class="re-file-col"
            label={m.recent_edits_col_file()}
            width={columnWidth("file")}
            minWidth={120}
          />
          <TableHeaderCell
            class="re-count-col"
            numeric
            label={m.recent_edits_col_edits()}
            width={columnWidth("edits")}
            minWidth={48}
            maxWidth={200}
          />
          <TableHeaderCell
            class="re-time-col"
            numeric
            label={m.recent_edits_col_last_edit()}
            width={columnWidth("time")}
            minWidth={64}
            maxWidth={240}
          />
        {/snippet}
        {#snippet row(item: ListRow)}
          {@const f = item.file}
          {#if item.kind === "file"}
            {@const tail = projectTail(f.project)}
            {@const open = expanded.has(key(f))}
            <tr class="re-file-row" class:open onclick={() => toggle(f)}>
              {#if !stacked.current}
                <td>{@render projectName(f.project, tail)}</td>
              {/if}
              <td>
                {#if stacked.current}{@render projectName(f.project, tail)}{/if}
                <!-- The row toggles on click; the button gives keyboard
                     and screen reader users the same toggle. -->
                <button
                  type="button"
                  class="re-path"
                  title={f.file_path}
                  aria-expanded={open}
                >{shortPath(f.file_path)}</button>
              </td>
              <td class="re-num"><span class="re-count">{formatCount(f.edit_count)}</span></td>
              <td class="re-num">
                {#if f.last_edited_at}
                  <span class="re-time">{formatRelativeTime(f.last_edited_at)}</span>
                {/if}
              </td>
            </tr>
          {:else}
            <tr class="re-edits-row">
              <td colspan={columns.length}>
                <div class="re-edits">
                  {#each f.edits as e}
                    <button class="re-edit" onclick={() => jump(e)}>
                      <span class="re-edit-tool">{e.tool_name}</span>
                      <span class="re-edit-session">{e.session_id}</span>
                      {#if e.timestamp}
                        <span class="re-edit-time"
                          >{formatRelativeTime(e.timestamp)}</span
                        >
                      {/if}
                    </button>
                  {/each}
                  {#if f.edits_truncated}
                    <div class="re-truncated">
                      {m.recent_edits_truncated({
                        shown: f.edits.length,
                        total: f.edit_count,
                      })}
                    </div>
                  {/if}
                </div>
              </td>
            </tr>
          {/if}
        {/snippet}
      </VirtualTable>
    </div>
    {#if hasMore}
      <button class="re-load-more" onclick={loadMore} disabled={loading}>
        {m.recent_edits_load_more()}
      </button>
    {/if}
  {/if}
</div>

<style>
  /* Shared library page frame: a 1040px content column. */
  .recent-edits-page {
    max-width: 1088px;
    margin: 0 auto;
    padding: 28px 24px;
  }

  /* Same refresh glyph as the top bar and page refresh controls. */
  .re-refresh {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    background: none;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    padding: 4px 12px;
    cursor: pointer;
    transition: background 0.12s, color 0.12s;
  }

  .re-refresh:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .re-refresh:disabled {
    opacity: var(--opacity-disabled);
    cursor: default;
  }

  .re-toolbar {
    display: flex;
    align-items: center;
    gap: var(--space-5);
    margin-bottom: 12px;
  }

  .re-toolbar :global(.re-search) {
    flex: 1;
    min-width: 0;
  }

  .re-loading {
    text-align: center;
    color: var(--text-muted);
    padding: 40px 0;
    font-size: 13px;
  }

  /* One table with hairline rows. kit-ui's VirtualTable owns the column
     widths: drag a header edge to resize a column (double-click it to fit
     the content); wider columns than the page scroll sideways. Clip, not
     hidden, so the sticky header still follows the page scroll; the table's
     last row border tucks under the frame's own edge. */
  .re-list {
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    overflow: clip;
    background: var(--bg-surface);
  }

  .re-list :global(.kit-table) {
    margin-bottom: -1px;
  }

  .re-list :global(tbody td) {
    padding: 7px 14px;
    vertical-align: middle;
    white-space: nowrap;
    overflow: hidden;
  }

  .re-list :global(.kit-th) {
    padding-inline: 14px;
  }

  .re-file-row {
    cursor: pointer;
  }

  /* Project names sit in a muted column and truncate before the path
     does. */
  .re-project {
    display: flex;
    min-width: 0;
    font-size: 12px;
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
  }

  .re-project-head {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .re-project-tail {
    flex-shrink: 0;
  }

  .re-path {
    display: block;
    width: 100%;
    min-width: 0;
    padding: 0;
    border: 0;
    background: none;
    text-align: left;
    cursor: pointer;
    font-size: 12.5px;
    font-weight: 400;
    color: var(--text-primary);
    font-family: var(--font-mono);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .re-num {
    text-align: right;
  }

  .re-count,
  .re-time {
    font-size: 12px;
    color: var(--text-muted);
    white-space: nowrap;
    font-variant-numeric: tabular-nums;
  }

  /* The edits of an expanded file span the whole row. */
  .re-list :global(tbody tr.re-edits-row td) {
    padding: 0;
    white-space: normal;
  }

  .re-list :global(tbody tr.re-edits-row:hover) {
    background: none;
  }

  /* Phones: the project moves above the path inside the File column. */
  @media (max-width: 520px) {
    .recent-edits-page {
      padding: 20px 12px;
    }

    .re-list :global(tbody td),
    .re-list :global(.kit-th) {
      padding-inline: 10px;
    }

    .re-project {
      margin-bottom: 2px;
    }
  }

  .re-edits {
    background: var(--bg-inset);
    display: flex;
    flex-direction: column;
  }

  .re-edit {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    width: 100%;
    padding: 8px 14px 8px 24px;
    text-align: left;
    background: none;
    border: none;
    border-top: 1px solid var(--border-muted);
    cursor: pointer;
    transition: background 0.12s;
  }

  .re-edit:first-child {
    border-top: none;
  }

  .re-edit:hover {
    background: var(--bg-surface-hover);
  }

  .re-edit-tool {
    font-size: 10px;
    font-weight: 600;
    color: var(--accent-amber);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    flex-shrink: 0;
  }

  .re-edit-session {
    flex: 1;
    font-size: 11px;
    color: var(--text-secondary);
    font-family: var(--font-mono);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .re-edit-time {
    font-size: 11px;
    color: var(--text-muted);
    flex-shrink: 0;
    white-space: nowrap;
  }

  .re-truncated {
    padding: 6px 14px 6px 24px;
    font-size: 10px;
    font-style: italic;
    color: var(--text-muted);
    border-top: 1px solid var(--border-muted);
  }

  .re-load-more {
    display: block;
    margin: 20px auto 0;
    padding: 8px 24px;
    font-size: 12px;
    font-weight: 500;
    color: var(--accent-blue);
    background: none;
    border: 1px solid var(--accent-blue);
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background 0.12s;
  }

  .re-load-more:hover:not(:disabled) {
    background: color-mix(in srgb, var(--accent-blue) 8%, transparent);
  }

  .re-load-more:disabled {
    opacity: var(--opacity-disabled);
    cursor: default;
  }
</style>
