import { expect, test, type Page, type Route } from "@playwright/test";
import { clickNavTab, expectActiveNavTab } from "./helpers/nav";

const DATE = "2026-09-21";
const OLDER = "2026-09-20";
const SESSION_ID = "test-session-medium-8";
const ORDINAL = 3;

function listItem(date: string) {
  return {
    date,
    timezone: "UTC",
    rules_version: "friction-v1",
    built_at: `${date}T23:59:00Z`,
    revision: 1,
    sessions_scanned: 2,
  };
}

function signal(overrides: Record<string, unknown>) {
  return {
    kind: "correction",
    detector: "correction.coding",
    subject_id: SESSION_ID,
    subject_kind: "session",
    title: "",
    fingerprint: `fl1:${"a".repeat(64)}`,
    text: "",
    tool_name: "",
    label: "",
    evidence: "",
    message_ordinal: null,
    call_index: null,
    occurred_at: null,
    seat: "",
    agent: "",
    machine: "",
    persona: "",
    channel: "",
    session_url: "",
    ...overrides,
  };
}

function digest(date: string, signals: Record<string, unknown>[]) {
  return {
    date,
    timezone: "UTC",
    rules_version: "friction-v1",
    built_at: `${date}T23:59:00Z`,
    revision: 1,
    sessions_scanned: 2,
    markdown_sha256: "0".repeat(64),
    summary: {
      date,
      schema_version: 1,
      sessions_scanned: 2,
      corrections: signals.filter((s) => s.kind === "correction").length,
      errors: signals.filter((s) => s.kind === "error").length,
      workarounds: 0,
      deferrals: 0,
      patterns: signals.filter((s) => s.kind === "pattern").length,
      frustrations: signals.filter((s) => s.kind === "frustration").length,
      interruptions: signals.filter((s) => s.kind === "interruption").length,
      p0_alerts: { bash: ["session-a", "session-b", "session-c"] },
    },
    signals,
    p0_alerts: [{ tool: "bash", subject_ids: ["session-a", "session-b", "session-c"] }],
    patterns: [],
  };
}

const LATEST_DIGEST = digest(DATE, [
  signal({
    title: `[friction/correction] ${SESSION_ID}: no, use the other flag`,
    text: "no, use the other flag",
    message_ordinal: ORDINAL,
  }),
  signal({
    kind: "error",
    detector: "error",
    tool_name: "bash",
    text: "command not found",
    fingerprint: `fl1:${"b".repeat(64)}`,
    message_ordinal: 5,
  }),
  signal({
    kind: "error",
    detector: "error",
    subject_id: "nightly-42:ci_nightly",
    subject_kind: "diagnostic",
    tool_name: "ci_nightly",
    text: "nightly-42:ci_nightly: nightly check failed",
    fingerprint: `fl1:${"c".repeat(64)}`,
  }),
  signal({
    kind: "frustration",
    detector: "frustration",
    text: "this is still broken",
    fingerprint: `fl1:${"d".repeat(64)}`,
    message_ordinal: 6,
  }),
  signal({
    kind: "interruption",
    detector: "interruption",
    fingerprint: `fl1:${"e".repeat(64)}`,
    message_ordinal: 2,
  }),
]);
const OLDER_DIGEST = {
  ...digest(OLDER, []),
  p0_alerts: [],
  summary: { ...digest(OLDER, []).summary, p0_alerts: {} },
};

const MARKDOWN = [
  "---",
  `date: ${DATE}`,
  "signals_captured: 5",
  "p0_count: 1",
  "corrections: 1",
  "errors: 2",
  "workarounds: 0",
  "deferrals: 0",
  "patterns: 0",
  "frustrations: 1",
  "interruptions: 1",
  "---",
  "",
  `# Friction Log — ${DATE}`,
  "",
].join("\n");

async function mockFriction(page: Page) {
  await page.route("**/api/v1/version", (route: Route) =>
    route.fulfill({
      json: {
        api_version: 1,
        data_version: 1,
        insight_generation_available: false,
        friction_available: true,
        friction_build_allowed: true,
        friction_readable: true,
        version: "dev",
        commit: "unknown",
        build_date: "",
        read_only: false,
      },
    }),
  );
  await page.route("**/api/v1/friction/**", async (route: Route) => {
    const path = new URL(route.request().url()).pathname.replace(/\/+$/, "");
    if (path.endsWith("/api/v1/friction/digests")) {
      await route.fulfill({ json: { digests: [listItem(OLDER), listItem(DATE)] } });
    } else if (path.endsWith(`/api/v1/friction/digests/${DATE}/md`)) {
      await route.fulfill({ status: 200, contentType: "text/markdown", body: MARKDOWN });
    } else if (path.endsWith(`/api/v1/friction/digests/${DATE}`)) {
      await route.fulfill({ json: LATEST_DIGEST });
    } else if (path.endsWith(`/api/v1/friction/digests/${OLDER}`)) {
      await route.fulfill({ json: OLDER_DIGEST });
    } else if (path.endsWith("/api/v1/friction/findings")) {
      await route.fulfill({ json: { findings: [], next_cursor: "" } });
    } else {
      await route.fulfill({ status: 404, json: { detail: `unmocked ${path}` } });
    }
  });
}

