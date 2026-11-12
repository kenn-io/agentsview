<script lang="ts">
  import {
    CheckIcon,
    ChevronDownIcon,
    CirclePlayIcon,
    CodeIcon,
    CopyIcon,
    FileTextIcon,
    FolderIcon,
    SquareTerminalIcon,
    TriangleAlertIcon,
  } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import type { SessionOpeners } from "./sessionOpeners.svelte.js";

  // The session header's Resume/Open trigger and its menu. The header owns
  // the state, closes the menu on Escape or an outside click, and handles
  // the number-key shortcuts.
  interface Props {
    openers: SessionOpeners;
  }

  let { openers }: Props = $props();
</script>

{#if openers.showDropdown}
  <span class="open-group">
    <button
      class="resume-btn"
      class:has-feedback-success={openers.feedback !== "" && openers.feedbackKind === "success"}
      class:has-feedback-error={openers.feedback !== "" && openers.feedbackKind === "error"}
      onclick={(e) => { e.stopPropagation(); openers.menuOpen = !openers.menuOpen; }}
      title={openers.canLaunch
        ? m.session_breadcrumb_resume_session_in_terminal()
        : m.session_breadcrumb_session_actions()}
      aria-label={openers.canLaunch
        ? m.session_breadcrumb_resume_session()
        : m.session_breadcrumb_session_actions()}
    >
      {#if openers.feedback}
        {#if openers.feedbackKind === "error"}
          <TriangleAlertIcon size="12" aria-hidden="true" />
        {:else}
          <CheckIcon size="12" aria-hidden="true" />
        {/if}
        {openers.feedback}
      {:else}
        {openers.canLaunch
          ? m.session_breadcrumb_resume()
          : m.session_breadcrumb_open()}
        <ChevronDownIcon size="10" aria-hidden="true" />
      {/if}
    </button>
    {#if openers.menuOpen}
      <div class="open-menu">
        {#if openers.canLaunch}
          {#each openers.terminalOpeners as opener, i (opener.id)}
            <button
              class="open-menu-item"
              onclick={() => openers.resumeIn(opener)}
            >
              <span class="open-menu-num">{i + 1}</span>
              <span class="open-menu-name">{opener.name}</span>
            </button>
          {/each}
          <button class="open-menu-item" onclick={() => openers.resumeDefault()}>
            <span class="open-menu-num">
              <SquareTerminalIcon size="14" aria-hidden="true" />
            </span>
            <span class="open-menu-name">{m.session_breadcrumb_default_terminal()}</span>
          </button>
          {#if openers.codexLink}
            <div class="open-menu-divider"></div>
            <a
              class="open-menu-item"
              data-testid="codex-desktop-link"
              href={openers.codexLink}
              title={m.session_breadcrumb_open_in_codex_desktop()}
            >
              <span class="open-menu-num">
                <CirclePlayIcon size="14" aria-hidden="true" />
              </span>
              <span class="open-menu-name">{m.session_breadcrumb_open_in_codex_desktop()}</span>
            </a>
          {/if}
          {#if openers.claudeLink}
            <div class="open-menu-divider"></div>
            <a
              class="open-menu-item"
              data-testid="claude-code-link"
              href={openers.claudeLink}
              title={m.session_breadcrumb_open_in_claude_code()}
            >
              <span class="open-menu-num">
                <CirclePlayIcon size="14" aria-hidden="true" />
              </span>
              <span class="open-menu-name">{m.session_breadcrumb_open_in_claude_code()}</span>
            </a>
          {/if}
        {/if}
        {#if openers.canCopyCommand}
          {#if openers.canLaunch}<div class="open-menu-divider"></div>{/if}
          <button class="open-menu-item" onclick={() => openers.copyResumeCommand()}>
            <span class="open-menu-num">
              <CopyIcon size="14" aria-hidden="true" />
            </span>
            <span class="open-menu-name">{m.session_breadcrumb_copy_command()}</span>
          </button>
        {/if}
        {#if openers.isLocal}
        <button class="open-menu-item" onclick={() => openers.copyFilePath()}>
          <span class="open-menu-num">
            <FileTextIcon size="14" aria-hidden="true" />
          </span>
          <span class="open-menu-name">{m.session_breadcrumb_copy_directory_path()}</span>
        </button>
        {#if openers.editorOpeners.length > 0 || openers.fileOpeners.length > 0}
          <div class="open-menu-divider"></div>
          <div class="open-menu-section">{m.session_breadcrumb_open_in()}</div>
          {#each openers.editorOpeners as opener (opener.id)}
            <button
              class="open-menu-item"
              onclick={() => openers.openIn(opener)}
            >
              <span class="open-menu-num">
                <CodeIcon size="14" aria-hidden="true" />
              </span>
              <span class="open-menu-name">{opener.name}</span>
            </button>
          {/each}
          {#each openers.fileOpeners as opener (opener.id)}
            <button
              class="open-menu-item"
              onclick={() => openers.openIn(opener)}
            >
              <span class="open-menu-num">
                <FolderIcon size="14" aria-hidden="true" />
              </span>
              <span class="open-menu-name">{opener.name}</span>
            </button>
          {/each}
        {/if}
        {/if}
        {#if openers.canLaunch && openers.claudeDesktopOpener}
          {@const claudeDesktop = openers.claudeDesktopOpener}
          <div class="open-menu-divider"></div>
          <button
            class="open-menu-item"
            onclick={() => openers.resumeIn(claudeDesktop)}
          >
            <span class="open-menu-num">
              <CirclePlayIcon size="14" aria-hidden="true" />
            </span>
            <span class="open-menu-name">Claude Desktop</span>
          </button>
        {/if}
      </div>
    {/if}
  </span>
{/if}

<style>
  .open-group {
    position: relative;
    display: flex;
    align-items: center;
    flex-shrink: 0;
  }

  .resume-btn {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    padding: 1px 8px;
    border-radius: 4px;
    background: var(--bg-tertiary);
    cursor: pointer;
    white-space: nowrap;
    flex-shrink: 0;
    transition: color 0.15s, background 0.15s;
  }

  .resume-btn:hover {
    color: var(--text-secondary);
    background: var(--bg-surface-hover);
  }

  .resume-btn.has-feedback-success {
    color: var(--accent-green, #2ea043);
  }

  .resume-btn.has-feedback-error {
    color: var(--accent-red, #e55);
  }

  .open-menu {
    position: absolute;
    top: 100%;
    right: 0;
    margin-top: 4px;
    /* Same fill, edge, radius and shadow as the View and More actions
       popovers. */
    background: var(--bg-popover);
    border: 1px solid var(--border-popover);
    border-radius: var(--radius-md);
    padding: 4px;
    min-width: 200px;
    z-index: var(--z-popover);
    box-shadow: var(--shadow-lg);
  }

  .open-menu-item {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    width: 100%;
    padding: 6px 10px;
    font-size: 12.5px;
    text-align: left;
    color: var(--text-primary);
    text-decoration: none;
    border-radius: 5px;
    cursor: pointer;
    transition: background 0.1s;
  }

  .open-menu-item:hover {
    background: var(--bg-surface-hover);
  }

  .open-menu-num {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 18px;
    font-size: 11px;
    font-weight: 500;
    color: var(--text-secondary);
    flex-shrink: 0;
  }

  .open-menu-name {
    flex: 1;
    font-weight: 400;
  }

  .open-menu-divider {
    height: 1px;
    background: var(--border-muted);
    margin: 4px 0;
  }

  .open-menu-section {
    padding: 4px 10px 2px;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
</style>
