---
name: localization-paraglide
description: Use when adding, reviewing, or fixing localized UI copy in agentsview's Svelte frontend with Paraglide JS. Trigger for frontend/messages/*.json edits, generated m.* message usage, locale-aware number/date/relative-time formatting, pluralization, language switching, or hard-coded user-facing English in frontend/src.
---

# Localization Paraglide

## Workflow

1. Read `frontend/project.inlang/settings.json`,
   `frontend/src/lib/i18n/index.ts`, and nearby localized components before
   editing.

1. Put user-facing copy in `frontend/messages/en.json` and in every other locale
   catalog listed in `frontend/project.inlang/settings.json` (currently
   `zh-CN`, `zh-TW`, `ko`, `fr`, `ja`, `az`, and `es`) with identical keys.

1. Import app localization through `frontend/src/lib/i18n/index.ts`:

    ```ts
    import { m } from "../../i18n/index.js";
    ```

1. Call generated Paraglide messages as functions, for example
   `m.nav_sessions()` or `m.shared_active_filters_remove_agent({ agent })`.

1. Use Paraglide message declarations for plural, number, datetime, and relative
   time when the formatted value is part of translatable copy.

1. Use `formatDateTime()` from `frontend/src/lib/i18n/index.ts` for standalone
   visible date/time labels that need the active Paraglide locale.

1. Keep technical identifiers untranslated: agent names, model names, file
   paths, CLI commands, IDs, and raw API values.

1. Run `npm run i18n:compile` and `npm run check` from `frontend/` after message
   or component changes.

## Message Rules

- Keep key names scoped and descriptive, such as `settings_terminal_title` or
  `activity_concurrency_empty`.
- Do not concatenate translated sentence fragments. Prefer one complete message
  with parameters.
- Pass numbers as numbers to pluralized messages. Pass strings only for display
  fragments that have already been intentionally formatted.
- In Svelte, put arrays or objects containing translated labels in `$derived` or
  `$derived.by` when they must update after locale changes.
- Do not import from `frontend/src/lib/paraglide/*` directly in components
  unless changing the i18n wrapper itself.

## Formatting

Read `references/paraglide-formatting.md` when adding pluralization, ordinal
rules, date/time formatting, relative dates, compact numbers, currencies, or
mixed selector messages.
