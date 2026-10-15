<script lang="ts">
  import { tick } from "svelte";
  import { fetchSessionInputTree } from "../../api/inputTree.js";
  import type {
    DbSession,
    ServiceInputTree,
    ServiceInputTreeNode,
  } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { formatNumber } from "../../utils/format.js";
  import { layoutInputTree } from "./input-tree.js";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();

  let tree = $state<ServiceInputTree | null>(null);
  let loading = $state(false);
  let error = $state(false);
  // Folded runs the reader opened, by the index of their first input.
  let expandedRuns = $state<ReadonlySet<number>>(new Set());

  $effect(() => {
    const id = sessionId;
    const controller = new AbortController();
    tree = null;
    loading = true;
    error = false;
    expandedRuns = new Set();

    fetchSessionInputTree(id, { signal: controller.signal })
      .then((next) => {
        tree = next;
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        error = true;
        console.error("failed to load input tree", err);
      })
      .finally(() => {
        if (!controller.signal.aborted) loading = false;
      });

    return () => controller.abort();
  });

  const nodes = $derived(tree?.nodes ?? []);
  const sessionsById = $derived(
    new Map<string, DbSession>((tree?.sessions ?? []).map((s) => [s.id, s])),
  );
  // The input selected in the conversation, when it is on the active path.
  const selectedIndex = $derived.by(() => {
    const ordinal = ui.selectedOrdinal;
    if (ordinal === null) return -1;
    return nodes.findIndex((node) => node.active_ordinal === ordinal);
  });
  const activeEndIndex = $derived(
    nodes.findIndex((node) =>
      node.end_session_ids.includes(tree?.active_session_id ?? ""),
    ),
  );
  // The row the list keeps in view: the selected input, or else the last
  // input of the active session.
  const focusIndex = $derived(selectedIndex >= 0 ? selectedIndex : activeEndIndex);
  const rows = $derived(
    layoutInputTree(nodes, {
      expandedRuns,
      keepVisible: new Set(focusIndex >= 0 ? [focusIndex] : []),
    }),
  );

  let listElement = $state<HTMLElement | null>(null);

  // Bring the focused input into view when the tree loads or the selected
  // input changes; opening a folded run leaves the scroll position alone.
  $effect(() => {
    const list = listElement;
    const loaded = tree;
    const index = focusIndex;
    if (!list || !loaded || index < 0) return;
    void tick().then(() => {
      if (listElement === list && tree === loaded && focusIndex === index) {
        scrollFocusedIntoView(list);
      }
    });
  });

  function scrollFocusedIntoView(list: HTMLElement) {
    const row = list.querySelector<HTMLElement>(".input-row.focused");
    if (!row) return;
    // Scroll the list directly: scrollIntoView would also scroll the
    // vitals column and the page layout around it.
    const top = row.offsetTop;
    const bottom = top + row.offsetHeight;
    const viewTop = list.scrollTop;
    const viewBottom = viewTop + list.clientHeight;
    if (top >= viewTop && bottom <= viewBottom) return;
    list.scrollTop = top - (list.clientHeight - row.offsetHeight) / 2;
  }

  function expandRun(runStart: number) {
    expandedRuns = new Set([...expandedRuns, runStart]);
  }

  function isOnActivePath(node: ServiceInputTreeNode): boolean {
    return node.active_ordinal !== undefined && node.active_ordinal !== null;
  }

  // Inputs on the active path jump within the open session; the others
  // open the session that holds them.
  function nodeHref(node: ServiceInputTreeNode): string {
    if (isOnActivePath(node)) {
      return router.buildSessionHref(sessionId, {
        msg: String(node.active_ordinal),
      });
    }
    return router.buildSessionHref(node.session_id, {
      msg: String(node.ordinal),
    });
  }

  function nodeTitle(node: ServiceInputTreeNode): string {
    return isOnActivePath(node)
      ? m.session_input_tree_jump_to({ preview: node.preview })
      : m.session_input_tree_open({ preview: node.preview });
  }

  function openNode(event: MouseEvent, node: ServiceInputTreeNode) {
    if (
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    ) {
      return;
    }
    event.preventDefault();
    if (isOnActivePath(node)) {
      ui.scrollToOrdinal(node.active_ordinal!);
      return;
    }
    ui.scrollToOrdinal(node.ordinal, node.session_id);
    router.navigateToSession(node.session_id);
  }

  function relationshipLabel(id: string): string {
    if (id === tree?.active_session_id) return m.session_input_tree_current();
    return sessionsById.get(id)?.relationship_type === "fork"
      ? m.session_tree_relationship_fork()
      : m.session_tree_relationship_root();
  }

  function sessionTitle(id: string): string {
    const session = sessionsById.get(id);
    const title = (session?.display_name || session?.first_message || "").trim();
    return title || id;
  }

  function messageCountLabel(id: string): string {
    const count = sessionsById.get(id)?.message_count ?? 0;
    return m.session_tree_message_count({
      count,
      countLabel: formatNumber(count),
    });
  }
