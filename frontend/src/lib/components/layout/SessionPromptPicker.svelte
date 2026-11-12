<!-- Jump to any prompt in the session. The header button opens a search
     over the session's prompts, including prompts older than the loaded
     part of a large session; the message list loads them on the way. -->
<script lang="ts">
  import { untrack } from "svelte";
  import type { Attachment } from "svelte/attachments";
  import { IconButton, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { isComposingKey } from "../../search/find-input.js";
  import { TableOfContentsIcon } from "../../icons.js";
  import { messages } from "../../stores/messages.svelte.js";
  import { sessionToc } from "../../stores/sessionToc.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { formatNumber, formatTimestamp } from "../../utils/format.js";
  import {
    currentEntryIndex,
    entryVisible,
    mergeTocEntries,
    pickPrompts,
    PICKER_LIMIT,
  } from "../content/session-toc.js";

  let query = $state("");
  // True while the list is open; prompts are gathered only then.
  let active = $state(false);
  let triggerEl: HTMLButtonElement | undefined;

  let oldestLoaded = $derived(messages.messages[0]?.ordinal ?? 0);
  let needsOlder = $derived(messages.hasOlder && oldestLoaded > 0);
  let complete = $derived(
    !needsOlder || (messages.sessionId !== null && sessionToc.covers(messages.sessionId, oldestLoaded)),
  );
  // Every prompt, oldest first. Numbers come from this list, so a prompt
  // keeps its number when the block filter hides others.
  let allEntries = $derived(
    active ? mergeTocEntries(sessionToc.olderFor(messages.sessionId), messages.messages) : [],
  );
  let numbers = $derived(new Map(allEntries.map((entry, index) => [entry.ordinal, index + 1])));
  // The prompts the transcript shows under the block filter.
  let entries = $derived(allEntries.filter((entry) => entryVisible(entry, (type) => ui.isBlockVisible(type))));
  let currentIndex = $derived(currentEntryIndex(entries, sessionToc.view, sessionToc.picked));
  let pick = $derived(pickPrompts(entries, query, currentIndex));
  let olderLoading = $derived(needsOlder && sessionToc.olderLoading);
  let olderFailed = $derived(needsOlder && sessionToc.olderFailed);

  let options: TypeaheadOption[] = $derived(
    pick.shown.map((entry) => {
      const time = formatTimestamp(entry.timestamp);
      return {
        name: String(entry.ordinal),
        label: entry.label || m.session_toc_empty_prompt(),
        // Numbers are known only once every earlier prompt is.
        meta: complete ? `#${formatNumber(numbers.get(entry.ordinal) ?? 0)} · ${time}` : time,
      };
    }),
  );
  let value = $derived(currentIndex >= 0 ? String(entries[currentIndex]!.ordinal) : "");

  let status = $derived.by(() => {
    // Counts and positions are wrong until every earlier prompt is known;
    // a failed read shows its error instead.
    if (!complete) return olderFailed ? "" : m.session_toc_loading_older();
    const total = entries.length;
    if (query.trim() !== "") {
      return pick.matched > PICKER_LIMIT
        ? m.session_toc_matches_capped({
            matched: formatNumber(pick.matched),
            limit: formatNumber(PICKER_LIMIT),
          })
        : m.session_toc_matches({ matched: formatNumber(pick.matched), total: formatNumber(total) });
    }
    if (total > PICKER_LIMIT) {
      return m.session_toc_window({
        from: formatNumber(pick.start + 1),
        to: formatNumber(pick.start + pick.shown.length),
        total: formatNumber(total),
      });
    }
    return m.session_toc_count({ count: total, countLabel: formatNumber(total) });
  });

  let emptyLabel = $derived(
    query.trim() !== ""
      ? m.session_toc_no_match()
      : allEntries.length > 0
        ? m.session_toc_hidden_by_filter()
        : m.session_toc_empty(),
  );

  $effect(() => {
    const sessionId = messages.sessionId;
    const before = oldestLoaded;
    // A finished read may cover less than a newer window needs, so this
    // runs again when one ends.
    void sessionToc.olderLoading;
    if (!active || !sessionId || messages.loading || !needsOlder) return;
    untrack(() => void sessionToc.ensureOlder(sessionId, before));
  });

  // The list's header exists only while the list is open, so it marks the
  // opening and closing. Each opening reads earlier prompts again, which
  // also retries a read that failed.
  const trackOpen: Attachment = () => {
    untrack(() => {
      sessionToc.refreshOlder();
      active = true;
    });
    return () => {
      active = false;
    };
  };

  // Typeahead opens a search for any letter typed on its button, so `t`
  // there would search for "t". Open it empty, as `t` does elsewhere. A
  // capture listener runs before Typeahead's own.
  const bindTrigger: Attachment<HTMLButtonElement> = (node) => {
    triggerEl = node;
    const keydown = (event: KeyboardEvent) => {
      if (event.key !== "t" || event.ctrlKey || event.metaKey || event.altKey) return;
      if (node.getAttribute("aria-expanded") === "true") return;
      event.preventDefault();
      event.stopImmediatePropagation();
      node.click();
    };
    node.addEventListener("keydown", keydown, { capture: true });
    return () => {
      node.removeEventListener("keydown", keydown, { capture: true });
      if (triggerEl === node) triggerEl = undefined;
    };
  };

  // Typeahead does not scroll its list to follow the highlighted row, and
  // the list holds up to 100 rows, so keep the row it names in view.
  const revealHighlighted: Attachment<HTMLElement> = (node) => {
    const observer = new MutationObserver(() => {
      const input = node.querySelector("input[aria-activedescendant]");
      const id = input?.getAttribute("aria-activedescendant");
      if (id) document.getElementById(id)?.scrollIntoView?.({ block: "nearest" });
    });
    observer.observe(node, {
      subtree: true,
      childList: true,
      attributes: true,
      attributeFilter: ["aria-activedescendant"],
    });
    return () => observer.disconnect();
  };

  // Typeahead acts on Enter and Escape even while an input method is
  // composing, which would jump or close instead of confirming the text.
  function guardComposition(event: KeyboardEvent) {
    if ((event.key === "Enter" || event.key === "Escape") && isComposingKey(event)) {
      event.stopPropagation();
    }
  }

  // The `t` shortcut opens the list the way a click on its button does.
  let handledOpenRequest = untrack(() => sessionToc.openRequest);
  $effect(() => {
    const request = sessionToc.openRequest;
    if (request === handledOpenRequest) return;
    handledOpenRequest = request;
    if (triggerEl?.getAttribute("aria-expanded") !== "true") triggerEl?.click();
  });

  function jump(name: string) {
    const ordinal = Number(name);
    sessionToc.picked = ordinal;
    ui.scrollToOrdinal(ordinal);
  }
</script>

<span class="prompt-picker" onkeydowncapture={guardComposition} {@attach revealHighlighted}>
  <Typeahead
    remote
    {options}
    {value}
    placeholder={m.session_toc_search()}
    fallbackLabel={m.session_toc_search()}
    {emptyLabel}
    loading={olderLoading && entries.length === 0}
    loadingLabel={m.session_toc_loading_older()}
    error={olderFailed ? m.session_toc_load_failed() : undefined}
    placement="bottom"
    onquery={(next) => (query = next)}
    onselect={jump}
  >
    {#snippet trigger({ attachment })}
      <IconButton
        ariaLabel={m.session_toc_toggle()}
        title={m.session_toc_toggle_shortcut()}
        ariaKeyshortcuts="T"
        {@attach attachment}
        {@attach bindTrigger}
      >
        <TableOfContentsIcon size="14" aria-hidden="true" />
      </IconButton>
    {/snippet}
    {#snippet header()}
      <div class="picker-status" {@attach trackOpen}>
        {#if status}<span role="status">{status}</span>{/if}
      </div>
    {/snippet}
  </Typeahead>
</span>

<style>
  /* Wide enough to read a prompt's first line; the list still stays
     inside the window on narrow panes. */
  .prompt-picker {
    display: contents;
    --typeahead-panel-min-width: 640px;
  }

  .picker-status {
    display: flex;
    justify-content: space-between;
    gap: 8px;
    padding: 2px 8px 4px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
