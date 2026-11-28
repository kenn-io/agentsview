<!-- Jump to any prompt in the session: the header button opens a search
     over every prompt, including ones the transcript has not loaded. -->
<script lang="ts">
  import { untrack } from "svelte";
  import type { Attachment } from "svelte/attachments";
  import { IconButton, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import { SessionsService, type DbMessage as Message } from "../../api/generated/index";
  import { ApiError, isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import { TableOfContentsIcon } from "../../icons.js";
  import { messages } from "../../stores/messages.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { hasVisibleSegments } from "../../utils/content-parser.js";
  import { formatNumber, formatTimestamp } from "../../utils/format.js";
  import { isPromptMessage, previewMessage } from "../../utils/messages.js";
  import { setPromptPickerTrigger } from "./prompt-picker.js";

  const PAGE_SIZE = 1000;
  // Typeahead renders every option it gets, so a session with thousands
  // of prompts shows the newest matches and asks for more letters.
  const SHOWN = 100;
  // A sync that lands between pages restarts the read this many times at most.
  const MAX_READS = 3;

  interface Prompt {
    message: Message;
    option: TypeaheadOption;
    search: string;
  }

  let prompts: Prompt[] = $state.raw([]);
  let query = $state("");
  let loading = $state(false);
  let failed = $state(false);
  let read: AbortController | null = null;
  // The session and transcript revision `prompts` were read from. Their
  // ordinals are only safe to jump to while the transcript shows that same
  // session at that same revision. Revisions count per session, so two
  // sessions can share one.
  let promptsFor: string | null = $state(null);
  let promptsRevision: string | null = $state(null);

  // Every prompt, newest first, read again on each opening so a live
  // session's new prompts show up.
  async function load() {
    const id = messages.sessionId;
    if (!id) return;
    read?.abort();
    const controller = new AbortController();
    read = controller;
    if (promptsFor !== id) prompts = [];
    loading = true;
    failed = false;
    try {
      const { rows, revision } = await readPrompts(id, controller.signal);
      if (messages.sessionId !== id) return;
      promptsFor = id;
      promptsRevision = revision;
      prompts = rows
        .map((row, index) => {
          const text = previewMessage(row.content).text.trim();
          return {
            message: row,
            option: {
              name: String(row.ordinal),
              label: text.split("\n", 1)[0] || m.session_toc_empty_prompt(),
              meta: `#${formatNumber(index + 1)} · ${formatTimestamp(row.timestamp)}`,
            },
            search: text.replace(/\s+/g, " ").toLowerCase(),
          };
        })
        .reverse();
    } catch (err) {
      if (!isAbortError(err)) failed = true;
    } finally {
      if (read === controller) {
        read = null;
        loading = false;
      }
    }
  }

  // Every page names the first page's revision, so a sync between pages
  // answers 409 and the read starts over rather than mixing two transcripts.
  async function readPrompts(
    id: string,
    signal: AbortSignal,
  ): Promise<{ rows: Message[]; revision: string }> {
    for (let attempt = 1; ; attempt++) {
      const rows: Message[] = [];
      let revision: string | undefined;
      try {
        for (let from = 0; ; ) {
          const page = await SessionsService.getApiV1SessionsByIdMessages(
            { id },
            {
              roles: "user",
              direction: "asc",
              from,
              limit: PAGE_SIZE,
              ...(revision ? { expected_revision: revision } : {}),
            },
            { signal },
          );
          // The same value MessagesStore records as its loadedRevision.
          revision ??= page.transcript_revision ?? "";
          rows.push(...page.messages.filter(isPromptMessage));
          const last = page.messages.at(-1);
          if (page.messages.length < PAGE_SIZE || !last) return { rows, revision };
          from = last.ordinal + 1;
        }
      } catch (err) {
        if (!(err instanceof ApiError && err.status === 409) || attempt >= MAX_READS) throw err;
      }
    }
  }

  // A resync can renumber messages, so prompts read from a revision other
  // than the transcript's wait until the two agree: a cached list once the
  // transcript moves on, or a fresh read the transcript has not caught up
  // with yet.
  let current = $derived(
    promptsFor === messages.sessionId && promptsRevision === messages.loadedRevision,
  );

  // The transcript's own rule, so a prompt it hides is never offered as a
  // place to jump. A filtered code fence keeps a placeholder, so it counts.
  // A prompt newer than the transcript's newest row has no card to scroll
  // to, and loading a missing message only reaches back for older ones.
  // It shows up here once the transcript's own refresh brings it in.
  let shown = $derived.by(() => {
    if (!current) return [];
    const newest = messages.messages.at(-1)?.ordinal ?? -1;
    return prompts.filter(
      (p) =>
        p.message.ordinal <= newest &&
        (!ui.hasBlockFilters ||
          hasVisibleSegments(p.message, (type) => type === "code" || ui.isBlockVisible(type))),
    );
  });

  let matches = $derived.by(() => {
    const words = query.toLowerCase().split(/\s+/).filter(Boolean);
    return words.length === 0
      ? shown
      : shown.filter((p) => words.every((word) => p.search.includes(word)));
  });

  // Whether the list is open, and the transcript it last read for.
  let open = $state(false);
  let watchedSession: string | null = null;
  let watchedRevision: string | null = null;

  // The list's header exists only while the list is open.
  const onOpen: Attachment = () => {
    untrack(() => {
      open = true;
      watchedSession = messages.sessionId;
      watchedRevision = messages.loadedRevision;
      void load();
    });
    return () => {
      open = false;
    };
  };

  // Every new message raises the revision, and navigating can switch the
  // session under an open list, so an open list would otherwise stay
  // withdrawn until reopened. A read that is already on the new revision,
  // because it got ahead of the transcript, needs no second read.
  $effect(() => {
    const session = messages.sessionId;
    const revision = messages.loadedRevision;
    if (!open || (session === watchedSession && revision === watchedRevision)) return;
    watchedSession = session;
    watchedRevision = revision;
    untrack(() => {
      if (session !== promptsFor || revision !== promptsRevision) void load();
    });
  });

  const bindTrigger: Attachment<HTMLButtonElement> = (node) => {
    setPromptPickerTrigger(node);
    return () => setPromptPickerTrigger(undefined);
  };
</script>

<span class="prompt-picker">
  <Typeahead
    remote
    options={matches.slice(0, SHOWN).map((p) => p.option)}
    placeholder={m.session_toc_search()}
    fallbackLabel={m.session_toc_search()}
    emptyLabel={query.trim() ? m.session_toc_no_match() : m.session_toc_empty()}
    loading={!failed && (loading || !current) && shown.length === 0}
    loadingLabel={m.session_toc_loading()}
    error={failed ? m.session_toc_load_failed() : undefined}
    placement="bottom"
    onquery={(next) => (query = next)}
    onselect={(name) => ui.scrollToOrdinal(Number(name))}
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
      <div class="picker-status" {@attach onOpen}>
        {#if matches.length > SHOWN}
          {m.session_toc_more({ shown: formatNumber(SHOWN), total: formatNumber(matches.length) })}
        {/if}
      </div>
    {/snippet}
  </Typeahead>
</span>

<style>
  /* Wide enough to read a prompt's first line. */
  .prompt-picker {
    display: contents;
    --typeahead-panel-min-width: 640px;
  }

  /* The phone header is one row with no room left for another button. */
  @media (max-width: 640px) {
    .prompt-picker {
      display: none;
    }
  }

  .picker-status:not(:empty) {
    padding: 2px 8px 4px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
