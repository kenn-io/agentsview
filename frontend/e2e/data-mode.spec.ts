import { expect, test, type Page } from "@playwright/test";

const isDuckDBBackend = process.env.AGENTSVIEW_E2E_BACKEND === "duckdb";
const mappingWorkspaceE2EEnabled = process.env.PROJECT_MAPPING_WORKSPACE_E2E_ENABLED === "true";
const wrongProject = "wrong_branch_label";
const targetProject = "sample_service";
const machine = "remote-example-host";
const worktreeRoot = "/srv/worktrees/github.com/example-org/sample-service/example-worktree";
const broaderPrefix = "/srv/worktrees/github.com/example-org/sample-service";

function workspace(page: Page) {
  return page.locator("section.workspace");
}

function waitForApiResponse(page: Page, method: string, pathname: string) {
  return page
    .waitForResponse((response) => {
      const url = new URL(response.url());
      return response.request().method() === method && url.pathname === pathname && response.ok();
    })
    .then(async (response) => {
      await response.finished();
      return response;
    });
}

test.describe("Data mode project reclassification", () => {
  test.skip(
    !mappingWorkspaceE2EEnabled,
    "requires a frontend build with the project mapping workspace enabled",
  );

  test.skip(
    ({ browserName }) => browserName !== "chromium",
    "the workflow mutates the shared fixture",
  );

  test.afterEach(async ({ request, baseURL, browserName }) => {
    if (!mappingWorkspaceE2EEnabled || isDuckDBBackend || browserName !== "chromium") return;

    // Restore the shared fixture even when an assertion fails after Save.
    // Deleting the rule alone does not undo the session reclassification.
    const response = await request.get("/api/v1/settings/worktree-mappings", {
      params: { machine },
    });
    await expect(response).toBeOK();
    const { mappings } = await response.json();
    const headers = { Origin: baseURL! };
    for (const mapping of mappings) {
      if (mapping.path_prefix !== broaderPrefix) continue;
      const restored = await request.put(`/api/v1/settings/worktree-mappings/${mapping.id}`, {
        headers,
        data: { path_prefix: broaderPrefix, project: wrongProject, enabled: true },
      });
      await expect(restored).toBeOK();
      const applied = await request.post("/api/v1/settings/worktree-mappings/apply", {
        headers,
        data: { machine },
      });
      await expect(applied).toBeOK();
      const deleted = await request.delete(`/api/v1/settings/worktree-mappings/${mapping.id}`, {
        headers,
      });
      await expect(deleted).toBeOK();
    }
  });

  test("keeps the bulk destination and Save visible while observed folders scroll", async ({
    page,
  }) => {
    test.skip(isDuckDBBackend, "requires correction controls");
    const project = {
      project_key: "layout-fixture",
      label: "project-a",
      sessions: 80,
      machines: 1,
      agents: 1,
      distinct_cwds: 80,
      enabled_rules_targeting: 0,
      recorded_as_original: false,
    };
    await page.route("**/api/v1/data/projects", (route) =>
      route.fulfill({
        json: { projects: [project], total_projects: 1, total_sessions: 80, governed_sessions: 0 },
      }),
    );
    await page.route("**/api/v1/data/projects/layout-fixture/sessions?*", (route) =>
      route.fulfill({
        json: {
          sessions: ["session-a", "session-b"].map((id) => ({
            id,
            agent: "claude",
            project: "project-a",
            cwd: "/srv/checkouts/project-0",
          })),
          total: 2,
        },
      }),
    );
    await page.route("**/api/v1/sessions/session-*/messages?*", (route) =>
      route.fulfill({
        json: {
          messages: Array.from({ length: 12 }, (_, id) => ({
            id,
            role: "user",
            content: "Explain this project's folder mapping. ".repeat(20),
          })),
          count: 12,
        },
      }),
    );
    await page.route("**/api/v1/data/project-reclassification/candidates?*", (route) =>
      route.fulfill({
        json: {
          candidates: Array.from({ length: 40 }, (_, i) => ({
            id: `folder-${i}`,
            machine: "host.example",
            suggested_prefix: `/srv/checkouts/project-${i}`,
            contributing_sessions: 2,
            distinct_cwds: 2,
            evidence_kind: "parent",
            available: true,
            examples: [],
          })),
        },
      }),
    );
    for (const viewport of [
      { width: 1440, height: 900 },
      { width: 980, height: 650 },
      { width: 600, height: 700 },
    ]) {
      await page.setViewportSize(viewport);
      await page.goto("/data?project_key=layout-fixture");
      await page.getByRole("radio", { name: "All folders", exact: true }).click();
      if (viewport.width >= 980) {
        const heading = await page
          .getByRole("heading", { name: "Projects", exact: true })
          .boundingBox();
        const summary = await page.getByText("80 sessions", { exact: true }).first().boundingBox();
        expect(summary!.y).toBeGreaterThanOrEqual(heading!.y);
        expect(summary!.y + summary!.height).toBeLessThanOrEqual(heading!.y + heading!.height);
        expect(summary!.x).toBeGreaterThan(heading!.x + heading!.width);
      }
      const ws = workspace(page);
      const save = ws.getByRole("button", { name: "Save 40 corrections" });
      const target = ws.getByTitle("Project", { exact: true });
      await expect(save).toBeVisible();
      await expect(target).toBeVisible();
      const before = await save.boundingBox();
      expect(before).not.toBeNull();
      expect(before!.y + before!.height).toBeLessThan(viewport.height);
      const targetBox = await target.boundingBox();
      expect(targetBox!.x + targetBox!.width).toBeLessThanOrEqual(before!.x);
      const list = ws.locator(".editor > .suggestions");
      const scrolled = await list.evaluate((el) => {
        el.scrollTop = el.scrollHeight;
        return { top: el.scrollTop, height: el.clientHeight };
      });
      expect(scrolled.top).toBeGreaterThan(0);
      expect(scrolled.height).toBeGreaterThan(80);
      expect(await save.boundingBox()).toEqual(before);
      await expect(save).toBeDisabled();
      if (viewport.width === 1440) {
        await page.getByRole("radio", { name: "One folder", exact: true }).click();
        await ws.getByRole("button", { name: "/srv/checkouts/project-0", exact: true }).click();
        const transcript = ws.locator(".session-transcript");
        const beforeCollapse = await transcript.boundingBox();
        await ws.getByRole("button", { name: "Folder suggestions", exact: true }).click();
        const afterCollapse = await transcript.boundingBox();
        expect(afterCollapse!.y).toBeLessThan(beforeCollapse!.y);
        expect(afterCollapse!.height).toBeGreaterThan(beforeCollapse!.height);
        const previewHeading = await ws
          .getByRole("button", { name: "2 session previews", exact: true })
          .boundingBox();
        const next = ws.getByRole("button", { name: "Next session", exact: true });
        const nextBox = await next.boundingBox();
        expect(nextBox!.y + nextBox!.height / 2).toBe(
          previewHeading!.y + previewHeading!.height / 2,
        );
        await next.click();
        await expect(ws.getByText("2 of 2", { exact: true })).toBeVisible();
      }
    }
  });

  test("reclassifies a worktree from Activity through Data and persists a rule", async ({
    page,
  }) => {
    test.skip(isDuckDBBackend, "requires the writable SQLite archive");

    let reclassifyMutations = 0;
    const legacyCandidateRequests: string[] = [];
    page.on("request", (request) => {
      const pathname = new URL(request.url()).pathname;
      if (
        request.method() === "POST" &&
        pathname === "/api/v1/settings/worktree-mappings/reclassify"
      ) {
        reclassifyMutations += 1;
      }
      if (pathname === "/api/v1/activity/project-reclassification/candidates") {
        legacyCandidateRequests.push(pathname);
      }
    });

    const reportPromise = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        response.request().method() === "GET" &&
        url.pathname === "/api/v1/activity/report" &&
        response.ok()
      );
    });
    await page.goto("/activity?window_days=40");
    await reportPromise;
    const reportLoading = page.getByText("Loading activity report...", {
      exact: true,
    });
    await reportLoading.waitFor({ state: "hidden" });
    // The breakdown project link's visible text is the project name itself;
    // "View {project} in Data" lives only in its title attribute, which the
    // accessible name computation ignores once the link has text content.
    const link = page.getByTitle(`View ${wrongProject} in Data`);
    await expect(link).toBeVisible();
    const inventoryPromise = waitForApiResponse(page, "GET", "/api/v1/data/projects");
    const candidatesPromise = waitForApiResponse(
      page,
      "GET",
      "/api/v1/data/project-reclassification/candidates",
    );
    await link.click();
    await Promise.all([inventoryPromise, candidatesPromise]);

    await expect(page).toHaveURL(/\/data\?.*project_key=/);
    await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();
    const ws = workspace(page);
    await expect(ws.getByRole("heading", { name: wrongProject })).toBeVisible();
    await expect(ws.getByRole("button", { name: "Folder suggestions" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    await expect(ws.getByRole("button", { name: worktreeRoot })).toBeVisible();
    const correctionHeader = ws.getByRole("heading", { name: "Project correction" }).locator("..");
    await expect(correctionHeader.getByText(machine, { exact: true })).toBeVisible();
    await expect(ws.locator("header").getByText("2 sessions", { exact: true })).toBeVisible();

    const prefix = ws.getByRole("textbox", { name: "Path prefix" });
    await expect(prefix).toHaveValue(worktreeRoot);
    await prefix.fill(broaderPrefix);

    await ws.getByRole("button", { name: "Project", exact: true }).click();
    const targetInput = ws.getByRole("combobox");
    await targetInput.fill(targetProject);
    const previewPromise = waitForApiResponse(
      page,
      "POST",
      "/api/v1/settings/worktree-mappings/preview",
    );
    await page.getByRole("option", { name: `Use project "${targetProject}"` }).click();

    await previewPromise;
    await expect(ws.getByText("2 sessions matched", { exact: true })).toBeVisible();
    await expect(ws.getByText("2 sessions will change", { exact: true })).toBeVisible();
    await expect(ws.getByText("1 project", { exact: true })).toBeVisible();

    const reclassifyPromise = waitForApiResponse(
      page,
      "POST",
      "/api/v1/settings/worktree-mappings/reclassify",
    );
    const refreshPromise = waitForApiResponse(page, "GET", "/api/v1/data/projects");
    await ws.getByRole("button", { name: "Save correction" }).click();
    await Promise.all([reclassifyPromise, refreshPromise]);

    // Explicit inventory reload; selection follows the applied target.
    await expect(ws.getByRole("heading", { name: targetProject })).toBeVisible();
    expect(reclassifyMutations).toBe(1);
    await expect(page.getByRole("row", { name: targetProject })).toBeVisible();
    await expect(page.getByRole("row", { name: wrongProject })).toHaveCount(0);

    const rulesPromise = waitForApiResponse(page, "GET", "/api/v1/data/project-rules");
    await page
      .locator('[aria-label="Data view"]')
      .getByText("Project mapping rules", { exact: true })
      .click();
    await rulesPromise;
    await expect(page.getByRole("heading", { name: "Worktree mappings" })).toBeVisible();
    await page.getByRole("button", { name: "Select machine" }).click();
    const machineRulesPromise = waitForApiResponse(page, "GET", "/api/v1/data/project-rules");
    await page.getByRole("option", { name: machine, exact: true }).click();
    await machineRulesPromise;

    const rule = page.locator("tr.rule-row");
    await expect(rule).toHaveCount(1);
    await expect(rule).toContainText(broaderPrefix);
    await expect(rule.getByRole("button", { name: targetProject })).toBeVisible();
    await expect(rule).toContainText(wrongProject);
    await expect(rule).toContainText("On");

    expect(legacyCandidateRequests).toEqual([]);
  });

  test("stops at the editor read-only notice without offering mutations", async ({ page }) => {
    test.skip(!isDuckDBBackend, "runs only against duckdb serve");

    const mutationRequests: string[] = [];
    page.on("request", (request) => {
      const pathname = new URL(request.url()).pathname;
      if (request.method() !== "GET" && pathname.startsWith("/api/v1/settings/worktree-mappings")) {
        mutationRequests.push(`${request.method()} ${pathname}`);
      }
    });

    // Wait for version hydration: DataPage also renders read-only while
    // sync.serverVersion is still null, so the assertions below must run
    // against the backend's real read_only flag, not the pre-hydration state.
    const versionPromise = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/version" && response.ok(),
    );
    await page.goto("/data");
    const versionResponse = await versionPromise;
    expect(await versionResponse.json()).toMatchObject({ read_only: true });
    await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();
    const row = page.getByRole("row", { name: wrongProject });
    await expect(row).toBeVisible();
    await row.click();

    const ws = workspace(page);
    await expect(ws.getByRole("button", { name: "Folder suggestions" })).toBeVisible();
    await expect(ws.getByRole("button", { name: worktreeRoot })).toBeVisible();
    await expect(ws.getByRole("note")).toContainText("Changes are unavailable here.");
    await expect(ws.getByRole("textbox", { name: "Path prefix" })).toHaveCount(0);
    await expect(ws.getByRole("button", { name: "Project", exact: true })).toHaveCount(0);
    await expect(ws.getByRole("button", { name: "Save correction" })).toHaveCount(0);

    expect(mutationRequests).toEqual([]);
  });

  test("rules view is read-only without actions or the mapping form", async ({ page }) => {
    test.skip(!isDuckDBBackend, "runs only against duckdb serve");

    // Same version-hydration gate as above: the read-only notice must come
    // from the backend's read_only flag, not the null pre-hydration state.
    const versionPromise = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/version" && response.ok(),
    );
    await page.goto("/data?view=rules");
    const versionResponse = await versionPromise;
    expect(await versionResponse.json()).toMatchObject({ read_only: true });
    await expect(page.getByRole("heading", { name: "Worktree mappings" })).toBeVisible();
    await expect(page.getByRole("note")).toContainText("This store is read-only.");
    await expect(page.getByRole("button", { name: "Add mapping" })).toHaveCount(0);
    await expect(page.getByRole("columnheader", { name: "Actions" })).toHaveCount(0);
    await expect(page.getByText("No worktree mappings configured.")).toBeVisible();
  });
});
