# Paraglide Formatting Reference

Official docs:

- Basic usage and generated messages:
  <https://github.com/opral/paraglide-js/blob/main/docs/basics.md>
- Variants and plural selectors:
  <https://github.com/opral/paraglide-js/blob/main/docs/variants.md>
- Number, datetime, and relative-time formatting:
  <https://github.com/opral/paraglide-js/blob/main/docs/formatting.md>

## Generated Messages

Use generated message functions through the app wrapper:

```ts
import { m } from "../../i18n/index.js";

m.greeting({ name: "Ada" });
```

Messages live in `frontend/messages/{locale}.json`. The English and Simplified
Chinese catalogs must have the same keys.

## Cardinal Plurals

Use variant messages for counts. Paraglide's `plural` selector uses
`Intl.PluralRules`, so it works for locales with more than English singular and
plural categories.

```json
{
  "tool_call_group_call_count": [
    {
      "declarations": [
        "input count",
        "local countPlural = count: plural"
      ],
      "selectors": ["countPlural"],
      "match": {
        "countPlural=one": "{count} tool call",
        "countPlural=other": "{count} tool calls"
      }
    }
  ]
}
```

Call with a number:

```ts
m.tool_call_group_call_count({ count: 3 });
```

For `zh-CN`, use the same declaration and provide the matching locale text; a
single `other` or wildcard branch is often enough only when the locale truly has
no visible plural distinction.

## Ordinals

Use `type=ordinal` for values such as 1st, 2nd, and 3rd.

```json
{
  "ranking_place": [
    {
      "declarations": [
        "input place",
        "local placePlural = place: plural type=ordinal"
      ],
      "selectors": ["placePlural"],
      "match": {
        "placePlural=one": "{place}st",
        "placePlural=two": "{place}nd",
        "placePlural=few": "{place}rd",
        "placePlural=*": "{place}th"
      }
    }
  ]
}
```

## Date And Time

For dates inside translatable copy, use the Paraglide `datetime` formatter in the
message declaration so locale formatting and word order stay in the catalog.

```json
{
  "session_started_at": [
    {
      "declarations": [
        "input startedAt",
        "local started = startedAt: datetime dateStyle=medium timeStyle=short"
      ],
      "match": {
        "startedAt=*": "Started {started}"
      }
    }
  ]
}
```

For standalone visible date/time labels in components, use the app helper:

```ts
import { formatDateTime } from "../../i18n/index.js";

formatDateTime(timestamp, {
  month: "short",
  day: "numeric",
  timeZone,
});
```

Do not hard-code `"en"` or `"en-US"` for visible UI formatting. It is acceptable
to use a fixed locale for internal sentinel calculations when the formatted
string is not displayed.

## Relative Dates

Use Paraglide's `relativetime` formatter. The `unit` option is required.

```json
{
  "status_bar_synced_ago": [
    {
      "declarations": [
        "input duration",
        "local formattedDuration = duration: relativetime unit=minute numeric=auto"
      ],
      "match": {
        "duration=*": "synced {formattedDuration}"
      }
    }
  ]
}
```

Use a variable unit only when the caller computes the unit intentionally:

```json
{
  "updated_relative": [
    {
      "declarations": [
        "input duration",
        "input unit",
        "local formattedDuration = duration: relativetime unit=$unit style=short"
      ],
      "match": {
        "duration=*,unit=*": "Updated {formattedDuration}"
      }
    }
  ]
}
```

## Numbers And Currency

Prefer Paraglide's `number` formatter for numbers embedded in messages:

```json
{
  "usage_total_cost": [
    {
      "declarations": [
        "input amount",
        "local cost = amount: number style=currency currency=USD"
      ],
      "match": {
        "amount=*": "Total cost {cost}"
      }
    }
  ]
}
```

Use raw `toLocaleString()` only for non-sentence labels where the browser or
app-level locale behavior is already intentional.
