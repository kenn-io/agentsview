import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

const isDuckDBBackend = process.env.AGENTSVIEW_E2E_BACKEND === "duckdb";

test.describe("DuckDB backend", () => {
  test.skip(!isDuckDBBackend, "runs only against duckdb serve");

  test("serves fixture sessions in read-only mode", async ({
    page,
    request,
 }) => {
    const version = await request.get("/api/v1/version");
    expect(version.ok()).toBeTruthy();
    expect(await version.json()).toMatchObject({ read_only: true });

    const sp = new SessionsPage(page);
    await sp.goto();
    await expect(sp.sessionItems).toHaveCount(9);
    await expect(sp.sessionListHeader).toContainText("9 sessions");
  });
});
