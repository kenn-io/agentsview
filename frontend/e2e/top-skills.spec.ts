import { expect, test } from "@playwright/test";

const skills = {
  total_skill_calls: 12,
  distinct_skills: 2,
  by_skill: [
    {
      skill_name: "commit",
      call_count: 9,
      session_count: 4,
      agent_breakdown: [
        { agent: "codex", count: 6 },
        { agent: "claude", count: 3 },
      ],
      project_breakdown: [{ project: "project-alpha", count: 9 }],
      last_used_at: "2024-01-15T12:00:00Z",
      pct: 75,
    },
    {
      skill_name: "review",
      call_count: 3,
      session_count: 2,
      agent_breakdown: [{ agent: "claude", count: 3 }],
      project_breakdown: [{ project: "project-beta", count: 3 }],
      last_used_at: "2024-01-14T12:00:00Z",
      pct: 25,
    },
  ],
  trend: [],
};

test("Top skills rows open their breakdown from the keyboard", async ({ page }) => {
  await page.route("**/api/v1/analytics/skills*", (route) => route.fulfill({ json: skills }));
  await page.goto("/sessions");

  const table = page.getByRole("table", { name: "Top skills" });
  await expect(table.getByRole("columnheader")).toHaveText([
    "Skill",
    "Share",
    "Calls",
    "Sessions",
    "Last used",
  ]);

  const commit = table.getByRole("button", { name: "commit" });
  const review = table.getByRole("button", { name: "review" });
  await expect(commit).toHaveAttribute("aria-expanded", "false");
  await expect(review).toHaveAttribute("aria-expanded", "false");
  await expect(table.getByText("project-alpha")).toBeHidden();

  await commit.focus();
  await page.keyboard.press("Enter");
  await expect(commit).toHaveAttribute("aria-expanded", "true");
  const details = page.locator(`[id="${await commit.getAttribute("aria-controls")}"]`);
  await expect(details).toBeVisible();
  await expect(details.getByRole("listitem")).toHaveText([/codex\s*6\s*67%/, /claude\s*3\s*33%/, /project-alpha\s*9/]);
  // Opening one row leaves the others closed.
  await expect(review).toHaveAttribute("aria-expanded", "false");

  // Tab moves to the next row's disclosure button, which shows a focus ring.
  await page.keyboard.press("Tab");
  await expect(review).toBeFocused();
  expect(await review.evaluate((el) => getComputedStyle(el).outlineStyle)).not.toBe("none");
  await page.keyboard.press("Space");
  await expect(review).toHaveAttribute("aria-expanded", "true");

  await commit.focus();
  await page.keyboard.press("Space");
  await expect(commit).toHaveAttribute("aria-expanded", "false");
  await expect(details).toBeHidden();
});
