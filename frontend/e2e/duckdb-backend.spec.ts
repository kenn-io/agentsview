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
    await expect(sp.sessionItems).toHaveCount(12);
    await expect(sp.sessionListHeader).toContainText("12 sessions");
  });

  // Listing sessions exercises none of the message path, which is how
  // v0.42.0 shipped with every duckdb serve transcript blank: the messages
  // response failed to marshal and the connection was dropped
  // (ERR_EMPTY_RESPONSE), while the session list kept rendering fine.
  test("renders a session transcript", async ({ page }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();
    await expect(sp.messageRows.first()).toBeVisible();
  });

  // The same failure at the API boundary, where it is unambiguous: a
  // dropped connection surfaces as a request error rather than a status.
  test("serves session messages over the API", async ({ page, request }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const id = await page.evaluate(() => location.pathname.split("/").pop());
    expect(id).toBeTruthy();

    const res = await request.get(`/api/v1/sessions/${id}/messages`);
    expect(res.status()).toBe(200);
    const body = await res.json();
    expect(Array.isArray(body.messages)).toBe(true);
    expect(body.messages.length).toBeGreaterThan(0);
  });
});
