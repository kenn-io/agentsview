<script lang="ts">
  import { _ } from "svelte-i18n";
  import {
    SUPPORTED_LOCALES,
    chooseInitialLocale,
    setLocale,
    type SupportedLocale,
  } from "../../i18n/index.js";
  import OptionTypeahead, {
    type TypeaheadOption,
  } from "../layout/OptionTypeahead.svelte";
  import SettingsSection from "./SettingsSection.svelte";

  function currentLocale(): SupportedLocale {
    return chooseInitialLocale();
  }

  let selectedLocale = $state<SupportedLocale>(currentLocale());

  const localeOptions: TypeaheadOption[] = $derived([
    {
      name: "en",
      label: $_("settings.language.english"),
    },
    {
      name: "zh-CN",
      label: $_("settings.language.simplifiedChinese"),
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
  title={$_("settings.language.title")}
  description={$_("settings.language.description")}
  allowOverflow
>
  <div class="setting-row">
    <span class="setting-label">{$_("settings.language.label")}</span>
    <OptionTypeahead
      options={localeOptions}
      value={selectedLocale}
      fallbackLabel={$_("settings.language.english")}
      placeholder={$_("settings.language.label")}
      title={$_("settings.language.label")}
      emptyLabel={$_("settings.language.noResults")}
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
