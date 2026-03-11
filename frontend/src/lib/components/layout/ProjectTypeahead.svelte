<script lang="ts">
  import { tick } from "svelte";
  import type { ProjectInfo } from "../../api/types/core.js";

  interface Props {
    projects: ProjectInfo[];
    value: string;
    onselect: (value: string) => void;
  }

  let { projects, value, onselect }: Props = $props();

  let query = $state("");
  let open = $state(false);
  let highlightIndex = $state(0);
  let inputEl = $state<HTMLInputElement>();
  let containerEl = $state<HTMLDivElement>();

  const allOption = { name: "", label: "All Projects", count: 0 };

  const options = $derived.by(() => {
    const items = projects.map((p) => ({
      name: p.name,
      label: `${p.name} (${p.session_count})`,
      count: p.session_count,
    }));
    return [allOption, ...items];
  });

  const filtered = $derived.by(() => {
    if (!query) return options;
    const q = query.toLowerCase();
    return options.filter((o) => o.label.toLowerCase().includes(q));
  });

  const displayValue = $derived(
    value ? projects.find((p) => p.name === value)?.name ?? value : "All Projects",
  );

  async function openDropdown() {
    query = "";
    open = true;
    highlightIndex = 0;
    // Wait for Svelte to commit the DOM update before focusing.
    // requestAnimationFrame alone can race with reactive updates
    // that destroy and recreate the input element.
    await tick();
    inputEl?.focus();
  }

  function closeDropdown() {
    open = false;
    query = "";
  }

  function select(name: string) {
    onselect(name);
    closeDropdown();
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      highlightIndex = Math.min(highlightIndex + 1, filtered.length - 1);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      highlightIndex = Math.max(highlightIndex - 1, 0);
    } else if (e.key === "Enter") {
      e.preventDefault();
      const item = filtered[highlightIndex];
      if (item) {
        select(item.name);
      }
    } else if (e.key === "Escape") {
      closeDropdown();
    }
  }

  function handleInput() {
    highlightIndex = 0;
  }

  function highlightSegments(label: string, q: string): { text: string; match: boolean }[] {
    if (!q) return [{ text: label, match: false }];
    const idx = label.toLowerCase().indexOf(q.toLowerCase());
    if (idx === -1) return [{ text: label, match: false }];
    return [
      ...(idx > 0 ? [{ text: label.slice(0, idx), match: false }] : []),
      { text: label.slice(idx, idx + q.length), match: true },
      ...(idx + q.length < label.length ? [{ text: label.slice(idx + q.length), match: false }] : []),
    ];
  }

  function handleBlur(e: FocusEvent) {
    const related = e.relatedTarget as Node | null;
    if (containerEl && related && containerEl.contains(related)) return;
    closeDropdown();
  }

  function preventBlur(e: MouseEvent) {
    // Prevent mousedown on the list from stealing focus from the
    // input. This keeps blur firing correctly for outside clicks
    // while allowing scrollbar and option interactions.
    e.preventDefault();
  }
</script>

<div class="typeahead" bind:this={containerEl}>
  {#if open}
    <input
      bind:this={inputEl}
      class="typeahead-input"
      type="text"
      bind:value={query}
      oninput={handleInput}
      onkeydown={handleKeydown}
      onblur={handleBlur}
      placeholder="Filter projects..."
      aria-label="Filter projects"
      autocomplete="off"
    />
    <ul class="typeahead-list" role="listbox" onmousedown={preventBlur}>
      {#each filtered as option, i}
        <li
          class="typeahead-option"
          class:highlighted={i === highlightIndex}
          class:selected={option.name === value}
          role="option"
          aria-selected={option.name === value}
          onmousedown={() => select(option.name)}
          onmouseenter={() => (highlightIndex = i)}
        >
          {#each highlightSegments(option.label, query) as seg}{#if seg.match}<mark class="match">{seg.text}</mark>{:else}{seg.text}{/if}{/each}
        </li>
      {:else}
        <li class="typeahead-empty">No matching projects</li>
      {/each}
    </ul>
  {:else}
    <button class="typeahead-trigger" onclick={openDropdown} title="Select project">
      <span class="typeahead-value">{displayValue}</span>
      <svg class="typeahead-chevron" width="10" height="10" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
        <path d="M1.646 4.646a.5.5 0 01.708 0L8 10.293l5.646-5.647a.5.5 0 01.708.708l-6 6a.5.5 0 01-.708 0l-6-6a.5.5 0 010-.708z"/>
      </svg>
    </button>
  {/if}
</div>

<style>
  .typeahead {
    position: relative;
    min-width: 180px;
    max-width: 300px;
  }

  .typeahead-trigger {
    height: 26px;
    width: 100%;
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 0 8px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--text-secondary);
    cursor: pointer;
    transition: border-color 0.15s;
    text-align: left;
  }

  .typeahead-trigger:hover {
    border-color: var(--border-default);
  }

  .typeahead-trigger:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .typeahead-value {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .typeahead-chevron {
    flex-shrink: 0;
    opacity: 0.5;
  }

  .typeahead-input {
    height: 26px;
    width: 100%;
    padding: 0 8px;
    background: var(--bg-inset);
    border: 1px solid var(--accent-blue);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--text-primary);
    outline: none;
    box-sizing: border-box;
  }

  .typeahead-input::placeholder {
    color: var(--text-muted);
  }

  .typeahead-list {
    position: absolute;
    top: 100%;
    left: 0;
    right: 0;
    margin-top: 2px;
    max-height: 50vh;
    overflow-y: auto;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    box-shadow: var(--shadow-md, 0 4px 12px rgba(0, 0, 0, 0.15));
    z-index: 100;
    list-style: none;
    padding: 2px;
  }

  .typeahead-option {
    padding: 4px 8px;
    font-size: 11px;
    color: var(--text-secondary);
    cursor: pointer;
    border-radius: 3px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .typeahead-option.highlighted {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .typeahead-option.selected {
    color: var(--accent-blue);
    font-weight: 600;
  }

  .match {
    background: color-mix(in srgb, var(--accent-blue) 40%, transparent);
    color: var(--accent-blue);
    font-weight: 600;
    border-radius: 1px;
  }

  .typeahead-empty {
    padding: 6px 8px;
    font-size: 11px;
    color: var(--text-muted);
    font-style: italic;
  }
</style>
