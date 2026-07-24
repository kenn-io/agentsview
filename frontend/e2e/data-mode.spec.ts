import { expect, test, type Page } from "@playwright/test";

const isDuckDBBackend = process.env.AGENTSVIEW_E2E_BACKEND === "duckdb";
const wrongProject = "wrong_branch_label";
const targetProject = "sample_service";
const machine = "remote-example-host";
const worktreeRoot =
  "/srv/worktrees/github.com/example-org/sample-service/example-worktree";
const broaderPrefix = "/srv/worktrees/github.com/example-org/sample-service";

function workspace(page: Page) {
  return page.locator("section.workspace");
}

test.describe("Data mode project reclassification", () => {
  test.skip(
    ({ browserName }) => browserName !== "chromium",
    "the workflow mutates the shared fixture once",
  );

  test("reclassifies a worktree from Activity through Data and persists a rule", async ({
    page,
  }) => {
    test.skip(isDuckDBBackend, "requires the writable SQLite archive");

    let previewRequests = 0;
    let reclassifyMutations = 0;
    const legacyCandidateRequests: string[] = [];
    page.on("request", (request) => {
      const pathname = new URL(request.url()).pathname;
      if (
        request.method() === "POST" &&
        pathname === "/api/v1/settings/worktree-mappings/preview"
      ) {
        previewRequests += 1;
      }
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

    await page.goto("/activity?window_days=40");
    // The breakdown project link's visible text is the project name itself;
    // "View {project} in Data" lives only in its title attribute, which the
    // accessible name computation ignores once the link has text content.
    const link = page.getByTitle(`View ${wrongProject} in Data`);
    await expect(link).toBeVisible();
    await link.click();

    await expect(page).toHaveURL(/\/data\?.*project_key=/);
    await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();
    const ws = workspace(page);
    await expect(ws.getByRole("heading", { name: wrongProject })).toBeVisible();
    await expect(
      ws.getByText(`Originally shown as ${wrongProject}`),
    ).toBeVisible();

    const summary = ws.locator(".candidate-summary");
    await expect(summary).toContainText(machine);
    await expect(summary).toContainText("2 sessions");

    const prefix = ws.getByRole("textbox", { name: "Path prefix" });
    await expect(prefix).toHaveValue(worktreeRoot);
    await prefix.fill(broaderPrefix);

    await ws.getByTitle("Target project").click();
    const targetInput = ws.getByRole("combobox");
    await targetInput.fill(targetProject);
    await page
      .getByRole("option", { name: `Use project "${targetProject}"` })
      .click();

    await expect.poll(() => previewRequests).toBe(1);
    await expect(ws.getByText("Full archive impact")).toBeVisible();
    await expect(
      ws.getByText("2 sessions matched", { exact: true }),
    ).toBeVisible();
    await expect(
      ws.getByText("2 sessions will change", { exact: true }),
    ).toBeVisible();
    await expect(ws.getByText("1 project", { exact: true })).toBeVisible();

    await ws.getByRole("button", { name: "Apply reclassification" }).click();

    // Explicit inventory reload; selection follows the applied target.
    await expect(
      ws.getByRole("heading", { name: targetProject }),
    ).toBeVisible();
    expect(reclassifyMutations).toBe(1);
    await expect(page.getByRole("row", { name: targetProject })).toBeVisible();
    await expect(page.getByRole("row", { name: wrongProject })).toHaveCount(0);

    await page
      .locator('[aria-label="Data view"]')
      .getByText("Rules", { exact: true })
      .click();
    await expect(
      page.getByRole("heading", { name: "Worktree mappings" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Select machine" }).click();
    await page.getByRole("option", { name: machine, exact: true }).click();

    const rule = page.locator("tr.rule-row");
    await expect(rule).toHaveCount(1);
    await expect(rule).toContainText(broaderPrefix);
    await expect(rule.getByRole("button", { name: targetProject })).toBeVisible();
    await expect(rule).toContainText(wrongProject);
    await expect(rule).toContainText("On");

    expect(legacyCandidateRequests).toEqual([]);
  });

  test("stops at the editor read-only notice without offering mutations", async ({
    page,
  }) => {
    test.skip(!isDuckDBBackend, "runs only against duckdb serve");

    const mutationRequests: string[] = [];
    page.on("request", (request) => {
      const pathname = new URL(request.url()).pathname;
      if (
        request.method() !== "GET" &&
        pathname.startsWith("/api/v1/settings/worktree-mappings")
      ) {
        mutationRequests.push(`${request.method()} ${pathname}`);
      }
    });

    // Wait for version hydration: DataPage also renders read-only while
    // sync.serverVersion is still null, so the assertions below must run
    // against the backend's real read_only flag, not the pre-hydration state.
    const versionPromise = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === "/api/v1/version" && response.ok(),
    );
    await page.goto("/data");
    const versionResponse = await versionPromise;
    expect(await versionResponse.json()).toMatchObject({ read_only: true });
    await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();
    const row = page.getByRole("row", { name: wrongProject });
    await expect(row).toBeVisible();
    await row.click();

    const ws = workspace(page);
    await expect(
      ws.getByText(`Originally shown as ${wrongProject}`),
    ).toBeVisible();
    await expect(ws.getByRole("note")).toContainText(
      "This store is read-only.",
    );
    await expect(
      ws.getByRole("textbox", { name: "Path prefix" }),
    ).toHaveCount(0);
    await expect(
      ws.getByRole("button", { name: "Apply reclassification" }),
    ).toHaveCount(0);

    expect(mutationRequests).toEqual([]);
  });

  test("rules view is read-only without actions or the mapping form", async ({
    page,
  }) => {
    test.skip(!isDuckDBBackend, "runs only against duckdb serve");

    // Same version-hydration gate as above: the read-only notice must come
    // from the backend's read_only flag, not the null pre-hydration state.
    const versionPromise = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === "/api/v1/version" && response.ok(),
    );
    await page.goto("/data?view=rules");
    const versionResponse = await versionPromise;
    expect(await versionResponse.json()).toMatchObject({ read_only: true });
    await expect(
      page.getByRole("heading", { name: "Worktree mappings" }),
    ).toBeVisible();
    await expect(page.getByRole("note")).toContainText(
      "This store is read-only.",
    );
    await expect(
      page.getByRole("button", { name: "Add mapping" }),
    ).toHaveCount(0);
    await expect(
      page.getByRole("columnheader", { name: "Actions" }),
    ).toHaveCount(0);
    await expect(
      page.getByText("No worktree mappings configured."),
    ).toBeVisible();
  });
});
