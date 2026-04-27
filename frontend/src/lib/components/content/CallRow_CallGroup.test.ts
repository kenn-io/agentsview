// @vitest-environment jsdom
// ABOUTME: Visual smoke test for CallRow + CallGroup. Mounts each component
// with representative props, captures rendered HTML to .test-data18/, and
// asserts the class names & DOM structure match the mockup contract.
//
// Note: this test lives in the frontend tree so vitest picks it up, but its
// captured HTML artifacts are written to ../../.test-data18/ at the worktree
// root for human inspection. Don't delete that directory.
import { afterEach, describe, expect, it } from "vitest";
import { mount, tick, unmount } from "svelte";
// @ts-ignore -- @types/node is not in devDependencies; harmless at runtime.
import { mkdirSync, writeFileSync } from "node:fs";
// @ts-ignore -- @types/node is not in devDependencies; harmless at runtime.
import { resolve } from "node:path";
import type { CallTiming } from "../../api/types/timing.js";
// @ts-ignore
import CallRow from "./CallRow.svelte";
// @ts-ignore
import CallGroup from "./CallGroup.svelte";

const ARTIFACT_DIR = resolve(
  // @ts-ignore -- import.meta.dirname is Node 20.11+, in the supported range.
  import.meta.dirname,
  "../../../../../.test-data18",
);
// Only mutate the worktree when the developer has opted in. Default
// CI/local test runs leave the previously-captured artifacts in place
// so the tree stays clean.
// @ts-ignore -- process is from node, no @types/node configured.
const CAPTURE_ARTIFACTS = process.env.CAPTURE_ARTIFACTS === "1";
if (CAPTURE_ARTIFACTS) {
  mkdirSync(ARTIFACT_DIR, { recursive: true });
}

function makeCall(overrides: Partial<CallTiming> = {}): CallTiming {
  return {
    tool_use_id: "tu-1",
    tool_name: "Read",
    category: "Read",
    duration_ms: 240,
    is_parallel: false,
    input_preview: "src/lib/components/content/SessionVitals.svelte",
    ...overrides,
  };
}

afterEach(() => {
  document.body.innerHTML = "";
});

function dumpHtml(filename: string, html: string) {
  if (!CAPTURE_ARTIFACTS) return;
  writeFileSync(resolve(ARTIFACT_DIR, filename), html, "utf8");
}

// Svelte 5 appends scoped style hashes (e.g. "svelte-t7hivm") to class
// attributes, so assertions on raw class strings need to allow them. This
// helper builds a regex that matches a class attribute containing all the
// given tokens in order, with arbitrary other tokens (typically the scope
// hash) interleaved.
function hasClasses(...tokens: string[]): RegExp {
  const inner = tokens
    .map((t) => `\\b${t}\\b`)
    .join("[^\"]*");
  return new RegExp(`class="[^"]*${inner}[^"]*"`);
}

