import { expect, test } from "@playwright/test";
import { serveTranscript, transcriptMessage } from "./helpers/mock-transcript";

const sessionId = "test-session-xlarge-5500";
const fence = "`".repeat(3);
const content = [
  "Inline: $e^{i\\pi}+1=0$ and \\(\\alpha^2+\\beta^2=\\gamma^2\\).",
  "$$\n\\sum_{k=1}^{n} k = \\frac{n(n+1)}{2}\n$$",
  "\\[\n\\begin{aligned}\n\\nabla\\cdot\\mathbf{E}&=\\frac{\\rho}{\\varepsilon_0}\\\\\n\\nabla\\times\\mathbf{B}&=\\mu_0\\mathbf{J}\n\\end{aligned}\n\\]",
  "\\begin{equation*}\\int_0^1 x^2\\,dx=\\frac{1}{3}\\end{equation*}",
  fence + "math\n\\begin{pmatrix}a&b\\\\c&d\\end{pmatrix}\n" + fence,
  "$$\n" + Array.from({ length: 70 }, () => "x").join("+") + "\n$$",
  "Prices: $5 and $10. Literal: " + "`" + "$x$" + "`",
].join("\n\n");

test("renders model math formats with local fonts and scrolls wide equations", async ({
  page,
}, info) => {
  const externalRequests: string[] = [];
  await page.route(/^https?:\/\/(?!127\.0\.0\.1[:/]|localhost[:/])/, async (route) => {
    externalRequests.push(route.request().url());
    await route.abort();
  });
  await serveTranscript(page, sessionId, [
    transcriptMessage(sessionId, 0, { content, content_length: content.length }),
  ]);
  await page.addInitScript(() => localStorage.setItem("agentsview-message-layout", "default"));
  await page.goto("/sessions/" + sessionId);
  await expect(page.locator(".katex")).toHaveCount(7);
  await expect(page.locator(".katex-display")).toHaveCount(5);
  await expect(page.locator(".message-body")).toContainText("Prices: $5 and $10.");
  await expect(page.locator(".markdown code")).toHaveText("$x$");
  await page.evaluate(async () => {
    await document.fonts.ready;
  });
  expect(await page.evaluate(() => document.fonts.check("16px KaTeX_Main"))).toBe(true);
  expect(
    await page.evaluate(() =>
      Array.from(document.fonts).some(
        (font) => font.family === "KaTeX_Main" && font.status === "loaded",
      ),
    ),
  ).toBe(true);
  const wide = page.locator(".katex-display").last();
  expect(
    await wide.evaluate((el) => ({
      scrolls: el.scrollWidth > el.clientWidth,
      overflow: getComputedStyle(el).overflowX,
    })),
  ).toEqual({ scrolls: true, overflow: "auto" });
  expect(externalRequests.filter((url) => /katex|woff|ttf/i.test(url))).toEqual([]);
  await page.screenshot({ path: info.outputPath("math.png") });
});
