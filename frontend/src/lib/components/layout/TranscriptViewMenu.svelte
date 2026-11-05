<script lang="ts">
  import {
    Menu,
    MenuContent,
    MenuItem,
    MenuRadioGroup,
    MenuRadioItem,
    MenuSeparator,
    MenuTrigger,
  } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { CheckIcon, ChevronDownIcon, LayoutListIcon } from "../../icons.js";
  import {
    ALL_BLOCK_TYPES,
    ui,
    type BlockType,
    type MessageLayout,
    type TranscriptMode,
  } from "../../stores/ui.svelte.js";

  // One labeled control for "how much of the transcript do I see": the
  // message layout, the transcript mode, and block visibility. It replaces
  // the unlabeled layout-cycle icon, the Normal/Focused toggle, and the
  // block filter funnel.
  const LAYOUTS: { id: MessageLayout; name: () => string; description: () => string }[] = [
    { id: "default", name: m.appearance_layout_default, description: m.header_view_layout_default_description },
    { id: "compact", name: m.appearance_layout_compact, description: m.header_view_layout_compact_description },
    { id: "stream", name: m.appearance_layout_stream, description: m.header_view_layout_stream_description },
    { id: "skim", name: m.appearance_layout_skim, description: m.header_view_layout_skim_description },
  ];

  const MODES: { id: TranscriptMode; name: () => string; description: () => string }[] = [
    { id: "normal", name: m.header_view_mode_normal, description: m.header_view_mode_normal_description },
    { id: "focused", name: m.header_view_mode_focused, description: m.header_view_mode_focused_description },
  ];

  const BLOCK_LABELS: Record<BlockType, () => string> = {
    user: m.header_transcript_blocks_user,
    assistant: m.header_transcript_blocks_assistant,
    thinking: m.header_transcript_blocks_thinking,
    tool: m.header_transcript_blocks_tool,
    code: m.header_transcript_blocks_code,
    system: m.header_transcript_blocks_system,
  };

  const layoutName = $derived(
    (LAYOUTS.find((layout) => layout.id === ui.messageLayout) ?? LAYOUTS[0]!).name(),
  );
  const filtered = $derived(ui.hasBlockFilters);
  const focused = $derived(ui.transcriptMode === "focused");
  const labelFor = (layout: string, inFocus: boolean) =>
    inFocus ? m.header_view_trigger_focused({ layout }) : m.header_view_trigger({ layout });
  const triggerText = $derived(labelFor(layoutName, focused));
</script>

