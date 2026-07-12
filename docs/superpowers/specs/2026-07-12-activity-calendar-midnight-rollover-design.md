# Activity Calendar Midnight Rollover Design

## Problem

The Activity page passes the current local date to the shared range picker as
its maximum selectable date. That value is evaluated when the page mounts and
does not change while the page remains open. After local midnight, the newly
current day therefore remains disabled until the page is refreshed or remounted.

## Desired behavior

While the Activity page remains mounted, its calendar maximum advances at each
local midnight. The newly current day becomes selectable without a page refresh,
including when the tab has been left idle or backgrounded.

## Design

`ActivityPage.svelte` will own a reactive local-date maximum and a page-local
one-shot timer. On mount, the timer will be scheduled for the next local
midnight. When it fires, the page will recompute the local date, update the
maximum passed to `RangePicker`, and schedule the following local midnight.

The next boundary will be constructed as the next local calendar day at midnight
rather than by adding a fixed 24-hour duration. This preserves the correct
boundary across daylight-saving transitions. The callback will derive the
displayed date from the current clock, so delayed background-tab timers catch up
to the actual day when the browser runs them. The timer will be cleared when the
page unmounts.

The existing shared `RangePicker` API and kit-ui calendar remain unchanged.
There are no API, URL, data model, styling, or localization changes.

## Testing

A component regression test will mount the Activity page immediately before
local midnight under fake timers and interact with the real shared range picker.
It will verify that the following day is initially disabled, advance the clock
beyond midnight without remounting the page, then verify that the same day
becomes enabled and can be selected.

The test protects the user-observable contract rather than the timer's internal
implementation. Existing Activity page tests and the frontend type and
localization checks will also run before handoff.
