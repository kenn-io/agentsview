import { test, expect, type Page, type Response } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

const TELEMETRY_PATH = "/api/v1/telemetry/events";
const CANNED_KINDS = [
  "prompt_maturity_review",
  "context_setup_review",
  "workflow_hygiene_review",
  "tool_reliability_review",
  "model_cost_review",
  "instruction_opportunity_review",
];

// Register before acting: resolves with the daemon's answer to the first matching telemetry POST.
function telemetryResponse(page: Page, event: string): Promise<Response> {
  return page.waitForResponse((res) => {
    const req = res.request();
    return (
      req.method() === "POST" &&
      new URL(req.url()).pathname === TELEMETRY_PATH &&
      req.postDataJSON()?.event === event
    );
  });
}

async function expectAccepted(response: Promise<Response>): Promise<Record<string, unknown>> {
  const res = await response;
  expect(res.status()).toBe(202);
  expect(await res.json()).toEqual({ status: "disabled" });
  return res.request().postDataJSON();
}

test.describe("core action telemetry", () => {
  test("analytics pages report analytics_viewed with their page", async ({ page }) => {
    const overview = telemetryResponse(page, "analytics_viewed");
    await page.goto("/");
    expect(await expectAccepted(overview)).toEqual({
      event: "analytics_viewed",
      properties: { page: "sessions" },
    });

    for (const name of ["usage", "activity", "trends", "quality"]) {
      const viewed = telemetryResponse(page, "analytics_viewed");
      await page.goto(`/${name}`);
      expect(await expectAccepted(viewed)).toEqual({
        event: "analytics_viewed",
        properties: { page: name },
      });
    }
  });

  test("opening a session reports session_viewed with its agent", async ({ page }) => {
    const sessionsPage = new SessionsPage(page);
    await sessionsPage.goto();
    const viewed = telemetryResponse(page, "session_viewed");
    await sessionsPage.sessionItems.first().click();
    const body = await expectAccepted(viewed);

    const sessionId = new URL(page.url()).pathname
      .split("/")
      .slice(2)
      .map(decodeURIComponent)
      .join(":");
    const detail = await page.request.get(`/api/v1/sessions/${encodeURIComponent(sessionId)}`);
    expect(detail.ok()).toBe(true);
    const { agent } = await detail.json();
    expect(body).toEqual({ event: "session_viewed", properties: { agent } });
  });

  test("an archive search reports search_run with its query type", async ({ page }) => {
    await new SessionsPage(page).goto();
    const searched = telemetryResponse(page, "search_run");
    await page.keyboard.press("ControlOrMeta+k");
    const input = page.locator(".palette-input");
    await expect(input).toBeVisible();
    await input.fill("test");
    expect(await expectAccepted(searched)).toEqual({
      event: "search_run",
      properties: { query_type: "text" },
    });
  });

  test("the session HTML export reports export_run html", async ({ page }) => {
    const sessionsPage = new SessionsPage(page);
    await sessionsPage.goto();
    await sessionsPage.selectFirstSession();
    const exported = telemetryResponse(page, "export_run");
    await page.getByRole("button", { name: "Export session" }).first().click();
    // The export opens in a new window, so watch the whole context for its request.
    const exportRequest = page
      .context()
      .waitForEvent("request", (req) => new URL(req.url()).pathname.endsWith("/export"));
    await page.getByText("Download HTML export").first().click();
    await exportRequest;
    expect(await expectAccepted(exported)).toEqual({
      event: "export_run",
      properties: { format: "html" },
    });
  });

  test("the analytics CSV export reports export_run csv", async ({ page }) => {
    const sessionsPage = new SessionsPage(page);
    await sessionsPage.goto();
    await expect(sessionsPage.exportBtn).toBeEnabled();
    const exported = telemetryResponse(page, "export_run");
    await sessionsPage.exportBtn.click();
    expect(await expectAccepted(exported)).toEqual({
      event: "export_run",
      properties: { format: "csv" },
    });
  });

  test("generating an insight reports insight_generated with its kind", async ({ page }) => {
    await page.route("**/api/v1/insights/generate", (route) =>
      route.fulfill({
        contentType: "text/event-stream",
        body: [
          "event: done",
          `data: ${JSON.stringify({
            id: 77,
            type: "llm_canned",
            date_from: "2026-05-01",
            date_to: "2026-05-26",
            project: null,
            agent: "claude",
            model: "test-model",
            prompt: null,
            content: "# Prompt Maturity",
            kind: "prompt_maturity_review",
            created_at: "2026-05-26T12:00:00Z",
          })}`,
          "",
          "",
        ].join("\n"),
      }),
    );
    await page.goto("/recall?tab=generated");
    const generated = telemetryResponse(page, "insight_generated");
    await page
      .getByRole("region", { name: "Generated insights" })
      .getByRole("button", { name: "Generate" })
      .click();
    const body = await expectAccepted(generated);
    expect(body.event).toBe("insight_generated");
    expect(CANNED_KINDS).toContain((body.properties as { kind: string }).kind);
  });
});
