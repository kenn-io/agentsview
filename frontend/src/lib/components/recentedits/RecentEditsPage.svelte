<script lang="ts">
  import { SearchInput } from "@kenn-io/kit-ui";
  import { PencilIcon, RefreshCwIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import { RecentEditsService } from "../../api/generated/index";
  import { isAbortError } from "../../api/runtime.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { formatRelativeTime } from "../../utils/format.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import LibraryPageHeader from "../shared/LibraryPageHeader.svelte";
  import StateNotice from "../shared/StateNotice.svelte";
  import { LatestRead } from "../../utils/latest-read.js";

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

  <div class="re-toolbar">
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
    <div class="re-list">
      <div class="re-head">
        <span class="re-project">{m.recent_edits_col_project()}</span>
        <span class="re-path">{m.recent_edits_col_file()}</span>
        <span class="re-count">{m.recent_edits_col_edits()}</span>
        <span class="re-time">{m.recent_edits_col_last_edit()}</span>
      </div>
      {#each files as f (key(f))}
        {@const tail = projectTail(f.project)}
        <div class="re-file">
          <button class="re-file-row" onclick={() => toggle(f)}>
            <!-- Long names truncate in the middle: the head shrinks with an
                 ellipsis and the distinguishing tail stays visible. -->
            <span class="re-project" title={f.project}
              ><span class="re-project-head">{f.project.slice(0, f.project.length - tail.length)}</span
              ><span class="re-project-tail">{tail}</span></span
            >
            <span class="re-path" title={f.file_path}
              >{shortPath(f.file_path)}</span
            >
            <span class="re-count"
              >{m.recent_edits_count({ count: f.edit_count })}</span
            >
            {#if f.last_edited_at}
              <span class="re-time"
                >{formatRelativeTime(f.last_edited_at)}</span
              >
            {/if}
          </button>
          {#if expanded.has(key(f))}
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
          {/if}
        </div>
      {/each}
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

  /* One list with hairline rows, not a stack of cards. */
  .re-list {
    display: flex;
    flex-direction: column;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    overflow: hidden;
    background: var(--bg-surface);
  }

  /* Column labels over the rows: same flex columns and padding as a row,
     in small uppercase muted type. */
  .re-head {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    padding: 6px 14px;
    border-bottom: 1px solid var(--border-muted);
  }

  .re-head > span {
    font-family: inherit;
    font-size: 10.5px;
    font-weight: 600;
    letter-spacing: 0.04em;
    text-transform: uppercase;
    color: var(--text-muted);
  }

  .re-file + .re-file {
    border-top: 1px solid var(--border-muted);
  }

  .re-file-row {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    width: 100%;
    padding: 7px 14px;
    text-align: left;
    background: transparent;
    border: none;
    cursor: pointer;
    transition: background 0.12s;
  }

  .re-file-row:hover {
    background: var(--bg-surface-hover);
  }

  /* Project names sit in a fixed muted column and truncate before the
     path does. The trailing padding keeps a clear gutter before the mono
     path, so a hash tail never reads as part of the file name. */
  .re-project {
    display: flex;
    width: 240px;
    padding-right: 16px;
    font-size: 12px;
    color: var(--text-muted);
    flex-shrink: 0;
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
    flex: 1;
    font-size: 12.5px;
    font-weight: 400;
    color: var(--text-primary);
    font-family: var(--font-mono);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .re-count,
  .re-time {
    min-width: 64px;
    font-size: 12px;
    color: var(--text-muted);
    flex-shrink: 0;
    white-space: nowrap;
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  /* A wider gap between the two right-aligned columns, so "Edits" and
     "Last edit" read as two headers, not one label. */
  .re-time {
    min-width: 72px;
    margin-left: 20px;
  }

  /* Phones: the project moves above the path so the file name keeps the
     row's width, and count and time take only what they need. */
  @media (max-width: 520px) {
    /* Rows stack project over path here, so the column labels no longer
       line up with anything. */
    .re-head {
      display: none;
    }

    .recent-edits-page {
      padding: 20px 12px;
    }

    .re-file-row {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto auto;
      grid-template-areas:
        "project project project"
        "path count time";
      column-gap: var(--space-3);
      row-gap: 2px;
      padding: 7px 10px;
    }

    .re-project {
      grid-area: project;
      width: auto;
      padding-right: 0;
      min-width: 0;
    }

    .re-path {
      grid-area: path;
      min-width: 0;
    }

    .re-count {
      grid-area: count;
      min-width: 0;
    }

    .re-time {
      grid-area: time;
      min-width: 0;
      margin-left: 0;
    }
  }

  .re-edits {
    border-top: 1px solid var(--border-muted);
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
