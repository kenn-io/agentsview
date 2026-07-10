<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { yokedDates } from "../../stores/yokedDates.svelte.js";
  import SettingsSection from "./SettingsSection.svelte";
</script>

<SettingsSection
  title={m.settings_date_ranges_title()}
  description={m.settings_date_ranges_description()}
>
  <label class="date-range-toggle">
    <input
      type="checkbox"
      checked={yokedDates.enabled}
      onchange={(event) =>
        yokedDates.setEnabled(event.currentTarget.checked)}
    />
    <span>{m.settings_date_ranges_link()}</span>
  </label>
</SettingsSection>

<style>
  .date-range-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--text-secondary);
    font-size: 12px;
    cursor: pointer;
  }

  .date-range-toggle input {
    accent-color: var(--accent-blue);
  }
</style>
