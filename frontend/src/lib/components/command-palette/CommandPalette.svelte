<script lang="ts">
  import { m } from "../../i18n/index.js";
  import {
    Button,
    KbdBadge,
    SegmentedControl,
    type SegmentedControlOption,
  } from "@kenn-io/kit-ui";
  import { SearchIcon } from "../../icons.js";
  import { tick, onDestroy, untrack } from "svelte";
  import { ui } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { searchStore } from "../../stores/search.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import {
    formatRelativeTime,
    truncate,
    sanitizeSnippet, highlightTerms,
  } from "../../utils/format.js";
  import { agentColor, agentLabel } from "../../utils/agents.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { stripIdPrefix } from "../../utils/resume.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  import SemanticSetupHelp from "./SemanticSetupHelp.svelte";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import RangePicker from "../shared/RangePicker.svelte";
  import type { Session } from "../../api/types.js";
  import type {
    PaletteSearchResult,
    SearchMode,
  } from "../../stores/search.svelte.js";

  let inputRef: HTMLInputElement | undefined = $state(undefined);
  // Title and snippet widths (percent) of the placeholder rows shown while
  // a search runs; varied so they read as text, not as a striped block.
  const SKELETON_ROWS = [
    { title: 62, snippet: 88 },
    { title: 48, snippet: 80 },
    { title: 70, snippet: 92 },
    { title: 54, snippet: 76 },
    { title: 66, snippet: 84 },
  ];
  let project = $state(sessions.filters.project);
  let selectedIndex: number = $state(0);
  let inputValue: string = $state(searchStore.query ?? "");
  let searchModeOptions = $derived<SegmentedControlOption[]>([
    { value: "fulltext", label: m.command_palette_mode_fulltext() },
    { value: "semantic", label: m.command_palette_mode_semantic() },
    { value: "hybrid", label: m.command_palette_mode_hybrid() },
  ]);

  // Clear state and reset sort whenever the palette is unmounted, regardless
  // of close path (Escape key, overlay click, command-palette toggle, or any other
  // mechanism). This ensures stale results and in-flight requests are always
  // cancelled even when the caller bypasses close().
  onDestroy(() => {
    searchStore.endSession();
    searchStore.clear();
    searchStore.resetSort();
    searchStore.resetRange();
  });

  // Most Chinese, Japanese, and Korean words are one or two characters long,
  // so the three-character minimum that suits Latin text would keep common
  // CJK queries such as "消融" from ever reaching the server.
  const CJK_TEXT = /[\p{Script=Han}\p{Script=Hiragana}\p{Script=Katakana}\p{Script=Hangul}]/u;

  function isServerSearchQuery(query: string): boolean {
    if (CJK_TEXT.test(query)) return Array.from(query).length >= 2;
    return query.length >= 3;
  }

  // Filtered recent sessions (client-side filter)
  let recentSessions = $derived.by(() => {
    if (inputValue.length > 0 && !isServerSearchQuery(inputValue)) {
      const q = inputValue.toLowerCase();
      return sessions.sessions
        .filter(
          (s) =>
            s.project.toLowerCase().includes(q) ||
            (s.display_name?.toLowerCase().includes(q) ?? false) ||
            (s.first_message?.toLowerCase().includes(q) ?? false),
        )
        .slice(0, 10);
    }
    if (!inputValue) {
      return sessions.sessions.slice(0, 10);
    }
    return [];
  });

  // Combined results: server search results once the query is long enough,
  // else recent sessions
  let showSearchResults = $derived(isServerSearchQuery(inputValue));

  let totalItems = $derived(
    showSearchResults
      ? searchStore.results.length
      : recentSessions.length,
  );

  function handleInput(e: Event) {
    const target = e.target as HTMLInputElement;
    inputValue = target.value;
    selectedIndex = 0;

    if (isServerSearchQuery(inputValue)) {
      searchStore.search(inputValue, project);
    } else {
      searchStore.clear();
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    const interactiveTarget =
      e.target !== inputRef &&
      e.target instanceof Element &&
      e.target.closest(
        "button, a[href], input, select, textarea, [contenteditable='true'], " +
          "[role='button'], [role='checkbox'], [role='combobox'], " +
          // kit-ui-check-ignore: selector list for interactive event targets, not toggle markup
          "[role='menuitem'], [role='radio'], [role='switch'], [role='tab']",
      );
    if (e.key !== "Escape" && interactiveTarget) return;

    if (e.key === "ArrowDown") {
      e.preventDefault();
      selectedIndex = Math.min(selectedIndex + 1, totalItems - 1);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      selectedIndex = Math.max(selectedIndex - 1, 0);
    } else if (e.key === "Enter") {
      e.preventDefault();
      selectCurrent();
    }
  }

  // The range picker claims Escape at document level. Handle the outer
  // palette at window level so one keypress dismisses only the top layer.
  function handleEscape(e: KeyboardEvent) {
    if (e.key === "Escape" && !e.defaultPrevented) {
      e.preventDefault();
      e.stopPropagation();
      close();
    }
  }

  function retryActiveMode(target: EventTarget | null): boolean {
    const radio = target instanceof Element
      ? target.closest<HTMLElement>('[role="radio"]')
      : null;
    if (
      radio?.getAttribute("aria-checked") !== "true" ||
      searchStore.mode === "fulltext" ||
      searchStore.error === null ||
      !inputValue.trim()
    ) {
      return false;
    }
    searchStore.retry();
    selectedIndex = 0;
    return true;
  }

  function handleControlClick(e: MouseEvent) {
    retryActiveMode(e.target);
  }

  function handleControlKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") return;
    e.stopPropagation();
    if (
      (e.key === "Enter" || e.key === " ") &&
      retryActiveMode(e.target)
    ) {
      e.preventDefault();
    }
  }

  function selectCurrent() {
    if (showSearchResults) {
      const result = searchStore.results[selectedIndex];
      if (result) {
        selectSearchResult(result);
      }
    } else {
      const session = recentSessions[selectedIndex];
      if (session) {
        selectSession(session);
      }
    }
  }

  // Route-first: commit the URL and let App's deep-link effect own
  // selection and hydration, exactly as a direct deep link does.
  // Selecting through the sessions store before the route commits
  // starts hydration under the old route, where it can be cancelled
  // or lost (#1190).
  function selectSession(s: Session) {
    router.navigateToSession(s.id);
    close();
  }

  function selectSearchResult(r: PaletteSearchResult) {
    router.navigateToSession(r.session_id);
    if (r.ordinal !== -1) {
      ui.scrollToOrdinal(r.ordinal, r.session_id);
    } else {
      // Name-only match: clear any stale selection/scroll state so the
      // previously highlighted ordinal is not left active.
      ui.clearScrollState();
    }
    close();
  }

  function close() {
    inputValue = "";
    ui.activeModal = null;
  }

  // A click event targets the common ancestor of its mousedown and mouseup, so a
  // drag crossing the palette edge (selecting the query text, say) targets the
  // overlay. Dismiss only when press and release both land on the overlay.
  let pressedOverlay = false;

  function isOverlay(e: MouseEvent) {
    return (e.target as HTMLElement).classList.contains("palette-overlay");
  }

  function handleOverlayMousedown(e: MouseEvent) {
    pressedOverlay = isOverlay(e);
  }

  function handleOverlayMouseup(e: MouseEvent) {
    const pressed = pressedOverlay;
    pressedOverlay = false;
    if (pressed && isOverlay(e)) {
      close();
    }
  }

  $effect(() => {
    if (inputRef) {
      inputRef.focus();
      if (untrack(() => inputValue)) {
        inputRef.select();
      }
    }
  });

  $effect(() => {
    const _idx = selectedIndex;
    tick().then(() => {
      const el = document.querySelector(
        ".palette-results .palette-item.selected",
      );
      if (el) el.scrollIntoView({ block: "nearest" });
    });
  });
