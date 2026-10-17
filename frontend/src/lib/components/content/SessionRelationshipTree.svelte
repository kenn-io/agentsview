<script lang="ts">
  import {
    IconButton,
    SegmentedControl,
    SplitResizeHandle,
    type SegmentedControlOption,
    type SplitResizeEvent,
  } from "@kenn-io/kit-ui";
  import { tick } from "svelte";
  import { fetchSessionTree } from "../../api/sessionTree.js";
  import type { Session } from "../../api/types/core.js";
  import type {
    SessionTreeNode,
    SessionTreeResponse,
  } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { Maximize2Icon } from "../../icons.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui, type SessionTreeView } from "../../stores/ui.svelte.js";
  import { formatNumber } from "../../utils/format.js";
  import SessionInputTree from "./SessionInputTree.svelte";
  import { inForkFamily } from "./input-tree.js";
  import {
    SESSION_TREE_HEIGHT_MIN,
    SESSION_TREE_HEIGHT_STORAGE_MAX,
    clampSessionTreeHeightForLayout,
  } from "./session-tree-height.js";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();

  let tree = $state<SessionTreeResponse | null>(null);
  let requestSerial = 0;

  $effect(() => {
    const id = sessionId;
    const serial = ++requestSerial;
    tree = null;

    fetchSessionTree(id)
      .then((next) => {
        if (serial === requestSerial) {
          tree = next;
        }
      })
      .catch((err) => {
        if (serial === requestSerial) {
          tree = null;
        }
        console.error("failed to load session tree", err);
      });
  });

  function flattenTree(node: SessionTreeNode): SessionTreeNode[] {
    const rows = [node];
    for (const child of node.children) {
      rows.push(...flattenTree(child));
    }
    return rows;
  }

  let rows = $derived(tree ? flattenTree(tree.root) : []);
  let visible = $derived(rows.length > 1);
  // The input view needs a fork or continuation that links the session's
  // inputs to another session's; other sessions show only the session view.
  let hasFamily = $derived(
    tree ? inForkFamily(tree.root, tree.active_session_id) : false,
  );
  let view = $derived<SessionTreeView>(
    hasFamily ? ui.sessionTreeView : "sessions",
  );
  const viewOptions = $derived<SegmentedControlOption[]>([
    { value: "sessions", label: m.session_tree_view_sessions() },
    { value: "inputs", label: m.session_tree_view_inputs() },
  ]);

  // Sizes below are CSS pixels (clientHeight/offsetHeight), not
  // getBoundingClientRect, which includes interface zoom.
  let treeElement = $state<HTMLElement | null>(null);
  let listElement = $state<HTMLElement | null>(null);
  // Height of the column the tree shares with the session vitals, or null
  // until it has been laid out.
  let columnHeight = $state<number | null>(null);
  let resizeStartHeight = 0;

  const columnLimit = $derived(
    columnHeight ?? Number.POSITIVE_INFINITY,
  );
  // The stored height is a limit: shorter trees keep their natural height
  // and taller ones scroll inside it.
  const maxHeight = $derived(
    clampSessionTreeHeightForLayout(ui.sessionTreeHeight, columnLimit),
  );
  const layoutMaxHeight = $derived(
    clampSessionTreeHeightForLayout(
      SESSION_TREE_HEIGHT_STORAGE_MAX,
      columnLimit,
    ),
  );

  function measureColumnHeight(): number | null {
    const height = treeElement?.parentElement?.clientHeight ?? 0;
    columnHeight = height > 0 ? height : null;
    return columnHeight;
  }

  $effect(() => {
    const column = treeElement?.parentElement;
    if (!column) return;
    measureColumnHeight();
    const observer = new ResizeObserver(() => {
      measureColumnHeight();
    });
    observer.observe(column);
    return () => observer.disconnect();
  });

  // Each loaded tree starts scrolled to the top, so bring the active
  // session's node into view when it is below the height limit.
  $effect(() => {
    const list = listElement;
    const loaded = tree;
    if (!list || !loaded) return;
    // Let the height limit settle against the measured column first.
    void tick().then(() => {
      if (listElement === list && tree === loaded) {
        scrollActiveNodeIntoView(list);
      }
    });
  });

  function scrollActiveNodeIntoView(list: HTMLElement) {
    const node = list.querySelector<HTMLElement>(".tree-node.active");
    if (!node) return;
    // Scroll the list directly: scrollIntoView would also scroll the
    // vitals column and the page layout around it.
    const top = node.offsetTop;
    const bottom = top + node.offsetHeight;
    const viewTop = list.scrollTop;
    const viewBottom = viewTop + list.clientHeight;
    if (top >= viewTop && bottom <= viewBottom) return;
    // Center the node so its parent and siblings stay visible around it.
    list.scrollTop = top - (list.clientHeight - node.offsetHeight) / 2;
  }

  function handleResizeStart() {
    // Start from the rendered height so a tree shorter than its limit
    // follows the handle right away.
    const renderedHeight = treeElement?.offsetHeight ?? 0;
    resizeStartHeight =
      renderedHeight > 0 ? Math.min(renderedHeight, maxHeight) : maxHeight;
  }

  function handleResize(event: SplitResizeEvent) {
    // The handle sits below the tree, so moving it down (positive delta)
    // makes the tree taller.
    const nextHeight = clampSessionTreeHeightForLayout(
      resizeStartHeight + event.delta,
      measureColumnHeight() ?? Number.POSITIVE_INFINITY,
    );

    // Skip persisting when the clamp lands on the rendered limit so a taller
    // stored preference survives drags in a short window.
    if (nextHeight === maxHeight) return;
    ui.setSessionTreeHeight(nextHeight);
  }

  function sessionTitle(session: Session): string {
    const title = (
      session.display_name ||
      session.first_message ||
      ""
    ).trim();
    return title || m.session_tree_untitled();
  }

  function sessionPreview(session: Session): string {
    const first = (session.first_message || "").trim();
    if (!first || first === sessionTitle(session)) return session.id;
    return first;
  }

  function relationshipLabel(value: string | undefined): string {
    switch (value) {
      case "fork":
        return m.session_tree_relationship_fork();
      case "subagent":
        return m.session_tree_relationship_subagent();
      case "continuation":
        return m.session_tree_relationship_continuation();
      default:
        return m.session_tree_relationship_root();
    }
  }

  function messageCountLabel(count: number): string {
    return m.session_tree_message_count({
      count,
      countLabel: formatNumber(count),
    });
  }

  function navigate(
    event: MouseEvent & { currentTarget: HTMLAnchorElement },
    id: string,
  ) {
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
    router.navigateToSession(id);
  }
