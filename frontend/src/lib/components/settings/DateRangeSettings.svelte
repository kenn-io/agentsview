<script lang="ts">
  import { Toggle } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { yokedDates } from "../../stores/yokedDates.svelte.js";
</script>

<!-- The switch sits in the settings control column with its On/Off
     state, like every other settings switch. -->
<div class="setting-row compact">
  <span class="setting-label">{m.settings_date_ranges_link()}</span>
  <Toggle
    checked={yokedDates.enabled}
    onchange={(checked) => yokedDates.setEnabled(checked)}
    ariaLabel={m.settings_date_ranges_link()}
  >
    {yokedDates.enabled ? m.appearance_on() : m.appearance_off()}
  </Toggle>
</div>
