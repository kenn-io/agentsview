import { test, expect } from "./helpers/isolated-server.js";

const session = "test-session-file-references";
const endpoint = `/api/v1/sessions/${session}/file-references`;
const intent = { "X-AgentsView-Lookup-Intent": "docbank" };

test("retained files render during Docbank outage, retry and paginate without crossing sessions", async ({
  page,
  request,
}) => {
  test.setTimeout(60_000);
  if (process.env.AGENTSVIEW_E2E_BACKEND === "duckdb") {
    const result = await request.get("/api/v1/sessions/test-session-small-2/file-references", {
      headers: intent,
    });
    expect(result.ok()).toBe(true);
    expect((await result.json()).capability).toBe("unavailable");
    return;
  }
  const discovery = await request.get(`${endpoint}?first_ordinal=0&last_ordinal=0`, {
    headers: intent,
  });
  expect(discovery.ok()).toBe(true);
  const first = await discovery.json();
  const remote = new URL(first.occurrences[0].matches[0].open_url).origin;
  const messages = await request.get(`/api/v1/sessions/${session}/messages`);
  const loaded = await messages.json();
  const written = loaded.file_evidence
    .flatMap((group: { records: Array<{ display_name: string; sha256: string }> }) => group.records)
    .find((record: { display_name: string }) => record.display_name === "written.txt");
  expect(written).toBeDefined();
  const control = async (mode: string) => {
    const result = await request.post(
      `${remote}/__test__/state?mode=${mode}&sha256=${written.sha256}`,
    );
    expect(result.ok()).toBe(true);
  };
  const waiting = async () =>
    (await (await request.get(`${remote}/__test__/state`)).json()).waiting;
  await control("hold");
  try {
    await page.goto(`/sessions/${session}`, { waitUntil: "domcontentloaded" });
    await expect(page.getByText("written.txt", { exact: true }).first()).toBeVisible();
    await expect.poll(waiting).toBeGreaterThan(0);
    // A held external request must not hold the ordinary message API or DOM.
    expect((await request.get(`/api/v1/sessions/${session}/messages`)).ok()).toBe(true);
    await expect.poll(waiting).toBeGreaterThan(0);
    await expect(page.getByText("Docbank unavailable", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Check remaining", exact: true }).click();
    await expect.poll(waiting).toBeGreaterThan(0);
    await page.goto("/sessions/test-session-small-2", { waitUntil: "domcontentloaded" });
    await control("release");
    await expect(page.locator('a[href*="#node="]')).toHaveCount(0);
    await page.goto(`/sessions/${session}`, { waitUntil: "domcontentloaded" });
    const retry = page.getByRole("button", { name: "Check remaining", exact: true });
    if (await retry.isVisible()) await retry.click();
    await expect(page.getByRole("link", { name: /^Docbank · Current ·/ }).first()).toBeVisible();
    await expect(
      page.getByRole("link", { name: /^Docbank · Prior version ·/ }).first(),
    ).toBeVisible();
    await expect(page.getByRole("link", { name: /^Docbank · Trash ·/ }).first()).toBeVisible();
    await expect(page.getByText("Not found in Docbank", { exact: true }).first()).toBeVisible();
    const prior = page.locator('a[href*="#node=2&version=00000000-0000-4000-8000-000000000002"]');
    await expect(prior).toBeVisible();
    await expect(prior).toHaveAttribute("rel", /noreferrer/);
    await page.getByRole("button", { name: "More matches", exact: true }).click();
    await expect(
      page.locator('a[href*="#node=12&version=00000000-0000-4000-8000-000000000012"]'),
    ).toBeVisible();

    // Replace the real fixture archive while an actual lookup is held.
    await control("hold");
    const pending = request.get(
      `${endpoint}?binding=${encodeURIComponent(first.binding)}&refresh=true&first_ordinal=1&last_ordinal=1`,
      { headers: intent },
    );
    await expect.poll(waiting).toBeGreaterThan(0);
    await control("revise");
    await control("release");
    expect((await pending).status()).toBe(409);
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByText("Fixture transcript replaced.", { exact: true })).toBeVisible();
    await expect(page.locator('a[href*="#node="]')).toHaveCount(0);
  } finally {
    await control("release");
    await control("reset");
  }
});
