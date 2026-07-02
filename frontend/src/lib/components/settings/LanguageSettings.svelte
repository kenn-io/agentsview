<script lang="ts">
  import { m } from "../../i18n/index.js";
  import {
    SUPPORTED_LOCALES,
    chooseInitialLocale,
    setLocale,
    type SupportedLocale,
  } from "../../i18n/index.js";
  import { Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import SettingsSection from "./SettingsSection.svelte";

  function currentLocale(): SupportedLocale {
    return chooseInitialLocale();
  }

  let selectedLocale = $state<SupportedLocale>(currentLocale());

  const localeOptions: TypeaheadOption[] = $derived([
    {
      name: "en",
      label: m.settings_language_english(),
    },
    {
      name: "zh-CN",
      label: m.settings_language_simplified_chinese(),
    },
    {
      name: "zh-TW",
      label: m.settings_language_traditional_chinese(),
    },
  ]);

  function handleLocaleSelect(value: string) {
    if (!SUPPORTED_LOCALES.includes(value as SupportedLocale)) return;
    const locale = value as SupportedLocale;
    selectedLocale = locale;
    setLocale(locale);
  }
</script>

<SettingsSection
  title={m.settings_language_title()}
  description={m.settings_language_description()}
>
  <div class="setting-row">
    <span class="setting-label">{m.settings_language_label()}</span>
    <Typeahead
      options={localeOptions}
      value={selectedLocale}
      fallbackLabel={m.settings_language_english()}
      placeholder={m.settings_language_label()}
      title={m.settings_language_label()}
      emptyLabel={m.settings_language_no_results()}
      onselect={handleLocaleSelect}
    />
  </div>
</SettingsSection>

<style>
  .setting-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
    white-space: nowrap;
  }
</style>