describe("CallRow", () => {
  it("renders a non-subagent call with category color, args, bar, duration", async () => {
    const c = mount(CallRow, {
      target: document.body,
      props: {
        call: makeCall({
          tool_name: "Bash",
          category: "Bash",
          duration_ms: 1230,
          input_preview: "git status",
        }),
        barWidthPct: 35,
      },
    });
    await tick();
    const html = document.body.innerHTML;
    dumpHtml("call-row-bash.html", html);

    expect(html).toMatch(hasClasses("call"));
    expect(html).toMatch(hasClasses("chev", "spacer")); // non-subagent: spacer
    expect(html).toMatch(hasClasses("cn"));
    expect(html).toContain("var(--cat-bash)");
    expect(html).toMatch(hasClasses("ca"));
    expect(html).toContain("git status");
    expect(html).toMatch(hasClasses("cbar-wrap"));
    expect(html).toMatch(hasClasses("cbar"));
    expect(html).toContain("width: 35%");
    expect(html).toMatch(hasClasses("cd"));
    expect(html).toContain("1.2s");

    unmount(c);
  });

  it("renders a slow call with the slow class on .cd and .call", async () => {
    const c = mount(CallRow, {
      target: document.body,
      props: {
        call: makeCall({ duration_ms: 12000 }),
        barWidthPct: 80,
        isSlow: true,
      },
    });
    await tick();
    const html = document.body.innerHTML;
    dumpHtml("call-row-slow.html", html);

    expect(html).toMatch(hasClasses("call", "slow"));
    expect(html).toMatch(hasClasses("cd", "slow"));

    unmount(c);
  });

  it("renders a live call with the live class on .cbar and .cd", async () => {
    const c = mount(CallRow, {
      target: document.body,
      props: {
        call: makeCall({ duration_ms: 4000 }),
        barWidthPct: 60,
        isLive: true,
      },
    });
    await tick();
    const html = document.body.innerHTML;
    dumpHtml("call-row-live.html", html);

    expect(html).toMatch(hasClasses("cbar", "live"));
    expect(html).toMatch(hasClasses("cd", "live"));
    expect(html).toContain("running 4.0s+");

    unmount(c);
  });

  it("renders a subagent call with an active chevron and expanded class", async () => {
    const c = mount(CallRow, {
      target: document.body,
      props: {
        call: makeCall({
          tool_name: "Task",
          category: "Task",
          duration_ms: 5000,
          subagent_session_id: "sub-1",
          input_preview: "review code",
        }),
        barWidthPct: 50,
        isSubagentExpanded: true,
      },
    });
    await tick();
    const html = document.body.innerHTML;
    dumpHtml("call-row-subagent-expanded.html", html);

    expect(html).toMatch(hasClasses("call", "expanded"));
    // chevron is interactive (no "spacer" token) for subagent rows.
    expect(html).toMatch(hasClasses("chev"));
    expect(html).not.toMatch(hasClasses("chev", "spacer"));
    expect(html).toContain("var(--cat-task)");
    // a11y: chevron carries aria-label and aria-expanded.
    expect(html).toContain('aria-label="Toggle sub-agent calls"');
    expect(html).toContain('aria-expanded="true"');

    unmount(c);
  });

  it("renders a subagent call with a spacer chevron when expandable=false", async () => {
    const c = mount(CallRow, {
      target: document.body,
      props: {
        call: makeCall({
          tool_name: "Task",
          category: "Task",
          duration_ms: 5000,
          subagent_session_id: "sub-1",
          input_preview: "review code",
        }),
        barWidthPct: 50,
        expandable: false,
      },
    });
    await tick();
    const html = document.body.innerHTML;
    dumpHtml("call-row-subagent-not-expandable.html", html);

    // Even though it's a sub-agent row, expandable=false suppresses
    // the interactive chevron and renders the spacer instead.
    expect(html).toMatch(hasClasses("chev", "spacer"));
    expect(html).not.toContain("<button");

    unmount(c);
  });

  it("uses sharedDurationLabel when call has no duration", async () => {
    const c = mount(CallRow, {
      target: document.body,
      props: {
        call: makeCall({ duration_ms: null }),
        barWidthPct: 25,
        isShared: true,
        sharedDurationLabel: "≤2.5s",
      },
    });
    await tick();
    const html = document.body.innerHTML;
    dumpHtml("call-row-shared.html", html);

    expect(html).toContain("≤2.5s");
    expect(html).toMatch(hasClasses("cbar", "shared"));

    unmount(c);
  });
});