</script>

{#if loading}
  <div class="input-state">{m.session_input_tree_loading()}</div>
{:else if error}
  <div class="input-state error">{m.session_input_tree_error()}</div>
{:else if tree && nodes.length === 0}
  <div class="input-state">{m.session_input_tree_empty()}</div>
{:else if tree}
  {#if tree.truncated}
    <div class="input-state truncated">{m.session_tree_truncated()}</div>
  {/if}
  <div class="input-tree" bind:this={listElement}>
    {#each rows as row (row.key)}
      {#if row.kind === "gap"}
        <button
          type="button"
          class="input-row gap"
          class:on-path={row.onActivePath}
          style={`--tree-depth: ${row.depth};`}
          title={m.session_input_tree_show_hidden()}
          onclick={() => expandRun(row.runStart)}
        >
          {#each row.rails as col (col)}
            <span class="rail" style={`--col: ${col};`} aria-hidden="true"></span>
          {/each}
          <span class="line gap-line" aria-hidden="true"></span>
          <span class="input-body">
            <span class="gap-label">
              {m.session_input_tree_hidden({
                count: row.hidden,
                countLabel: formatNumber(row.hidden),
              })}
            </span>
          </span>
        </button>
      {:else}
        {@const node = row.node}
        <a
          class="input-row"
          class:on-path={row.onActivePath}
          class:selected={row.index === selectedIndex}
          class:focused={row.index === focusIndex}
          class:leaf={row.isLeaf}
          class:branch-point={row.isBranchPoint}
          href={nodeHref(node)}
          title={nodeTitle(node)}
          aria-current={row.index === selectedIndex ? "step" : undefined}
          style={`--tree-depth: ${row.depth};`}
          onclick={(event) => openNode(event, node)}
        >
          {#each row.rails as col (col)}
            <span class="rail" style={`--col: ${col};`} aria-hidden="true"></span>
          {/each}
          {#if row.elbow}
            <span
              class="elbow"
              class:from-above={row.elbow.top}
              class:continues={row.elbow.bottom}
              aria-hidden="true"
            ></span>
          {/if}
          {#if row.lineAbove}
            <span class="line line-above" aria-hidden="true"></span>
          {/if}
          {#if row.lineBelow}
            <span
              class="line line-below"
              class:path-line={row.lineBelowOnActivePath}
              aria-hidden="true"
            ></span>
          {/if}
          <span class="marker" aria-hidden="true"></span>
          <span class="input-body">
            <span class="input-preview" class:shell={node.is_shell}>
              {node.preview}
            </span>
            {#each node.end_session_ids as endId (endId)}
              <span class="input-end" title={sessionTitle(endId)}>
                <span
                  class="input-end-rel"
                  class:current={endId === tree.active_session_id}
                >
                  {relationshipLabel(endId)}
                </span>
                <span class="input-end-count">{messageCountLabel(endId)}</span>
              </span>
            {/each}
          </span>
        </a>
      {/if}
    {/each}
  </div>
{/if}

<style>
  .input-state {
    padding: 2px 14px 12px;
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.35;
  }

  .input-state.error {
    color: var(--slow-fg);
  }

  .input-state.truncated {
    flex-shrink: 0;
    padding-bottom: 6px;
    font-family: var(--font-mono);
    font-size: 9px;
  }

  .input-tree {
    /* Offset parent for the rows, so their offsetTop is in scroll space. */
    position: relative;
    min-height: 0;
    overflow-y: auto;
    padding: 0 8px 10px;
    display: flex;
    flex-direction: column;
  }

  .input-row {
    /* Column c is centred at --pad + c * --indent; the marker sits in the
       row's own column and the lines run through column centres. */
    --pad: 10px;
    --indent: 12px;
    --mid: 12px;
    --x: calc(var(--pad) + var(--tree-depth) * var(--indent));
    --line: var(--border-default);
    --path: color-mix(in srgb, var(--accent-blue) 70%, transparent);
    position: relative;
    flex-shrink: 0;
    min-height: 24px;
    padding: 4px 6px 4px calc(var(--x) + 10px);
    display: block;
    width: 100%;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    text-align: left;
    text-decoration: none;
    cursor: pointer;
    outline: none;
  }

  .input-row:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .input-row:focus-visible {
    box-shadow: inset 0 0 0 2px var(--accent-blue);
  }

  .input-row.on-path {
    color: var(--text-primary);
  }

  .input-row.selected {
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
  }

  .rail,
  .line,
  .elbow {
    position: absolute;
    pointer-events: none;
  }

  .rail {
    left: calc(var(--pad) + var(--col) * var(--indent));
    top: 0;
    bottom: 0;
    width: 1px;
    background: var(--line);
  }

  .line {
    left: var(--x);
    width: 1px;
    background: var(--line);
  }

  .line-above {
    top: 0;
    height: var(--mid);
  }

  .line-below {
    top: var(--mid);
    bottom: 0;
  }

  .on-path .line-above,
  .line-below.path-line {
    background: var(--path);
  }

  .elbow {
    left: calc(var(--x) - var(--indent));
    width: var(--indent);
    top: var(--mid);
    bottom: calc(100% - var(--mid));
    border-left: 1px solid var(--line);
  }

  .elbow.from-above {
    top: 0;
  }

  .elbow.continues {
    bottom: 0;
  }

  /* The horizontal arm into the marker. */
  .elbow::after {
    content: "";
    position: absolute;
    left: 0;
    width: 100%;
    top: var(--mid);
    height: 1px;
    background: var(--line);
  }

  .elbow:not(.from-above)::after {
    top: 0;
  }

  .on-path .elbow::after {
    background: var(--path);
  }

  .marker {
    position: absolute;
    z-index: 1;
    left: calc(var(--x) - 3.5px);
    top: calc(var(--mid) - 3.5px);
    width: 7px;
    height: 7px;
    box-sizing: border-box;
    border-radius: 50%;
    border: 1px solid var(--text-muted);
    background: var(--bg-surface);
  }

  .on-path .marker {
    border-color: var(--accent-blue);
  }

  .branch-point .marker {
    left: calc(var(--x) - 4.5px);
    top: calc(var(--mid) - 4.5px);
    width: 9px;
    height: 9px;
    border-color: var(--accent-blue);
    background: var(--accent-blue);
  }

  .leaf .marker {
    border-color: var(--accent-green, var(--accent-blue));
    background: var(--accent-green, var(--accent-blue));
  }

  .selected .marker {
    border-color: var(--text-primary);
    background: var(--text-primary);
  }

  .input-body {
    min-width: 0;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 2px 6px;
  }

  .input-preview {
    flex: 1 1 100%;
    min-width: 0;
    overflow: hidden;
    font-size: 11px;
    line-height: 16px;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .input-preview.shell {
    font-family: var(--font-mono);
    font-size: 10px;
  }

  .input-end {
    display: inline-flex;
    align-items: center;
    min-width: 0;
    gap: 6px;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    line-height: 1.35;
  }

  .input-end-rel {
    flex-shrink: 0;
    padding: 1px 5px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-xs, 4px);
    font-size: 8px;
    text-transform: uppercase;
  }

  .input-end-rel.current {
    border-color: var(--accent-blue);
    color: var(--accent-blue);
  }

  .input-end-count {
    white-space: nowrap;
  }

  .gap-line {
    top: 0;
    bottom: 0;
    background: none;
    border-left: 1px dashed var(--line);
  }

  .gap.on-path .gap-line {
    border-left-color: var(--path);
  }

  .gap-label {
    color: var(--text-muted);
    font-size: 10px;
    font-style: italic;
    line-height: 16px;
  }

  .gap:hover .gap-label {
    color: var(--text-secondary);
  }
</style>