test.describe("Friction Log", () => {
  test.beforeEach(async ({ page }) => {
    await mockFriction(page);
  });

  test("opens the latest digest and follows a finding to its message", async ({ page }) => {
    // Keep this link test focused on the target session. Hydrating every
    // unrelated fixture row can trigger the session sidebar's known Svelte
    // update-depth race before the target message is rendered.
    await page.route("**/api/v1/sessions/sidebar-index**", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      await route.fulfill({
        json: {
          ...body,
          sessions: body.sessions.filter((row: { id: string }) => row.id === SESSION_ID),
          total: 1,
          next_cursor: null,
        },
      });
    });
    await page.goto("/friction");

    await expect(page.getByRole("heading", { level: 1 })).toHaveText(`Friction Log — ${DATE}`);
    const attention = page.locator(".friction-headline");
    await expect(attention).toContainText("Tools failing across sessions");
    await expect(attention).toContainText("failed in 3 sessions");

    const corrections = page.locator("section#friction-section-correction");
    await expect(corrections).toContainText("no, use the other flag");
    await corrections.locator("a.session-link").first().click();

    await expect(page).toHaveURL(new RegExp(`/sessions/${SESSION_ID}\\?msg=${ORDINAL}$`));
    await expect(page.locator(".message-list-scroll")).toHaveAttribute(
      "data-messages-session-id",
      SESSION_ID,
      { timeout: 15_000 },
    );
    await expect(page.locator(".virtual-row.selected")).toContainText("Assistant response 3 of 8", {
      timeout: 15_000,
    });
  });

  test("is reachable from the primary navigation", async ({ page }) => {
    await page.goto("/");
    await clickNavTab(page, "Friction Log");
    await expect(page).toHaveURL(new RegExp(`/friction\\?date=${DATE}$`));
    await expectActiveNavTab(page, "Friction Log");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(`Friction Log — ${DATE}`);
  });

  test("steps to the older digest and back through the URL", async ({ page }) => {
    await page.goto("/friction");
    await page.getByRole("button", { name: "Previous digest" }).click();
    await expect(page).toHaveURL(new RegExp(`/friction\\?date=${OLDER}$`));
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(`Friction Log — ${OLDER}`);
    await expect(page.getByText("No friction found")).toBeVisible();

    await page.reload();
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(`Friction Log — ${OLDER}`);
  });

  test("shows frustration, counts interruptions and leaves diagnostics unlinked", async ({
    page,
  }) => {
    await page.goto("/friction");
    await expect(page.locator("section#friction-section-frustration")).toContainText(
      "this is still broken",
    );
    const interruptions = page.locator("section#friction-section-interruption");
    await expect(interruptions).toContainText("1 interruption");
    await expect(interruptions.locator("a.session-link")).toHaveAttribute(
      "href",
      `/sessions/${SESSION_ID}?msg=2`,
    );
    const diagnostic = page
      .locator("section#friction-section-error li")
      .filter({ hasText: "nightly-42:ci_nightly" });
    await expect(diagnostic.locator("code.identity")).toHaveText("nightly-42:ci_nightly");
    await expect(diagnostic.locator("a")).toHaveCount(0);
  });

  test("shows the stored Markdown bytes", async ({ page }) => {
    await page.goto("/friction");
    await page.getByRole("button", { name: "Show Markdown" }).click();
    await expect(page.locator("pre.markdown-source")).toContainText(`# Friction Log — ${DATE}`);
  });

  test("files a recurring pattern to Kata and shows the issue", async ({ page }) => {
    const fingerprint = `fl1:${"b".repeat(64)}`;
    const issue = {
      qualified_id: "agentsview#f001",
      web_url: "https://kata.example.test/issues/f001",
      issue_uid: "01J0ABCDEF0000000000000001",
      source: "created",
      linked_at: `${DATE}T08:00:00Z`,
    };
    await page.route("**/api/v1/kata/status", (route: Route) =>
      route.fulfill({ json: { state: "ready", project: "agentsview" } }),
    );
    await page.route(`**/api/v1/friction/digests/${DATE}`, (route: Route) =>
      route.fulfill({
        json: {
          ...LATEST_DIGEST,
          patterns: [
            {
              fingerprint,
              kind: "error",
              title: "[friction/error] bash: command not found",
              first_seen_date: "2026-09-01",
              last_seen_date: DATE,
              occurrence_count: 3,
              session_count: 2,
              last_subject_id: SESSION_ID,
              last_ordinal: 5,
            },
          ],
        },
      }),
    );
    let filed = "";
    await page.route("**/api/v1/friction/patterns/*/issue", async (route: Route) => {
      filed = decodeURIComponent(new URL(route.request().url()).pathname.split("/")[5] ?? "");
      await route.fulfill({ json: { issue, created: true } });
    });

    await page.goto("/friction");
    const row = page
      .locator(".friction-headline .headline-row")
      .filter({ hasText: "command not found" });
    await row.getByRole("button", { name: "File to Kata" }).click();

    await expect(row.locator("a.issue-link")).toHaveText("agentsview#f001");
    await expect(row.getByRole("button", { name: "File to Kata" })).toHaveCount(0);
    expect(filed).toBe(fingerprint);
    await expect(
      page
        .locator("section#friction-section-error li")
        .filter({ hasText: "command not found" })
        .locator(".row-issue"),
    ).toHaveText("agentsview#f001");
  });

  test("falls back to the latest digest for an unknown date", async ({ page }) => {
    await page.goto("/friction?date=2020-01-01");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(`Friction Log — ${DATE}`);
  });
});
