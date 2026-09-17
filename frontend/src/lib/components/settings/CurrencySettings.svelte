<script lang="ts">
  import { Button, TextInput, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import {
    costDisplay,
    type DisplayCurrency,
  } from "../../stores/costDisplay.svelte.js";

  const initialPreference = costDisplay.preference;
  let draftCurrency = $state<DisplayCurrency>(initialPreference.currency);
  let draftRate = $state(
    initialPreference.eurPerUsd === null
      ? ""
      : String(initialPreference.eurPerUsd),
  );
  let error = $state("");

  const currencyOptions: TypeaheadOption[] = $derived([
    {
      name: "USD",
      label: m.settings_currency_usd(),
    },
    {
      name: "EUR",
      label: m.settings_currency_eur(),
    },
  ]);

  function parseRate(value: string): number | null {
    const trimmed = value.trim();
    if (!trimmed) return null;
    const rate = Number(trimmed);
    return Number.isFinite(rate) && rate > 0 ? rate : null;
  }

  function handleCurrencySelect(value: string): void {
    if (value === "USD" || value === "EUR") {
      draftCurrency = value;
      const rememberedRate = costDisplay.preference.eurPerUsd;
      if (rememberedRate !== null && parseRate(draftRate) === null) {
        draftRate = String(rememberedRate);
      }
      error = "";
    }
  }

  function apply(): void {
    error = "";
    const parsedRate = parseRate(draftRate);
    if (draftCurrency === "EUR" && parsedRate === null) {
      error = m.settings_currency_invalid_rate();
      return;
    }

    const rate = parsedRate ?? costDisplay.preference.eurPerUsd;
    if (!costDisplay.setPreference(draftCurrency, rate)) {
      error = m.settings_currency_invalid_rate();
      return;
    }
    if (rate !== null) draftRate = String(rate);
  }
</script>

<div class="currency-settings">
  <div class="setting-row">
    <span class="setting-label">{m.settings_currency_label()}</span>
    <Typeahead
      options={currencyOptions}
      value={draftCurrency}
      fallbackLabel={m.settings_currency_usd()}
      placeholder={m.settings_currency_label()}
      title={m.settings_currency_label()}
      emptyLabel={m.settings_currency_no_results()}
      onselect={handleCurrencySelect}
    />
  </div>

  <div class="setting-row column">
    <label class="setting-label" for="currency-eur-per-usd">
      {m.settings_currency_rate_label()}
    </label>
    <TextInput
      id="currency-eur-per-usd"
      class="setting-input"
      size="lg"
      type="text"
      placeholder={m.settings_currency_rate_placeholder()}
      ariaLabel={m.settings_currency_rate_label()}
      invalid={error !== ""}
      bind:value={draftRate}
    />
    <p class="setting-help">{m.settings_currency_rate_help()}</p>
    <p class="setting-equation">
      {m.settings_currency_rate_equation({ rate: draftRate || m.settings_currency_rate_unset() })}
    </p>
    {#if error}
      <p class="setting-error" role="alert">{error}</p>
    {/if}
  </div>

  <div class="save-row">
    <Button tone="success" surface="solid" size="sm" onclick={apply}>
      {m.settings_currency_apply()}
    </Button>
  </div>
</div>

<style>
  .currency-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  .setting-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .setting-row.column {
    flex-direction: column;
    align-items: flex-start;
  }

  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
    white-space: nowrap;
  }

  :global(.setting-input.kit-text-input) {
    font-family: var(--font-mono, monospace);
  }

  .setting-help,
  .setting-equation,
  .setting-error {
    margin: 0;
    font-size: 11px;
    color: var(--text-muted);
  }

  .setting-equation {
    font-family: var(--font-mono, monospace);
    color: var(--text-secondary);
  }

  .setting-error {
    color: var(--accent-red);
  }

  .save-row {
    display: flex;
    justify-content: flex-end;
  }
</style>