</script>

{#if visible}
  <section
    class="session-tree"
    aria-label={m.session_tree_title()}
    bind:this={treeElement}
    style:max-height={`${maxHeight}px`}
  >
    <header class="tree-header">
      <span class="tree-heading">{m.session_tree_title()}</span>
      {#if tree?.truncated && view === "sessions"}
        <span class="tree-truncated">{m.session_tree_truncated()}</span>
      {/if}
      {#if hasFamily}
        <span class="tree-view-switch">
          <SegmentedControl
            options={viewOptions}
            value={view}
            ariaLabel={m.session_tree_view_label()}
            onchange={(next) => ui.setSessionTreeView(next as SessionTreeView)}
          />
        </span>
        <span class="tree-open-graph">
          <IconButton
            size="sm"
            ariaLabel={m.session_input_graph_open()}
            ariaHaspopup="dialog"
            onclick={() => (ui.activeModal = "inputTree")}
          >
            <Maximize2Icon size={12} aria-hidden="true" />
          </IconButton>
        </span>
      {/if}
    </header>

    {#if view === "inputs"}
      <SessionInputTree {sessionId} />
    {:else}
      <div class="tree-list" bind:this={listElement}>
        {#each rows as node (node.session.id)}
          {@const session = node.session}
          <a
            class="tree-node"
            class:active={node.is_active}
            class:leaf={node.is_leaf}
            class:branch-start={node.is_branch_start}
            href={router.buildSessionHref(session.id)}
            aria-current={node.is_active ? "page" : undefined}
            title={sessionTitle(session)}
            style={`--tree-depth: ${node.depth};`}
            onclick={(event) => navigate(event, session.id)}
          >
            <span class="tree-guide" aria-hidden="true"></span>
            <span class="tree-marker" aria-hidden="true"></span>
            <span class="tree-body">
              <span class="tree-row-main">
                <span class="tree-title">{sessionTitle(session)}</span>
                <span class="tree-rel">
                  {relationshipLabel(session.relationship_type)}
                </span>
              </span>
              <span class="tree-preview">{sessionPreview(session)}</span>
              <span class="tree-meta">
                <span>{session.agent}</span>
                <span>{messageCountLabel(session.message_count)}</span>
              </span>
            </span>
          </a>
        {/each}
      </div>
    {/if}
  </section>
  <SplitResizeHandle
    orientation="vertical"
    ariaLabel={m.session_tree_resize()}
    ariaValueMin={SESSION_TREE_HEIGHT_MIN}
    ariaValueMax={layoutMaxHeight}
    ariaValueNow={maxHeight}
    onResizeStart={handleResizeStart}
    onResize={handleResize}
    onResizeEnd={handleResize}
  />
{/if}

<style>
  .session-tree {
    flex-shrink: 0;
    display: flex;
    flex-direction: column;
    border-bottom: 1px solid var(--border-default);
    background: var(--bg-surface);
  }

  .tree-header {
    flex-shrink: 0;
    min-height: 34px;
    padding: 9px 14px 7px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    color: var(--text-muted);
    font-size: 9px;
    font-weight: 600;
    letter-spacing: 0.6px;
    text-transform: uppercase;
  }

  .tree-heading {
    margin-right: auto;
  }

  .tree-view-switch {
    flex-shrink: 0;
    --kit-control-height: 20px;
    --kit-control-font-size: 10px;
    letter-spacing: 0;
    text-transform: none;
  }

  .tree-open-graph {
    flex-shrink: 0;
    display: inline-flex;
    margin-left: -2px;
    letter-spacing: 0;
    text-transform: none;
  }

  .tree-truncated {
    color: var(--warning-fg, var(--text-muted));
    font-family: var(--font-mono);
    font-size: 9px;
    font-weight: 500;
    letter-spacing: 0;
    text-transform: none;
    white-space: nowrap;
  }

  .tree-list {
    /* Offset parent for the rows, so their offsetTop is in scroll space. */
    position: relative;
    min-height: 0;
    overflow-y: auto;
    padding: 0 8px 10px;
    display: grid;
    gap: 2px;
  }

  .tree-node {
    --depth-offset: calc(var(--tree-depth) * 14px);
    position: relative;
    min-height: 46px;
    padding: 6px 8px 6px calc(14px + var(--depth-offset));
    display: grid;
    grid-template-columns: 14px minmax(0, 1fr);
    gap: var(--space-3);
    align-items: start;
    border-radius: var(--radius-sm);
    color: inherit;
    text-decoration: none;
    outline: none;
  }

  .tree-node:hover {
    background: var(--bg-surface-hover);
  }

  .tree-node:focus-visible {
    box-shadow: 0 0 0 2px var(--accent-blue);
  }

  .tree-node.active {
    background: var(--selection-bg);
  }

  .tree-guide {
    position: absolute;
    left: calc(15px + var(--depth-offset));
    top: 0;
    bottom: 0;
    width: 1px;
    background: var(--border-muted);
  }

  .tree-marker {
    position: relative;
    z-index: 1;
    width: 8px;
    height: 8px;
    margin-top: 6px;
    border-radius: 50%;
    border: 1px solid var(--border-strong);
    background: var(--bg-surface);
  }

  .tree-node.leaf .tree-marker {
    border-color: var(--accent-green, var(--accent-blue));
    background: var(--accent-green, var(--accent-blue));
    box-shadow: 0 0 0 3px var(--accent-green-muted, transparent);
  }

  .tree-node.branch-start .tree-marker {
    width: 10px;
    height: 10px;
    margin-left: -1px;
    border-color: var(--accent-blue);
    background: var(--accent-blue);
  }

  .tree-node.active .tree-marker {
    border-color: var(--text-primary);
    background: var(--text-primary);
  }

  .tree-body {
    min-width: 0;
    display: grid;
    gap: 2px;
  }

  .tree-row-main {
    display: flex;
    align-items: center;
    min-width: 0;
    gap: 6px;
  }

  .tree-title {
    min-width: 0;
    overflow: hidden;
    color: var(--text-primary);
    font-size: 11px;
    font-weight: 620;
    line-height: 1.25;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .tree-rel {
    flex-shrink: 0;
    padding: 1px 5px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-xs, 4px);
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 8px;
    line-height: 1.35;
    text-transform: uppercase;
  }

  .tree-node.active .tree-rel {
    border-color: var(--accent-blue);
    color: var(--accent-blue);
  }

  .tree-preview {
    min-width: 0;
    overflow: hidden;
    color: var(--text-secondary);
    font-size: 10px;
    line-height: 1.25;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .tree-meta {
    display: flex;
    min-width: 0;
    gap: var(--space-3);
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    line-height: 1.25;
  }

  .tree-meta span {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
