<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { XIcon } from "../../icons.js";

  function handleOverlayClick(e: MouseEvent) {
    if (
      (e.target as HTMLElement).classList.contains(
        "about-overlay",
      )
    ) {
      ui.activeModal = null;
    }
  }

  const buildDate = $derived.by(() => {
    const raw = sync.serverVersion?.build_date;
    if (!raw) return null;
    try {
      return new Date(raw).toLocaleDateString(undefined, {
        year: "numeric",
        month: "long",
        day: "numeric",
        timeZone: "UTC",
      });
    } catch {
      return raw;
    }
  });
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="about-overlay"
  onclick={handleOverlayClick}
  onkeydown={(e) => {
    if (e.key === "Escape") ui.activeModal = null;
  }}
>
  <div class="about-modal">
    <div class="about-header">
      <svg class="about-logo" width="40" height="40" viewBox="0 0 32 32" aria-hidden="true">
        <rect width="32" height="32" rx="6" fill="var(--accent-blue, #3b82f6)"/>
        <rect x="13" y="10" width="6" height="16" rx="2" fill="var(--bg-surface, #fff)"/>
        <rect x="11" y="5" width="10" height="7" rx="2" fill="var(--bg-surface, #fff)"/>
        <circle cx="18" cy="8.5" r="2" fill="var(--accent-blue, #3b82f6)"/>
        <circle cx="18" cy="8.5" r="1" fill="#1d4ed8"/>
      </svg>
      <div class="about-name">AgentsView</div>
      <button
        class="close-btn"
        onclick={() => ui.activeModal = null}
        title={m.about_close()}
        aria-label={m.about_close()}
      >
        <XIcon size="14" strokeWidth="2.2" aria-hidden="true" />
      </button>
    </div>

    <div class="about-body">
      <div class="about-row">
        <span class="about-label">{m.about_author()}</span>
        <span class="about-value">Kenn Software LLC</span>
      </div>
      {#if sync.serverVersion}
        <div class="about-row">
          <span class="about-label">{m.about_version()}</span>
          <span class="about-value mono">
            {sync.serverVersion.version}
          </span>
        </div>
        <div class="about-row">
          <span class="about-label">{m.about_commit()}</span>
          <span class="about-value mono">
            {sync.serverVersion.commit}
          </span>
        </div>
        {#if buildDate}
          <div class="about-row">
            <span class="about-label">{m.about_build_date()}</span>
            <span class="about-value">{buildDate}</span>
          </div>
        {/if}
      {/if}
    </div>

    <div class="about-footer">
      {m.about_footer()}
    </div>
  </div>
</div>

<style>
  .about-overlay {
    position: fixed;
    inset: 0;
    background: var(--overlay-bg);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 100;
  }

  .about-modal {
    width: 320px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    box-shadow: var(--shadow-md);
    overflow: hidden;
  }

  .about-header {
    display: flex;
    flex-direction: column;
    align-items: center;
    padding: 20px 16px 12px;
    position: relative;
  }

  .about-logo {
    margin-bottom: 8px;
  }

  .about-name {
    font-size: 15px;
    font-weight: 650;
    color: var(--text-primary);
    letter-spacing: -0.01em;
  }

  .close-btn {
    position: absolute;
    top: 10px;
    right: 10px;
    width: 24px;
    height: 24px;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 16px;
    color: var(--text-muted);
    border-radius: var(--radius-sm);
  }

  .close-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .about-body {
    padding: 8px 20px 12px;
  }

  .about-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 4px 0;
  }

  .about-label {
    font-size: 12px;
    color: var(--text-muted);
  }

  .about-value {
    font-size: 12px;
    color: var(--text-secondary);
  }

  .about-value.mono {
    font-family: var(--font-mono);
    font-size: 11px;
  }

  .about-footer {
    padding: 10px 20px;
    border-top: 1px solid var(--border-default);
    font-size: 11px;
    color: var(--text-muted);
    text-align: center;
  }
</style>