<!-- A schematic of each layout: role header bars, message blocks, and tool
     rows, drawn as vectors so they stay sharp at any display scale. -->
{#snippet preview(layout: MessageLayout)}
  <svg class="preview" viewBox="0 0 48 32" aria-hidden="true">
    {#if layout === "default"}
      <rect class="rail-user" x="2" y="2" width="1.5" height="9" />
      <rect class="head" x="5" y="3" width="10" height="2" />
      <rect class="text" x="5" y="7" width="38" height="2" />
      <rect class="tool" x="2" y="13" width="44" height="7" rx="1" />
      <rect class="row" x="5" y="15" width="24" height="1.5" />
      <rect class="row" x="5" y="17.5" width="30" height="1.5" />
      <rect class="rail" x="2" y="22" width="1.5" height="8" />
      <rect class="head" x="5" y="23" width="12" height="2" />
      <rect class="text" x="5" y="27" width="34" height="2" />
    {:else if layout === "compact"}
      <rect class="rail-user" x="2" y="2" width="1" height="6" />
      <rect class="head" x="4" y="2.5" width="8" height="1.5" />
      <rect class="text" x="4" y="5.5" width="38" height="1.5" />
      <rect class="tool" x="2" y="9.5" width="44" height="5" rx="1" />
      <rect class="row" x="4" y="11" width="24" height="1.2" />
      <rect class="row" x="4" y="12.8" width="30" height="1.2" />
      <rect class="rail" x="2" y="16" width="1" height="6" />
      <rect class="head" x="4" y="16.5" width="10" height="1.5" />
      <rect class="text" x="4" y="19.5" width="34" height="1.5" />
      <rect class="rail" x="2" y="24" width="1" height="6" />
      <rect class="head" x="4" y="24.5" width="10" height="1.5" />
      <rect class="text" x="4" y="27.5" width="30" height="1.5" />
    {:else if layout === "stream"}
      <rect class="user-wash" x="2" y="2" width="44" height="7" rx="1" />
      <rect class="text" x="5" y="4.5" width="34" height="2" />
      <rect class="text" x="5" y="12" width="38" height="2" />
      <rect class="text" x="5" y="16" width="32" height="2" />
      <rect class="row" x="5" y="20.5" width="22" height="1.5" />
      <rect class="text" x="5" y="25" width="36" height="2" />
    {:else}
      {#each [3, 7, 11, 15, 19, 23, 27] as y, index (y)}
        <rect class={index % 3 === 1 ? "row" : "text"} x="3" y={y} width={[40, 26, 34, 42, 24, 36, 30][index]} height="1.6" />
      {/each}
    {/if}
  </svg>
{/snippet}

<Menu align="end">
  <MenuTrigger
    class={["view-trigger", { filtered }]}
    title={`${triggerText}\n${m.header_view_trigger_title()}`}
    ariaLabel={focused
      ? m.header_view_trigger_label_focused({ layout: layoutName })
      : m.header_view_trigger_label({ layout: layoutName })}
  >
    <LayoutListIcon size="14" strokeWidth="1.75" aria-hidden="true" />
    <!-- Hidden copies of the label for every layout in both modes share
         one grid cell with the visible label, so the trigger is as wide as
         its widest label in the current language. Switching layouts or
         modes does not move its neighbors. -->
    <span class="view-trigger-label-box">
      {#each LAYOUTS as layout (layout.id)}
        {#each [false, true] as inFocus (inFocus)}
          <span class="view-trigger-sizer" aria-hidden="true">{labelFor(layout.name(), inFocus)}</span>
        {/each}
      {/each}
      <span class="view-trigger-text">{triggerText}</span>
    </span>
    <!-- Always laid out, so showing it does not change the trigger's
         width. -->
    <span class="view-filter-dot" class:shown={filtered} aria-hidden="true"></span>
    <ChevronDownIcon size="12" strokeWidth="2" aria-hidden="true" />
  </MenuTrigger>
  <MenuContent ariaLabel={m.header_view_menu_label()} class="view-menu">
    <div class="view-heading" role="presentation">{m.header_view_layout_heading()}</div>
    <MenuRadioGroup
      value={ui.messageLayout}
      onchange={(value) => ui.setLayout(value as MessageLayout)}
      ariaLabel={m.header_view_layout_heading()}
    >
      {#each LAYOUTS as layout (layout.id)}
        <MenuRadioItem value={layout.id} closeOnSelect textValue={layout.name()} class="view-option">
          <span class="option">
            {@render preview(layout.id)}
            <span class="option-text">
              <span class="option-name">{layout.name()}</span>
              <span class="option-description">{layout.description()}</span>
            </span>
          </span>
        </MenuRadioItem>
      {/each}
    </MenuRadioGroup>
    <MenuSeparator />
    <div class="view-heading" role="presentation">{m.header_view_mode_heading()}</div>
    <MenuRadioGroup
      value={ui.transcriptMode}
      onchange={(value) => ui.setTranscriptMode(value as TranscriptMode)}
      ariaLabel={m.header_view_mode_heading()}
    >
      {#each MODES as mode (mode.id)}
        <MenuRadioItem value={mode.id} closeOnSelect textValue={mode.name()} class="view-option">
          <span class="option-text">
            <span class="option-name">{mode.name()}</span>
            <span class="option-description">{mode.description()}</span>
          </span>
        </MenuRadioItem>
      {/each}
    </MenuRadioGroup>
    <MenuSeparator />
    <div class="view-heading" role="presentation">{m.header_transcript_visibility()}</div>
    {#each ALL_BLOCK_TYPES as block (block)}
      <MenuItem closeOnSelect={false} onselect={() => ui.toggleBlock(block)} textValue={BLOCK_LABELS[block]()}>
        {#snippet icon()}
          {#if ui.isBlockVisible(block)}<CheckIcon size="12" strokeWidth="2.2" />{/if}
        {/snippet}
        {BLOCK_LABELS[block]()}
      </MenuItem>
    {/each}
    <MenuSeparator />
    <!-- Always mounted (disabled when nothing is hidden) so keyboard focus
         never lands on an item that disappears under it. -->
    <MenuItem
      closeOnSelect={false}
      disabled={!ui.hasBlockFilters}
      onselect={() => ui.showAllBlocks()}
    >
      {m.header_transcript_show_all()}
    </MenuItem>
  </MenuContent>
</Menu>

<style>
  :global(.view-trigger) {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: 26px;
    padding: 0 9px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-weight: 500;
    color: var(--text-primary);
    white-space: nowrap;
    flex-shrink: 1;
    min-width: 0;
    max-width: 100%;
    margin-right: 4px;
  }

  :global(.view-trigger:hover) {
    background: var(--bg-surface-hover);
  }

  :global(.view-trigger) {
    justify-content: flex-start;
  }

  /* Only a pane too narrow for the full label shrinks the trigger; the
     label then ends in an ellipsis, and the tooltip and accessible name
     still carry it in full. */
  .view-trigger-label-box {
    display: grid;
    flex: 1;
    min-width: 0;
  }

  .view-trigger-sizer,
  .view-trigger-text {
    grid-area: 1 / 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    text-align: left;
  }

  .view-trigger-sizer {
    visibility: hidden;
  }

  :global(.view-trigger) :global(svg) {
    flex-shrink: 0;
  }

  /* Hidden block types get a small accent dot; amber is avoided because it
     marks tool calls in the transcript. */
  .view-filter-dot {
    width: 6px;
    height: 6px;
    flex-shrink: 0;
    border-radius: 50%;
    background: transparent;
  }

  .view-filter-dot.shown {
    background: var(--accent-blue);
  }

  /* The block toggles' icon slot matches the radio items' 12px check
     slot, so all three sections share one indent. */
  :global(.view-menu) {
    width: 300px;
    --menu-icon-width: 12px;
  }

  .view-heading {
    padding: 6px 10px 2px;
    font-size: var(--font-size-2xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
  }

  .option {
    display: flex;
    align-items: center;
    gap: 10px;
  }

  .option-text {
    display: flex;
    flex-direction: column;
    min-width: 0;
    white-space: normal;
  }

  .option-name {
    font-size: 12.5px;
    color: var(--text-primary);
  }

  .option-description {
    font-size: 11.5px;
    line-height: 1.35;
    color: var(--text-muted);
  }

  .preview {
    width: 48px;
    height: 32px;
    flex-shrink: 0;
    border: 1px solid var(--border-muted);
    border-radius: 3px;
    background: var(--bg-primary);
  }

  .preview .text { fill: var(--text-secondary); opacity: 0.55; }
  .preview .head { fill: var(--accent-purple); opacity: 0.7; }
  .preview .row { fill: var(--accent-amber); opacity: 0.7; }
  .preview .tool { fill: var(--text-primary); opacity: 0.06; }
  .preview .rail { fill: var(--accent-purple); opacity: 0.6; }
  .preview .rail-user { fill: var(--accent-blue); }
  .preview .user-wash { fill: var(--accent-blue); opacity: 0.15; }
</style>
