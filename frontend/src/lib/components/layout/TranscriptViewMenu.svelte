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

<!-- A schematic of each layout, drawn as vectors so it stays sharp at any
     display scale. Each one shows what sets the layout apart: Default has
     roomy cards with role icons and a tool block, Compact stacks more,
     tighter cards without icons, Stream drops the headers and cards around
     open prose, and Skim reduces every message and tool call to one line. -->
{#snippet preview(layout: MessageLayout)}
  <svg class="preview" viewBox="0 0 48 32" aria-hidden="true">
    {#if layout === "default"}
      <rect class="card" x="2" y="2" width="44" height="10" />
      <rect class="rail-user" x="2" y="2" width="1.5" height="10" />
      <circle class="dot-user" cx="6.5" cy="5" r="1.5" />
      <rect class="head-user" x="9.5" y="4.2" width="9" height="1.6" />
      <rect class="text" x="5" y="8.2" width="34" height="1.6" />
      <rect class="card" x="2" y="14" width="44" height="16" />
      <rect class="rail" x="2" y="14" width="1.5" height="16" />
      <circle class="dot" cx="6.5" cy="17" r="1.5" />
      <rect class="head" x="9.5" y="16.2" width="11" height="1.6" />
      <rect class="text" x="5" y="20.2" width="36" height="1.6" />
      <rect class="tool" x="5" y="23.5" width="38" height="4.5" rx="0.5" />
      <rect class="row" x="7" y="25" width="22" height="1.4" />
    {:else if layout === "compact"}
      {#each [2, 9.5, 17, 24.5] as y, index (y)}
        <rect class="card" x="2" y={y} width="44" height="6" />
        <rect class={index === 0 ? "rail-user" : "rail"} x="2" y={y} width="1" height="6" />
        <rect class={index === 0 ? "head-user" : "head"} x="4" y={y + 1.2} width="6" height="1.1" />
        <rect class={index === 2 ? "row" : "text"} x="4" y={y + 3.5} width={[36, 40, 26, 32][index]} height="1.2" />
      {/each}
    {:else if layout === "stream"}
      <rect class="user-wash" x="2" y="2" width="44" height="8" />
      <rect class="rail-user" x="2" y="2" width="1" height="8" />
      <rect class="text" x="5" y="5.2" width="30" height="1.6" />
      <rect class="text" x="5" y="13.5" width="38" height="1.6" />
      <rect class="text" x="5" y="17.5" width="32" height="1.6" />
      <rect class="row" x="7" y="21.5" width="18" height="1.4" />
      <rect class="text" x="5" y="25.5" width="36" height="1.6" />
    {:else}
      {#each [2.5, 6.5, 10.5, 14.5, 18.5, 22.5, 26.5] as y, index (y)}
        {@const tool = index % 3 === 1}
        <rect class={index === 0 ? "rail-user" : tool ? "row" : "rail"} x="3" y={y} width="1.5" height="3" />
        <rect class={tool ? "row" : "text"} x="6.5" y={y + 0.8} width={[36, 22, 32, 38, 20, 34, 28][index]} height="1.4" />
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
      <!-- The icon, dot, and chevron travel with the visible label, which
           sits at the end of the reserved width: the quiet space falls on
           the title side, and the label stays beside the follow and sort
           controls. -->
      <span class="view-trigger-current">
        <LayoutListIcon size="14" strokeWidth="1.75" aria-hidden="true" />
        <span class="view-trigger-text">{triggerText}</span>
        <!-- Always laid out, so showing it does not change the trigger's
             width. -->
        <span class="view-filter-dot" class:shown={filtered} aria-hidden="true"></span>
        <ChevronDownIcon size="12" strokeWidth="2" aria-hidden="true" />
      </span>
    </span>
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
          <!-- The schematic sits at the right edge, so layout names share one
               text column with the mode and block-visibility labels. -->
          <span class="option">
            <span class="option-text">
              <span class="option-name">{layout.name()}</span>
              <span class="option-description">{layout.description()}</span>
            </span>
            {@render preview(layout.id)}
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

  /* A ghost control: no fill or frame on the button, whose width is
     reserved for the longest label, so it never reads as an empty field.
     Hover and the open menu draw a fill around the visible label instead.
     The selector outranks kit-ui's trigger rules. */
  :global(:root .kit-menu__trigger.view-trigger),
  :global(:root .kit-menu__trigger.view-trigger:hover),
  :global(:root .kit-menu__trigger.view-trigger[aria-expanded="true"]) {
    background: transparent;
    border-color: transparent;
    box-shadow: none;
  }

  :global(.kit-menu__trigger.view-trigger:not(:hover):not([aria-expanded="true"])) {
    color: var(--text-secondary);
  }

  /* Hug the visible label so the fill does not span the reserved width. */
  .view-trigger-current {
    justify-self: end;
    max-width: 100%;
    border-radius: var(--radius-sm);
    transition: background var(--transition-fast, 0.1s), box-shadow var(--transition-fast, 0.1s);
  }

  :global(.kit-menu__trigger.view-trigger:hover) .view-trigger-current,
  :global(.kit-menu__trigger.view-trigger[aria-expanded="true"]) .view-trigger-current {
    background: var(--bg-surface-hover);
    box-shadow:
      0 0 0 4px var(--bg-surface-hover),
      0 0 0 5px var(--border-default);
  }

  /* The chosen layout and content mode are marked by the check and a
     medium-weight name. Rows keep the plain item styling of the Resume and
     More menus, so a row fill only ever means hover or keyboard focus. */
  :global(.view-menu .kit-menu__radio-item[aria-checked="true"]) .option-name {
    font-weight: var(--selected-strong-weight);
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
  .view-trigger-current {
    grid-area: 1 / 1;
    min-width: 0;
  }

  .view-trigger-sizer,
  .view-trigger-text {
    min-width: 0;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    text-align: left;
  }

  /* Each sizer reserves the icon before the label (14px and a 6px gap)
     and the dot and chevron after it: two 6px gaps, the 6px dot, and the
     12px chevron. */
  .view-trigger-sizer {
    visibility: hidden;
    padding-left: 20px;
    padding-right: 30px;
  }

  .view-trigger-current {
    display: flex;
    align-items: center;
    gap: 6px;
  }

  .view-trigger-current > :global(svg:last-child) {
    color: var(--text-muted);
  }

  /* Phones put the trigger in the top bar beside its actions, where the
     reserved label width pushed them off screen. There the trigger shows
     only its icon, filter dot, and chevron; the tooltip and accessible
     name still carry the label. Matches the 760px mobile layout query. */
  @media (max-width: 760px) {
    .view-trigger-sizer,
    .view-trigger-text {
      display: none;
    }
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
     slot, so all three sections share one indent. Every section marks its
     selection with the same check. */
  :global(.view-menu) {
    width: 300px;
    --menu-icon-width: 12px;
  }

  :global(.view-menu .kit-menu__radio-content) {
    flex: 1;
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
    flex: 1;
    align-items: center;
    gap: 10px;
    min-width: 0;
  }

  .option-text {
    flex: 1;
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

  .preview .card { fill: var(--text-primary); opacity: 0.05; }
  .preview .text { fill: var(--text-secondary); opacity: 0.55; }
  .preview .head,
  .preview .dot { fill: var(--accent-purple); opacity: 0.75; }
  .preview .head-user,
  .preview .dot-user { fill: var(--accent-blue); opacity: 0.85; }
  .preview .row { fill: var(--accent-amber); opacity: 0.75; }
  .preview .tool { fill: var(--text-primary); opacity: 0.07; }
  .preview .rail { fill: var(--accent-purple); opacity: 0.6; }
  .preview .rail-user { fill: var(--accent-blue); }
  .preview .user-wash { fill: var(--accent-blue); opacity: 0.12; }
</style>
