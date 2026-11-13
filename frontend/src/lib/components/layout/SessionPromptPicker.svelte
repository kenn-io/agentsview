<!-- Jump to any prompt in the session: the header button opens a search
     over every prompt, including ones the transcript has not loaded. -->
<script lang="ts">
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
  // The session `prompts` belong to; another session's must not be picked.
  let promptsFor: string | null = null;

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
      const rows = await readPrompts(id, controller.signal);
      promptsFor = id;
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
  async function readPrompts(id: string, signal: AbortSignal): Promise<Message[]> {
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
          revision ??= page.transcript_revision || undefined;
          rows.push(...page.messages.filter(isPromptMessage));
          const last = page.messages.at(-1);
          if (page.messages.length < PAGE_SIZE || !last) return rows;
          from = last.ordinal + 1;
        }
      } catch (err) {
        if (!(err instanceof ApiError && err.status === 409) || attempt >= MAX_READS) throw err;
      }
    }
  }

  // The transcript's own rule, so a prompt it hides is never offered as a
  // place to jump. A filtered code fence keeps a placeholder, so it counts.
  let shown = $derived(
    ui.hasBlockFilters
      ? prompts.filter((p) =>
          hasVisibleSegments(p.message, (type) => type === "code" || ui.isBlockVisible(type)),
        )
      : prompts,
  );

  let matches = $derived.by(() => {
    const words = query.toLowerCase().split(/\s+/).filter(Boolean);
    return words.length === 0
      ? shown
      : shown.filter((p) => words.every((word) => p.search.includes(word)));
  });

  // The list's header exists only while the list is open.
  const onOpen: Attachment = () => void load();

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
    loading={loading && prompts.length === 0}
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

  .picker-status:not(:empty) {
    padding: 2px 8px 4px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