describe("CallGroup", () => {
  it("renders the rail, header chip, member rows, and forwards isLive to last row only", async () => {
    const calls: CallTiming[] = [
      makeCall({
        tool_use_id: "tu-1",
        tool_name: "Read",
        category: "Read",
        duration_ms: null,
        input_preview: "main.go",
      }),
      makeCall({
        tool_use_id: "tu-2",
        tool_name: "Read",
        category: "Read",
        duration_ms: null,
        input_preview: "config.go",
      }),
      makeCall({
        tool_use_id: "tu-3",
        tool_name: "Bash",
        category: "Bash",
        duration_ms: null,
        input_preview: "ls -la",
      }),
    ];
    const c = mount(CallGroup, {
      target: document.body,
      props: {
        calls,
        groupDurationMs: 2500,
        barScalePct: () => 40,
        headerBarPct: 70,
        onCallClick: () => {},
        onSubagentExpand: () => {},
        expandedSubagentIds: new Set<string>(),
        isLive: true,
      },
    });
    await tick();
    const html = document.body.innerHTML;
    dumpHtml("call-group-live.html", html);

    expect(html).toMatch(hasClasses("cgroup"));
    expect(html).toMatch(hasClasses("cg-rail"));
    expect(html).toMatch(hasClasses("cg-members"));
    expect(html).toMatch(hasClasses("cg-header"));
    expect(html).toMatch(hasClasses("cg-h-label"));
    expect(html).toContain("parallel · 3 calls");
    expect(html).toMatch(hasClasses("cg-h-bar-wrap"));
    expect(html).toMatch(hasClasses("cg-h-bar"));
    expect(html).toContain("width: 70%");
    expect(html).toMatch(hasClasses("cg-h-dur"));
    expect(html).toContain("2.5s");

    // The last row should be live; the first two should NOT be.
    const liveOccurrences = html.match(/\bcbar\b[^"]*\blive\b/g) ?? [];
    expect(liveOccurrences.length).toBe(1);

    // Per the spec, .shared and .live are mutually exclusive encodings: the
    // first two siblings render with .shared, the live last row drops it.
    const sharedOccurrences = html.match(/\bcbar\b[^"]*\bshared\b/g) ?? [];
    expect(sharedOccurrences.length).toBe(2);

    // The live cbar must not also carry the .shared modifier.
    expect(html).not.toMatch(/\bcbar\b[^"]*\bshared\b[^"]*\blive\b/);
    expect(html).not.toMatch(/\bcbar\b[^"]*\blive\b[^"]*\bshared\b/);

    // Shared duration label "≤2.5s" should appear on rows w/o duration.
    expect(html).toContain("≤2.5s");

    unmount(c);
  });

  it("forwards expandable=false to nested CallRows so subagent chevrons become spacers", async () => {
    const calls: CallTiming[] = [
      makeCall({
        tool_use_id: "tu-a",
        tool_name: "Task",
        category: "Task",
        subagent_session_id: "sub-a",
        duration_ms: 1000,
      }),
      makeCall({
        tool_use_id: "tu-b",
        tool_name: "Read",
        category: "Read",
        duration_ms: 200,
      }),
    ];
    const c = mount(CallGroup, {
      target: document.body,
      props: {
        calls,
        groupDurationMs: 1500,
        barScalePct: () => 30,
        headerBarPct: 50,
        onCallClick: () => {},
        onSubagentExpand: () => {},
        expandedSubagentIds: new Set<string>(),
        expandable: false,
      },
    });
    await tick();
    const html = document.body.innerHTML;
    dumpHtml("call-group-not-expandable.html", html);

    // No interactive chevron buttons should render even though one
    // of the calls is a sub-agent.
    expect(html).not.toContain("<button");
    // Both rows should render as the spacer variant.
    const spacers = html.match(/\bchev\b[^"]*\bspacer\b/g) ?? [];
    expect(spacers.length).toBe(2);

    unmount(c);
  });

  it("renders an em-dash header duration when groupDurationMs is null", async () => {
    const calls: CallTiming[] = [
      makeCall({ tool_use_id: "x1", duration_ms: 100 }),
      makeCall({ tool_use_id: "x2", duration_ms: 200 }),
    ];
    const c = mount(CallGroup, {
      target: document.body,
      props: {
        calls,
        groupDurationMs: null,
        barScalePct: () => 10,
        headerBarPct: 12,
        onCallClick: () => {},
        onSubagentExpand: () => {},
        expandedSubagentIds: new Set<string>(),
      },
    });
    await tick();
    const html = document.body.innerHTML;
    dumpHtml("call-group-unknown.html", html);

    expect(html).toContain("—");

    unmount(c);
  });
});
