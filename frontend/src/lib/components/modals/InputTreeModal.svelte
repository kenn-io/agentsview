<script lang="ts">
  import { Button, IconButton, Modal, Toggle } from "@kenn-io/kit-ui";
  import { tick } from "svelte";
  import { fetchSessionInputTree } from "../../api/inputTree.js";
  import type {
    DbSession,
    ServiceInputTree,
    ServiceInputTreeNode,
  } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { ScanIcon, ZoomInIcon, ZoomOutIcon } from "../../icons.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { formatNumber, formatTimestamp } from "../../utils/format.js";
  import { PROJECT_PALETTE } from "../../utils/projectColor.js";
  import {
    inputGraphPathTo,
    layoutInputGraph,
    type InputGraphItem,
  } from "../content/input-graph.js";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();

  const ZOOM_MIN = 0.25;
  const ZOOM_MAX = 2;
  const ZOOM_STEPS = [0.25, 0.33, 0.5, 0.67, 0.8, 1, 1.25, 1.5, 2];
  // Blue marks the open session's path, so sessions take the other colours.
  const SESSION_COLORS = PROJECT_PALETTE.slice(1);

  let tree = $state<ServiceInputTree | null>(null);
  let loading = $state(false);
  let error = $state(false);
  let fold = $state(true);
  // Folded runs the reader opened, by the index of their first input.
  let expandedRuns = $state<ReadonlySet<number>>(new Set());
  let zoom = $state(1);
  // The card under the pointer or keyboard focus; its path is highlighted.
  let hoverKey = $state<string | null>(null);
  let viewport = $state<HTMLElement | null>(null);
  let panning = $state(false);
  let pan: {
    pointerId: number;
    x: number;
    y: number;
    left: number;
    top: number;
  } | null = null;

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
  const sessions = $derived(tree?.sessions ?? []);
  const sessionsById = $derived(
    new Map<string, DbSession>(sessions.map((s) => [s.id, s])),
  );
  const sessionColors = $derived(
    new Map(
      sessions.map((s, i) => [s.id, SESSION_COLORS[i % SESSION_COLORS.length]!]),
    ),
  );
  const activeSessionId = $derived(tree?.active_session_id ?? sessionId);
  // The input selected in the conversation, when it is on the active path.
  const selectedIndex = $derived.by(() => {
    const ordinal = ui.selectedOrdinal;
    if (ordinal === null) return -1;
    return nodes.findIndex((node) => node.active_ordinal === ordinal);
  });
  const layout = $derived(
    layoutInputGraph(nodes, {
      activeSessionId,
      sessionOrder: sessions.map((s) => s.id),
      fold,
      expandedRuns,
      keepVisible: new Set(selectedIndex >= 0 ? [selectedIndex] : []),
    }),
  );
  // The card the view opens on: the selected input, or else the open
  // session's leaf.
  const focusItem = $derived(
    layout.items.find((item) =>
      selectedIndex >= 0
        ? item.kind === "input" && item.index === selectedIndex
        : item.kind === "session" && item.sessionId === activeSessionId,
    ),
  );
  const hoverPath = $derived(inputGraphPathTo(layout, hoverKey));
  const hoverEdges = $derived(layout.edges.filter((edge) => hoverPath.has(edge.to)));

  // Centre the focused card once the tree loads.
  $effect(() => {
    const view = viewport;
    const loaded = tree;
    if (!view || !loaded) return;
    void tick().then(() => {
      if (viewport === view && tree === loaded) centerOn(focusItem);
    });
  });

  // Ctrl or Cmd with the wheel zooms around the pointer. The listener is
  // not passive so it can stop the page from zooming instead.
  $effect(() => {
    const view = viewport;
    if (!view) return;
    const onWheel = (event: WheelEvent) => {
      if (!event.ctrlKey && !event.metaKey) return;
      event.preventDefault();
      const rect = view.getBoundingClientRect();
      setZoom(zoom * Math.exp(-event.deltaY * 0.0015), {
        x: event.clientX - rect.left,
        y: event.clientY - rect.top,
      });
    };
    view.addEventListener("wheel", onWheel, { passive: false });
    return () => view.removeEventListener("wheel", onWheel);
  });

  function close() {
    ui.activeModal = null;
  }

  function centerOn(item: InputGraphItem | undefined) {
    const view = viewport;
    if (!view || !item) return;
    view.scrollLeft = (item.x + item.width / 2) * zoom - view.clientWidth / 2;
    view.scrollTop = (item.y + item.height / 2) * zoom - view.clientHeight / 2;
  }

  // Changes the zoom and keeps the graph point under the anchor, a position
  // in the viewport, in place. The anchor defaults to the viewport centre.
  function setZoom(value: number, anchor?: { x: number; y: number }) {
    const next = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, value));
    const view = viewport;
    if (next === zoom) return;
    if (!view) {
      zoom = next;
      return;
    }
    const x = anchor?.x ?? view.clientWidth / 2;
    const y = anchor?.y ?? view.clientHeight / 2;
    const graphX = (view.scrollLeft + x) / zoom;
    const graphY = (view.scrollTop + y) / zoom;
    zoom = next;
    void tick().then(() => {
      view.scrollLeft = graphX * next - x;
      view.scrollTop = graphY * next - y;
    });
  }

  function zoomIn() {
    setZoom(ZOOM_STEPS.find((step) => step > zoom + 0.001) ?? ZOOM_MAX);
  }

  function zoomOut() {
    setZoom(ZOOM_STEPS.findLast((step) => step < zoom - 0.001) ?? ZOOM_MIN);
  }

  function zoomToFit() {
    const view = viewport;
    if (!view || layout.width === 0) return;
    zoom = Math.min(
      1,
      Math.max(
        ZOOM_MIN,
        Math.min(view.clientWidth / layout.width, view.clientHeight / layout.height),
      ),
    );
    void tick().then(() => centerOn(focusItem));
  }

  function setFold(next: boolean) {
    fold = next;
    expandedRuns = new Set();
  }

  function expandRun(runStart: number) {
    expandedRuns = new Set([...expandedRuns, runStart]);
  }

  // Dragging the background scrolls the graph.
  function startPan(event: PointerEvent) {
    const view = viewport;
    const target = event.target;
    if (!view || event.button !== 0 || !(target instanceof Element)) return;
    // Leave clicks on cards, and scrollbar drags, to the browser.
    if (!target.closest(".graph-sizer") || target.closest(".graph-node")) return;
    event.preventDefault();
    pan = {
      pointerId: event.pointerId,
      x: event.clientX,
      y: event.clientY,
      left: view.scrollLeft,
      top: view.scrollTop,
    };
    panning = true;
    view.setPointerCapture?.(event.pointerId);
  }

  function movePan(event: PointerEvent) {
    const view = viewport;
    if (!view || !pan || event.pointerId !== pan.pointerId) return;
    view.scrollLeft = pan.left - (event.clientX - pan.x);
    view.scrollTop = pan.top - (event.clientY - pan.y);
  }

  function endPan(event: PointerEvent) {
    if (!pan || event.pointerId !== pan.pointerId) return;
    viewport?.releasePointerCapture?.(event.pointerId);
    pan = null;
    panning = false;
  }

  function isPlainClick(event: MouseEvent): boolean {
    return (
      event.button === 0 &&
      !event.metaKey &&
      !event.ctrlKey &&
      !event.shiftKey &&
      !event.altKey
    );
  }

  function isInActiveView(node: ServiceInputTreeNode): boolean {
    return node.active_ordinal !== undefined && node.active_ordinal !== null;
  }

  // Inputs the open session's message view shows jump within it; the
  // others, including those a continuation carries on from the session it
  // continues, open the session that holds them.
  function inputHref(node: ServiceInputTreeNode): string {
    if (isInActiveView(node)) {
      return router.buildSessionHref(sessionId, {
        msg: String(node.active_ordinal),
      });
    }
    return router.buildSessionHref(node.session_id, {
      msg: String(node.ordinal),
    });
  }

  function inputTitle(node: ServiceInputTreeNode): string {
    return isInActiveView(node)
      ? m.session_input_tree_jump_to({ preview: node.preview })
      : m.session_input_tree_open({ preview: node.preview });
  }

  function openInput(event: MouseEvent, node: ServiceInputTreeNode) {
    if (!isPlainClick(event)) return;
    event.preventDefault();
    if (isInActiveView(node)) {
      ui.scrollToOrdinal(node.active_ordinal!);
    } else {
      ui.scrollToOrdinal(node.ordinal, node.session_id);
      router.navigateToSession(node.session_id);
    }
    close();
  }

  function openSession(event: MouseEvent, id: string) {
    if (!isPlainClick(event)) return;
    event.preventDefault();
    if (id !== sessionId) router.navigateToSession(id);
    close();
  }

  function sessionTitle(id: string): string {
    const session = sessionsById.get(id);
    const title = (session?.display_name || session?.first_message || "").trim();
    return title || m.session_tree_untitled();
  }

  function relationshipLabel(id: string): string {
    switch (sessionsById.get(id)?.relationship_type) {
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

  function itemStyle(item: InputGraphItem, sessionOf: string): string {
    const color = sessionColors.get(sessionOf) ?? "var(--text-muted)";
    return [
      `left: ${item.x}px`,
      `top: ${item.y}px`,
      `width: ${item.width}px`,
      `height: ${item.height}px`,
      `--session-color: ${color}`,
    ].join("; ");
  }

  function hover(key: string | null) {
    hoverKey = key;
  }
</script>

<Modal
  title={m.session_input_graph_title()}
  closeLabel={m.session_input_graph_close()}
  width="min(1440px, calc(100vw - 32px))"
  maxWidth="calc(100vw - 32px)"
  maxHeight="calc(100svh - 32px)"
  onclose={close}
>
  <div class="graph-shell">
    <div class="graph-toolbar">
      {#if tree}
        <span class="graph-stats">
          <span>
            {m.session_input_graph_sessions({
              count: sessions.length,
              countLabel: formatNumber(sessions.length),
            })}
          </span>
          <span>
            {m.session_input_graph_inputs({
              count: nodes.length,
              countLabel: formatNumber(nodes.length),
            })}
          </span>
          {#if tree.truncated}
            <span class="graph-truncated">{m.session_tree_truncated()}</span>
          {/if}
        </span>
      {/if}
      <span class="graph-hint">{m.session_input_graph_hint()}</span>
      <Toggle checked={fold} label={m.session_input_graph_fold()} onchange={setFold} />
      <span class="graph-zoom">
        <IconButton
          size="sm"
          ariaLabel={m.session_input_graph_zoom_out()}
          disabled={zoom <= ZOOM_MIN}
          onclick={zoomOut}
        >
          <ZoomOutIcon size={14} aria-hidden="true" />
        </IconButton>
        <Button
          size="sm"
          surface="soft"
          class="graph-zoom-level"
          ariaLabel={m.session_input_graph_zoom_reset()}
          title={m.session_input_graph_zoom_reset()}
          onclick={() => setZoom(1)}
        >
          {Math.round(zoom * 100)}%
        </Button>
        <IconButton
          size="sm"
          ariaLabel={m.session_input_graph_zoom_in()}
          disabled={zoom >= ZOOM_MAX}
          onclick={zoomIn}
        >
          <ZoomInIcon size={14} aria-hidden="true" />
        </IconButton>
        <IconButton
          size="sm"
          ariaLabel={m.session_input_graph_zoom_fit()}
          disabled={!tree || layout.width === 0}
          onclick={zoomToFit}
        >
          <ScanIcon size={14} aria-hidden="true" />
        </IconButton>
      </span>
    </div>

    {#if loading}
      <div class="graph-state">{m.session_input_tree_loading()}</div>
    {:else if error}
      <div class="graph-state error">{m.session_input_tree_error()}</div>
    {:else if tree && nodes.length === 0}
      <div class="graph-state">{m.session_input_tree_empty()}</div>
    {:else if tree}
      <div
        class="graph-viewport"
        class:panning
        role="group"
        aria-label={m.session_input_graph_canvas()}
        bind:this={viewport}
        onpointerdown={startPan}
        onpointermove={movePan}
        onpointerup={endPan}
        onpointercancel={endPan}
      >
        <div
          class="graph-sizer"
          style:width={`${layout.width * zoom}px`}
          style:height={`${layout.height * zoom}px`}
        >
          <div
            class="graph-stage"
            style:width={`${layout.width}px`}
            style:height={`${layout.height}px`}
            style:transform={`scale(${zoom})`}
          >
            <svg
              class="graph-edges"
              width={layout.width}
              height={layout.height}
              aria-hidden="true"
            >
              {#each layout.edges as edge (edge.key)}
                <path d={edge.d} class:on-path={edge.onActivePath} />
              {/each}
              {#each hoverEdges as edge (edge.key)}
                <path d={edge.d} class="hover-path" />
              {/each}
            </svg>

            {#each layout.items as item (item.key)}
              {#if item.kind === "input"}
                {@const node = item.node}
                <a
                  class="graph-node input-node"
                  class:on-path={item.onActivePath}
                  class:selected={item.index === selectedIndex}
                  class:branch-point={item.branches > 1}
                  class:shell={node.is_shell}
                  class:hovered={hoverPath.has(item.key)}
                  href={inputHref(node)}
                  title={inputTitle(node)}
                  aria-current={item.index === selectedIndex ? "step" : undefined}
                  style={itemStyle(item, node.session_id)}
                  onclick={(event) => openInput(event, node)}
                  onmouseenter={() => hover(item.key)}
                  onmouseleave={() => hover(null)}
                  onfocus={() => hover(item.key)}
                  onblur={() => hover(null)}
                >
                  <span class="node-meta">
                    <span class="node-time">
                      {node.timestamp ? formatTimestamp(node.timestamp) : ""}
                    </span>
                    {#if item.branches > 1}
                      <span class="node-branches">
                        {m.session_input_graph_branches({
                          count: item.branches,
                          countLabel: formatNumber(item.branches),
                        })}
                      </span>
                    {/if}
                  </span>
                  <span class="node-preview">{node.preview}</span>
                </a>
              {:else if item.kind === "run"}
                <button
                  type="button"
                  class="graph-node run-node"
                  class:on-path={item.onActivePath}
                  class:hovered={hoverPath.has(item.key)}
                  title={m.session_input_tree_show_hidden()}
                  style={itemStyle(item, item.sessionId)}
                  onclick={() => expandRun(item.runStart)}
                  onmouseenter={() => hover(item.key)}
                  onmouseleave={() => hover(null)}
                  onfocus={() => hover(item.key)}
                  onblur={() => hover(null)}
                >
                  {m.session_input_tree_hidden({
                    count: item.hidden,
                    countLabel: formatNumber(item.hidden),
                  })}
                </button>
              {:else}
                {@const session = sessionsById.get(item.sessionId)}
                {@const current = item.sessionId === activeSessionId}
                <a
                  class="graph-node session-node"
                  class:on-path={item.onActivePath}
                  class:current
                  class:hovered={hoverPath.has(item.key)}
                  href={router.buildSessionHref(item.sessionId)}
                  title={m.session_input_graph_open_session({
                    title: sessionTitle(item.sessionId),
                  })}
                  aria-current={current ? "page" : undefined}
                  style={itemStyle(item, item.sessionId)}
                  onclick={(event) => openSession(event, item.sessionId)}
                  onmouseenter={() => hover(item.key)}
                  onmouseleave={() => hover(null)}
                  onfocus={() => hover(item.key)}
                  onblur={() => hover(null)}
                >
                  <span class="session-title">{sessionTitle(item.sessionId)}</span>
                  <span class="session-meta">
                    <span class="session-badge">{relationshipLabel(item.sessionId)}</span>
                    {#if current}
                      <span class="session-badge current">
                        {m.session_input_tree_current()}
                      </span>
                    {/if}
                    {#if session}
                      <span class="session-fact">{session.agent}</span>
                      <span class="session-fact">
                        {messageCountLabel(session.message_count)}
                      </span>
                    {/if}
                  </span>
                  <span class="session-time">
                    {session?.started_at ? formatTimestamp(session.started_at) : ""}
                  </span>
                </a>
              {/if}
            {/each}
          </div>
        </div>
      </div>
    {/if}
  </div>
</Modal>

<style>
  .graph-shell {
    /* The modal panel is capped at the viewport less 32px; this leaves room
       for its header and body padding so only the graph scrolls. */
    height: calc(100svh - 124px);
    min-height: 320px;
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .graph-toolbar {
    flex-shrink: 0;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-4) var(--space-6);
    font-size: 11px;
  }

  .graph-stats {
    display: inline-flex;
    align-items: center;
    gap: var(--space-5);
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: 10px;
  }

  .graph-truncated {
    color: var(--warning-fg, var(--text-muted));
  }

  .graph-hint {
    margin-right: auto;
    color: var(--text-muted);
    font-size: 10px;
  }

  .graph-zoom {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }

  .graph-zoom :global(.graph-zoom-level) {
    min-width: 52px;
    font-family: var(--font-mono);
    font-variant-numeric: tabular-nums;
  }

  .graph-state {
    padding: 24px 4px;
    color: var(--text-muted);
    font-size: 12px;
  }

  .graph-state.error {
    color: var(--slow-fg);
  }

  .graph-viewport {
    flex: 1;
    min-height: 0;
    overflow: auto;
    display: flex;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background-color: var(--bg-inset);
    background-image: radial-gradient(var(--border-default) 1px, transparent 1px);
    background-size: 18px 18px;
    background-attachment: local;
    cursor: grab;
  }

  .graph-viewport.panning {
    cursor: grabbing;
    user-select: none;
  }

  .graph-sizer {
    /* Auto margins centre a small graph and fall back to zero when it
       overflows, so every part stays reachable by scrolling. */
    position: relative;
    flex-shrink: 0;
    margin: auto;
  }

  .graph-stage {
    position: absolute;
    top: 0;
    left: 0;
    transform-origin: 0 0;
  }

  .graph-edges {
    position: absolute;
    inset: 0;
    overflow: visible;
    pointer-events: none;
  }

  .graph-edges path {
    fill: none;
    stroke: var(--text-muted);
    stroke-opacity: 0.6;
    stroke-width: 1.5;
  }

  .graph-edges path.on-path {
    stroke: var(--accent-blue);
    stroke-opacity: 1;
    stroke-width: 2.5;
  }

  .graph-edges path.hover-path {
    stroke: var(--text-primary);
    stroke-opacity: 0.8;
    stroke-width: 2;
  }

  .graph-node {
    position: absolute;
    box-sizing: border-box;
    overflow: hidden;
    padding: 7px 10px 7px 13px;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    box-shadow: var(--shadow-sm, none);
    color: var(--text-secondary);
    font: inherit;
    text-align: left;
    text-decoration: none;
    cursor: pointer;
    outline: none;
  }

  /* The stripe takes the colour of the session that holds the input. */
  .graph-node::before {
    content: "";
    position: absolute;
    left: 0;
    top: 0;
    bottom: 0;
    width: 3px;
    background: var(--session-color);
  }

  .graph-node:hover,
  .graph-node.hovered {
    border-color: var(--text-muted);
    color: var(--text-primary);
  }

  .graph-node:hover {
    background: var(--bg-surface-hover);
  }

  .graph-node:focus-visible {
    box-shadow: 0 0 0 2px var(--accent-blue);
  }

  .graph-node.on-path {
    border-color: color-mix(in srgb, var(--accent-blue) 60%, var(--border-default));
    color: var(--text-primary);
  }

  .input-node.selected {
    border-color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 12%, var(--bg-surface));
    box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent-blue) 35%, transparent);
  }

  .node-meta {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    min-width: 0;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    line-height: 13px;
    white-space: nowrap;
  }

  .node-time {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .node-branches {
    flex-shrink: 0;
    padding: 0 5px;
    border-radius: var(--radius-xs, 4px);
    background: color-mix(in srgb, var(--accent-blue) 14%, transparent);
    color: var(--accent-blue);
  }

  .node-preview {
    display: -webkit-box;
    overflow: hidden;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    font-size: 11px;
    line-height: 15px;
    overflow-wrap: anywhere;
  }

  .shell .node-preview {
    font-family: var(--font-mono);
    font-size: 10px;
  }

  .run-node {
    flex-direction: row;
    align-items: center;
    justify-content: center;
    padding-block: 0;
    border-style: dashed;
    background: var(--bg-surface);
    box-shadow: none;
    color: var(--text-muted);
    font-size: 10px;
    font-style: italic;
  }

  .run-node.on-path {
    border-color: color-mix(in srgb, var(--accent-blue) 60%, var(--border-default));
  }

  .session-node {
    justify-content: center;
    border-radius: 14px;
    border-color: color-mix(in srgb, var(--session-color) 45%, var(--border-default));
    background: color-mix(in srgb, var(--session-color) 9%, var(--bg-surface));
  }

  .session-node:hover {
    background: color-mix(in srgb, var(--session-color) 15%, var(--bg-surface));
  }

  .session-node.current {
    border-color: var(--accent-blue);
    box-shadow: 0 0 0 1px var(--accent-blue);
  }

  .session-title {
    min-width: 0;
    overflow: hidden;
    color: var(--text-primary);
    font-size: 11px;
    font-weight: 620;
    line-height: 15px;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .session-meta {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    overflow: hidden;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    line-height: 14px;
    white-space: nowrap;
  }

  .session-badge {
    flex-shrink: 0;
    padding: 0 5px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-xs, 4px);
    font-size: 8px;
    text-transform: uppercase;
  }

  .session-badge.current {
    border-color: var(--accent-blue);
    color: var(--accent-blue);
  }

  .session-fact {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .session-time {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    line-height: 13px;
  }
</style>