</script>

<svelte:window onkeydown={handleEscape} />

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="palette-overlay"
  onmousedown={handleOverlayMousedown}
  onmouseup={handleOverlayMouseup}
  onkeydown={handleKeydown}
>
  <div class="palette">
    <div class="palette-input-wrap">
      <SearchIcon class="search-icon" size="14" strokeWidth="2" aria-hidden="true" />
      <input
        bind:this={inputRef}
        type="text"
        class="palette-input"
        placeholder={m.command_palette_placeholder()}
        value={inputValue}
        oninput={handleInput}
      />
      <KbdBadge keys={["Esc"]} ariaLabel="Escape" />
    </div>

    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div
      class="palette-controls"
      onclick={handleControlClick}
      onkeydown={handleControlKeydown}
    >
      <SegmentedControl
        options={searchModeOptions}
        value={searchStore.mode}
        onchange={(value) => {
          searchStore.setMode(value as SearchMode);
          selectedIndex = 0;
        }}
        ariaLabel={m.command_palette_search_mode_label()}
      />
      {#if showSearchResults}
        <ProjectTypeahead
          projects={sessions.projects}
          value={project}
          onselect={(value) => {
            project = value;
            searchStore.search(inputValue, project);
            selectedIndex = 0;
          }}
        />
        <RangePicker
          selection={searchStore.range}
          onSelect={(selection) => {
            searchStore.setRange(selection);
            selectedIndex = 0;
          }}
          align="right"
        />
      {/if}
      {#if showSearchResults && searchStore.mode === "fulltext"}
        <div class="palette-sort">
          <button
            class="sort-btn"
            class:active={searchStore.sort === "relevance"}
            onmousedown={(e: MouseEvent) => e.preventDefault()}
            onclick={() => { searchStore.setSort("relevance"); selectedIndex = 0; }}
          >{m.command_palette_relevance()}</button>
          <button
            class="sort-btn"
            class:active={searchStore.sort === "recency"}
            onmousedown={(e: MouseEvent) => e.preventDefault()}
            onclick={() => { searchStore.setSort("recency"); selectedIndex = 0; }}
          >{m.command_palette_recency()}</button>
        </div>
      {/if}
      {#if searchStore.mode !== "fulltext" && !searchStore.error}
        <a class="palette-server-settings" href="https://agentsview.io/docs/semantic-search/#named-embeddings-servers" target="_blank" rel="noreferrer">{m.command_palette_semantic_setup_local_link()}</a>
      {/if}
    </div>

    <div class="palette-results">
      {#if showSearchResults}
        {#if searchStore.isSearching}
          <!-- Placeholder rows shaped like results keep the palette at a
               result-list size instead of a large empty box. -->
          <div class="palette-loading" role="status" aria-busy="true">
            <span class="palette-loading-label">{m.command_palette_searching()}</span>
            {#each SKELETON_ROWS as row, i (i)}
              <div class="skeleton-row" aria-hidden="true">
                <span class="skeleton-body">
                  <span class="skeleton-bar" style:width="{row.title}%"></span>
                  <span class="skeleton-bar skeleton-snippet" style:width="{row.snippet}%"></span>
                </span>
                <span class="skeleton-bar skeleton-meta"></span>
                <span class="skeleton-bar skeleton-id"></span>
              </div>
            {/each}
          </div>
        {:else if searchStore.error?.kind === "semantic-unavailable"}
          <SemanticSetupHelp
            onResolved={() => searchStore.retry()}
            searchDetail={searchStore.error.detail}
          />
        {:else if searchStore.error}
          <div class="palette-error" role="alert">
            {#if searchStore.error.kind === "timeout"}
              <strong>{m.command_palette_search_timeout_title()}</strong>
              <span>{m.command_palette_search_timeout_detail()}</span>
              <div class="palette-error-action">
                <Button
                  size="sm"
                  tone="info"
                  surface="soft"
                  label={m.shared_retry()}
                  onclick={() => searchStore.retry()}
                />
              </div>
            {:else}
              <strong>{m.command_palette_search_error()}</strong>
              <span>{searchStore.error.detail ?? m.command_palette_search_failed()}</span>
            {/if}
          </div>
        {:else if searchStore.results.length === 0}
          <div class="palette-empty">{m.command_palette_no_results()}</div>
        {:else}
          {#each searchStore.results as result, i}
            <button
              class="palette-item"
              class:selected={i === selectedIndex}
              onclick={() => selectSearchResult(result)}
              onmouseenter={() => (selectedIndex = i)}
            >
              <span class="item-body">
                {#if result.name}
                  <span class="item-name">{truncate(result.name, 60)}</span>
                {/if}
                {#if result.snippet && result.snippet.replace(/<\/?mark>/g, '') !== result.name}
                  <span class="item-snippet">
                    {#if result.snippetFormat !== "plain-text" && /<mark>/i.test(result.snippet)}
                      {@html sanitizeSnippet(result.snippet)}
                    {:else}
                      <!-- Backends without match markers get the query words emphasized here. -->
                      {#each highlightTerms(result.snippet, inputValue) as segment}{#if segment.match}<mark>{segment.text}</mark>{:else}{segment.text}{/if}{/each}
                    {/if}
                  </span>
                {/if}
              </span>
              <span class="item-agent" style:--agent-color={agentColor(result.agent)}>{agentLabel(result.agent)}</span>
              <span class="item-meta">
                {truncate(result.project, 20)}{result.timestamp ? ' · ' + formatRelativeTime(result.timestamp) : ''}
              </span>
              <!-- svelte-ignore a11y_click_events_have_key_events -->
              <!-- svelte-ignore a11y_no_static_element_interactions -->
              <span
                class="item-id"
                title={m.command_palette_copy_session_id()}
                onclick={(e) => {
                  e.stopPropagation();
                  copyToClipboard(result.session_id);
                }}
              >{stripIdPrefix(result.session_id, result.agent).slice(0, 8)}</span>
            </button>
          {/each}
        {/if}
      {:else}
        <div class="palette-section-label">{m.command_palette_recent_sessions()}</div>
        {#each recentSessions as session, i}
          {@const preview = session.display_name ?? normalizeMessagePreview(session.first_message)}
          <button
            class="palette-item"
            class:selected={i === selectedIndex}
            onclick={() => selectSession(session)}
            onmouseenter={() => (selectedIndex = i)}
          >
            <span class="item-body">
              <span class="item-name">{preview
                ? truncate(preview, 60)
                : session.project}</span>
            </span>
            <span class="item-agent" style:--agent-color={agentColor(session.agent)}>{agentLabel(session.agent, session.agent_label)}</span>
            <span class="item-meta">
              {formatRelativeTime(session.ended_at ?? session.started_at)}
            </span>
          </button>
        {/each}
      {/if}
    </div>
  </div>
</div>

<style>
  .palette-overlay {
    position: fixed;
    /* kit-ui-check-ignore: top-aligned command-palette overlay with palette-owned focus handling; kit-ui Modal's centered dialog chrome does not apply — adopting kit-ui CommandPalette wholesale is tracked as a follow-up */
    inset: 0;
    background: var(--overlay-bg);
    display: flex;
    /* The palette sizes to its content (up to its max height) instead of
       stretching, so short states stay short. */
    align-items: flex-start;
    justify-content: center;
    padding-top: 14vh;
    z-index: var(--z-overlay);
  }

  .palette {
    width: min(720px, calc(100vw - 32px));
    max-height: min(560px, 72vh);
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    box-shadow: var(--overlay-shadow);
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .palette-input-wrap {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 10px 14px;
    border-bottom: 1px solid var(--border-default);
  }

  :global(.search-icon) {
    flex-shrink: 0;
    color: var(--text-muted);
  }

  .palette-input {
    flex: 1;
    background: none;
    border: none;
    font-size: 14px;
    color: var(--text-primary);
    outline: none;
  }

  .palette-input::placeholder {
    color: var(--text-muted);
  }

  /* Rows vary in height, so the list cannot always end on a whole row.
     A short fade marks the cut row as "more below"; the matching bottom
     padding lets the real last row scroll fully clear of the fade, and
     scroll-padding keeps keyboard selection out of it. */
  .palette-results {
    --palette-fade: 28px;
    overflow-y: auto;
    flex: 1;
    padding: 4px 0 var(--palette-fade);
    scroll-padding-bottom: var(--palette-fade);
    mask-image: linear-gradient(
      to bottom,
      #000 calc(100% - var(--palette-fade)),
      transparent
    );
  }

  .palette-controls {
    --kit-control-font-size: 12px;
    --typeahead-min-width: 120px;
    --typeahead-max-width: 140px;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 6px 14px;
    border-bottom: 1px solid var(--border-default);
  }

  /* The project filter takes the date filter's field chrome (white
     surface, default edge, primary ink, same height), so the two filters
     read as the same kind of enabled control. kit-ui's grey raised
     trigger made the project filter look unavailable beside it. */
  .palette-controls {
    --typeahead-control-height: 28px;
    --typeahead-control-font-size: var(--font-size-sm);
  }

  .palette-controls :global(.kit-typeahead__trigger:not(:disabled)) {
    background: var(--bg-surface);
    border-radius: var(--radius-md);
    color: var(--text-primary);
  }

  .palette-controls :global(.kit-typeahead__trigger:not(:disabled):not(:focus)) {
    border-color: var(--border-default);
  }

  .palette-controls :global(.kit-typeahead__trigger:not(:disabled):hover) {
    background: var(--bg-surface-hover);
  }

  .palette-section-label {
    padding: 6px 14px 4px;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }

  /* Metadata sits on the title baseline instead of floating between the
     title and the snippet. */
  .palette-item {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    width: 100%;
    padding: 7px 14px;
    text-align: left;
    font-size: 13px;
    color: var(--text-primary);
    transition: background 0.05s;
  }

:global(:root.dark) .item-snippet :global(mark) {
    background: color-mix(in srgb, var(--accent-amber) 35%, transparent);
    color: #fff;
  }

  :global(:root.dark) .palette-item.selected {
    --selected-result-tint: 16%;
  }

  .palette-item:hover {
    background: var(--bg-surface-hover);
  }

  .palette-item.selected {
    background: color-mix(in srgb, var(--accent-blue) var(--selected-result-tint, 9%), var(--bg-surface));
    box-shadow: inset 3px 0 0 var(--accent-blue);
  }

  /* The agent is named in its color, as in the sidebar, instead of an
     unlabeled dot. A touch of the text color keeps it legible. */
  .item-agent {
    flex-shrink: 0;
    padding-top: 2px;
    font-size: 12px;
    font-weight: 500;
    color: color-mix(in srgb, var(--agent-color, var(--text-muted)) 82%, var(--text-primary));
    white-space: nowrap;
  }

  .item-body {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 1px;
  }

  .item-name {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    font-size: 13px;
    font-weight: 500;
    color: var(--text-primary);
  }

  .item-snippet {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    font-size: 12.5px;
    /* Muted so the result titles carry the list; matches stay highlighted. */
    color: var(--text-muted);
  }

  .item-meta {
    font-size: 12px;
    color: var(--text-secondary);
    white-space: nowrap;
    flex-shrink: 0;
    min-width: 108px;
    padding-top: 2px;
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  /* Same padding and gaps as .palette-item, so rows land where results
     will. The label stays for screen readers only; the rows carry the
     state visually. */
  .palette-loading-label {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip-path: inset(50%);
    white-space: nowrap;
  }

  .skeleton-row {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    padding: 9px 14px;
  }

  .skeleton-body {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 7px;
  }

  .skeleton-bar {
    display: block;
    height: 10px;
    border-radius: 4px;
    background: var(--bg-inset);
    animation: palette-skeleton-pulse 1.6s ease-in-out infinite alternate;
  }

  :global(:root.dark) .skeleton-bar {
    background: color-mix(in srgb, var(--text-primary) 8%, var(--bg-surface));
  }

  .skeleton-snippet {
    height: 8px;
  }

  .skeleton-meta {
    flex-shrink: 0;
    width: 108px;
  }

  .skeleton-id {
    flex-shrink: 0;
    width: 56px;
  }

  @keyframes palette-skeleton-pulse {
    from {
      opacity: 1;
    }
    to {
      opacity: 0.45;
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .skeleton-bar {
      animation: none;
    }
  }

  .palette-empty {
    padding: 16px;
    text-align: center;
    color: var(--text-muted);
    font-size: 13px;
  }

  .palette-error {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 16px;
    text-align: center;
    color: var(--text-muted);
    font-size: 13px;
  }

  .palette-error strong {
    color: var(--text-primary);
  }

  .palette-error-action {
    display: flex;
    justify-content: center;
    margin-top: 8px;
  }

  .palette-server-settings {
    font-size: 11px;
    color: var(--text-secondary);
  }

  /* Matches the shared segmented-control look: an inset track with a
     raised selected option. */
  .palette-sort {
    display: flex;
    gap: 2px;
    margin-left: auto;
    padding: 2px;
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .sort-btn {
    padding: 3px 10px;
    font-size: 12px;
    border: none;
    border-radius: var(--radius-sm);
    background: none;
    color: var(--text-muted);
    cursor: pointer;
    font-family: var(--font-sans);
    font-weight: 500;
  }

  .sort-btn:hover:not(.active) {
    color: var(--text-secondary);
  }

  .sort-btn.active {
    background: var(--selected-strong-bg);
    box-shadow: var(--selected-strong-edge);
    color: var(--text-primary);
    font-weight: var(--selected-strong-weight);
  }

  .item-snippet :global(mark) {
    padding: 0 1px;
    border-radius: 2px;
    background: color-mix(in srgb, var(--accent-amber) 26%, transparent);
    color: var(--text-primary);
  }

  .item-id {
    margin-top: 1px;
    width: 72px;
    text-align: right;
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--text-muted);
    white-space: nowrap;
    flex-shrink: 0;
    cursor: pointer;
    padding: 1px 3px;
    border-radius: var(--radius-sm);
  }

  .item-id:hover {
    background: var(--bg-inset);
    color: var(--text-primary);
  }

  /* Phones: the agent, metadata, and ID would leave the title and snippet
     no room, so each result wraps. The title and snippet take the first
     line; the agent and metadata shorten beside the ID on the second. */
  .palette-results {
    container-type: inline-size;
  }

  @container (max-width: 520px) {
    .skeleton-meta {
      width: 64px;
    }

    .skeleton-id {
      display: none;
    }

    .palette-item {
      flex-wrap: wrap;
      row-gap: 2px;
    }

    .item-body {
      flex: 1 0 100%;
    }

    .item-agent {
      flex: 0 1 auto;
      min-width: 0;
      max-width: 40%;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .item-meta {
      flex: 1 1 auto;
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      text-align: left;
    }
  }
</style>
