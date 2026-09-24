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
    web_url: "",
    summary: {
      schema_version: 3,
      sessions_scanned: 2,
      tracker_failures: 0,
      corrections: signals.filter((s) => s.kind === "correction").length,
      errors: signals.filter((s) => s.kind === "error").length,
      workarounds: 0,
      deferrals: 0,
      patterns: signals.filter((s) => s.kind === "pattern").length,
      frustrations: signals.filter((s) => s.kind === "frustration").length,
      interruptions: signals.filter((s) => s.kind === "interruption").length,
      p0_alerts: { bash: ["session-a", "session-b", "session-c"] },
      spend: null,
      digest_path: `friction:${date}`,
      created_issues: [],
    },
    signals,
    p0_alerts: [{ tool: "bash", subject_ids: ["session-a", "session-b", "session-c"] }],
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
const OLDER_DIGEST = digest(OLDER, []);

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
        kata_available: false,
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
    } else if (path.endsWith("/api/v1/friction/patterns")) {
      await route.fulfill({ json: { patterns: [], next_cursor: "" } });
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
    await expect(page.getByText("bash failed in 3 distinct sessions")).toBeVisible();

    const corrections = page.locator('section[aria-labelledby="friction-section-correction"]');
    await expect(corrections).toContainText("no, use the other flag");
    await corrections.locator("a.subject-link").first().click();

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
    await expect(page).toHaveURL(/\/friction$/);
    await expectActiveNavTab(page, "Friction Log");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(`Friction Log — ${DATE}`);
  });

  test("steps to the older digest and back through the URL", async ({ page }) => {
    await page.goto("/friction");
    await page.getByRole("button", { name: "Previous digest" }).click();
    await expect(page).toHaveURL(new RegExp(`/friction\\?date=${OLDER}$`));
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(`Friction Log — ${OLDER}`);
    await expect(
      page.locator('section[aria-labelledby="friction-section-correction"]'),
    ).toContainText("No corrections detected.");

    await page.reload();
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(`Friction Log — ${OLDER}`);
  });

  test("shows frustration, counts interruptions and leaves diagnostics unlinked", async ({
    page,
  }) => {
    await page.goto("/friction");
    await expect(
      page.locator('section[aria-labelledby="friction-section-frustration"]'),
    ).toContainText("this is still broken");
    const interruptions = page.locator('section[aria-labelledby="friction-section-interruption"]');
    await expect(interruptions).toContainText("1 interruption");
    await expect(interruptions.locator("a.subject-link")).toHaveAttribute(
      "href",
      `/sessions/${SESSION_ID}?msg=2`,
    );
    const diagnostic = page
      .locator('section[aria-labelledby="friction-section-error"] li')
      .filter({ hasText: "nightly-42:ci_nightly" });
    await expect(diagnostic.locator("code.subject")).toHaveText("nightly-42:ci_nightly");
    await expect(diagnostic.locator("a")).toHaveCount(0);
  });

  test("shows the stored Markdown bytes", async ({ page }) => {
    await page.goto("/friction");
    await page.getByRole("button", { name: "Show Markdown" }).click();
    await expect(page.locator("pre.markdown-source")).toContainText(`# Friction Log — ${DATE}`);
  });

  test("falls back to the latest digest for an unknown date", async ({ page }) => {
    await page.goto("/friction?date=2020-01-01");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(`Friction Log — ${DATE}`);
  });

  test("files, links, and unlinks headline patterns through the hub", async ({ page }) => {
    const fingerprint = `fl1:${"f".repeat(64)}`;
    const pattern = {
      fingerprint,
      kind: "pattern",
      title: "[friction/pattern] retry loop",
      first_seen_date: DATE,
      last_seen_date: DATE,
      occurrence_count: 2,
      session_count: 1,
      last_subject_id: SESSION_ID,
      last_ordinal: null,
    };
    let link: Record<string, unknown> | undefined;
    let kataAvailable = true;
    let readOnly = false;
    const mutations: { method: string; path: string; body: unknown }[] = [];
    await page.route("**/api/v1/version", (route) =>
      route.fulfill({
        json: {
          api_version: 1,
          data_version: 1,
          insight_generation_available: false,
          friction_available: true,
          kata_available: kataAvailable,
          version: "dev",
          commit: "unknown",
          build_date: "",
          read_only: readOnly,
        },
      }),
    );
    await page.route(`**/api/v1/friction/digests/${DATE}`, (route) =>
      route.fulfill({
        json: digest(DATE, [
          signal({
            kind: "pattern",
            detector: "pattern.retry_loop",
            fingerprint,
            label: "retry_loop",
          }),
        ]),
      }),
    );
    await page.route("**/api/v1/friction/patterns**", async (route) => {
      const request = route.request();
      const path = new URL(request.url()).pathname;
      const decodedPath = decodeURIComponent(path);
      const method = request.method();
      if (method === "GET" && path.endsWith("/patterns")) {
        await route.fulfill({
          json: { patterns: [{ ...pattern, ...(link ? { link } : {}) }], next_cursor: "" },
        });
        return;
      }
      const body = request.postDataJSON();
      mutations.push({ method, path, body });
      if (method === "POST" && decodedPath.endsWith(`/${fingerprint}/file`)) {
        link = {
          state: "linked",
          qualified_id: "agentsview#filed",
          web_url: "https://kata.example/issues/filed",
        };
      } else if (method === "PUT" && decodedPath.endsWith(`/${fingerprint}/link`)) {
        link = {
          state: "linked",
          qualified_id: "agentsview#existing",
          web_url: "https://kata.example/issues/existing",
        };
      } else if (method === "DELETE" && decodedPath.endsWith(`/${fingerprint}/link`)) {
        link = undefined;
      } else {
        throw new Error(`Unexpected Friction Log mutation ${method} ${path}`);
      }
      await route.fulfill({ json: method === "DELETE" ? { unlinked: true } : { link } });
    });

    await page.goto("/friction");
    const row = page.locator(".pattern-row").filter({ hasText: "retry loop" });
    await expect(row.getByRole("button", { name: "File to Kata" })).toBeVisible();
    await row.getByRole("button", { name: "File to Kata" }).click();
    await expect(row).toContainText("agentsview#filed");
    await expect(row.getByRole("link", { name: "agentsview#filed" })).toHaveAttribute(
      "href",
      "https://kata.example/issues/filed",
    );

    await row.getByRole("button", { name: "Unlink" }).click();
    await expect(row.getByRole("button", { name: "Link issue" })).toBeVisible();
    await row.getByRole("button", { name: "Link issue" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Link issue" }).click();
    await expect(dialog).toContainText("Enter a Kata issue reference.");
    await dialog
      .getByRole("textbox", { name: "Kata issue reference" })
      .fill("  agentsview#existing  ");
    await dialog.getByRole("button", { name: "Link issue" }).click();
    await expect(row).toContainText("agentsview#existing");
    await row.getByRole("button", { name: "Unlink" }).click();
    await expect(row.getByRole("button", { name: "File to Kata" })).toBeVisible();

    expect(mutations).toEqual([
      {
        method: "POST",
        path: `/api/v1/friction/patterns/${encodeURIComponent(fingerprint)}/file`,
        body: {},
      },
      {
        method: "DELETE",
        path: `/api/v1/friction/patterns/${encodeURIComponent(fingerprint)}/link`,
        body: null,
      },
      {
        method: "PUT",
        path: `/api/v1/friction/patterns/${encodeURIComponent(fingerprint)}/link`,
        body: { issue_ref: "agentsview#existing" },
      },
      {
        method: "DELETE",
        path: `/api/v1/friction/patterns/${encodeURIComponent(fingerprint)}/link`,
        body: null,
      },
    ]);

    kataAvailable = false;
    await page.reload();
    await expect(row).toBeVisible();
    await expect(row.locator("button")).toHaveCount(0);
    kataAvailable = true;
    readOnly = true;
    await page.reload();
    await expect(row).toBeVisible();
    await expect(row.locator("button")).toHaveCount(0);
  });
});
