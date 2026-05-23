import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";
import {
  waitForStableValue,
  waitForRowCountStable,
} from "./helpers/virtual-list-helpers";

test.describe("Message loading", () => {
  test("clicking session shows messages", async ({ page }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();
  });

  test("no request spam on session click", async ({ page }) => {
    const messageRequests: string[] = [];
    page.on("request", (req) => {
      if (req.url().includes("/messages")) {
        messageRequests.push(req.url());
      }
    });

    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    // Wait for at least one message request to have fired
    await expect
      .poll(() => messageRequests.length, { timeout: 5_000 })
      .toBeGreaterThan(0);

    // Wait for requests to stop firing
    await waitForStableValue(() => messageRequests.length, 500);

    // For large sessions we may fetch several pages while loading
    // into memory. With the reactive loop bug, this would be
    // dozens of parallel requests.
    expect(messageRequests.length).toBeLessThanOrEqual(15);
  });

  test("small session loads fast", async ({ page }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectLastSession();
  });

  test(
    "large session shows first page quickly",
    async ({ page }) => {
      const sp = new SessionsPage(page);
      await sp.goto();

      // First session is the largest (5500 messages)
      await sp.sessionItems.first().click();

      // First page should render within 3s
      await expect(sp.messageRows.first()).toBeVisible({
        timeout: 3_000,
      });
    },
  );

  test(
    "scroll does not reset to top during loading",
    async ({ page }) => {
      const sp = new SessionsPage(page);
      await sp.goto();
      await sp.selectFirstSession();

      // Wait for progressive loading to finish by polling
      // the message row count until it stabilizes.
      await waitForRowCountStable(sp);

      // Scroll down
      await sp.scroller.evaluate((el) => {
        el.scrollTop = 3000;
      });

      // Wait for scroll position to settle
      await expect
        .poll(
          () => sp.scroller.evaluate((el) => el.scrollTop),
          { timeout: 2_000 },
        )
        .toBeGreaterThan(500);
    },
  );

  test("follow latest scrolls down and exits on manual wheel", async ({
    page,
  }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const follow = page.getByLabel("Follow latest messages");
    await expect(follow).toHaveAttribute("aria-pressed", "false");

    await sp.scroller.evaluate((el) => {
      el.scrollTop = el.scrollHeight;
      el.dispatchEvent(new Event("scroll"));
    });
    await expect
      .poll(
        () =>
          sp.scroller.evaluate(
            (el) =>
              el.scrollHeight - el.clientHeight - el.scrollTop,
          ),
        { timeout: 2_000 },
      )
      .toBeLessThanOrEqual(8);

    await follow.click();

    await expect
      .poll(
        () =>
          sp.scroller.evaluate(
            (el) =>
              el.scrollHeight - el.clientHeight - el.scrollTop,
          ),
        { timeout: 2_000 },
      )
      .toBeLessThanOrEqual(8);
    await expect(follow).toHaveAttribute("aria-pressed", "true");

    await sp.scroller.hover();
    await page.mouse.wheel(0, -1800);

    await expect(follow).toHaveAttribute("aria-pressed", "false");
  });

  test("follow latest exits on pointer touch and keyboard scroll intent", async ({
    page,
  }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const follow = page.getByLabel("Follow latest messages");
    const cases = ["pointerdown", "touchmove", "keydown"];

    for (const item of cases) {
      await follow.click();
      await expect(follow, item).toHaveAttribute(
        "aria-pressed",
        "true",
      );

      await sp.scroller.evaluate((el, dispatchName) => {
        if (dispatchName === "pointerdown") {
          el.dispatchEvent(
            new PointerEvent("pointerdown", {
              bubbles: true,
              pointerType: "mouse",
            }),
          );
        } else if (dispatchName === "touchmove") {
          el.dispatchEvent(
            new Event("touchmove", { bubbles: true }),
          );
        } else {
          el.dispatchEvent(
            new KeyboardEvent("keydown", {
              bubbles: true,
              key: "PageUp",
            }),
          );
        }
      }, item);

      await expect(follow, item).toHaveAttribute(
        "aria-pressed",
        "false",
      );
    }
  });

  test("follow latest exits on global keyboard message navigation", async ({
    page,
  }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const follow = page.getByLabel("Follow latest messages");
    await follow.click();
    await expect(follow).toHaveAttribute("aria-pressed", "true");

    await page.keyboard.press("ArrowUp");

    await expect(follow).toHaveAttribute("aria-pressed", "false");
    await expect
      .poll(() => sp.scroller.evaluate((el) => el.scrollTop), {
        timeout: 2_000,
      })
      .toBeLessThan(1_000);
  });

  test("follow latest settles after a tall final message is measured", async ({
    page,
  }) => {
    await page.route(
      "**/api/v1/sessions/test-session-xlarge-5500/messages*",
      async (route) => {
        const now = new Date().toISOString();
        const messages = Array.from(
          { length: 1000 },
          (_, i) => {
            const ordinal = 4500 + i;
            const isLast = ordinal === 5499;
            const content = isLast
              ? Array.from(
                  { length: 120 },
                  (_, n) =>
                    `Final response paragraph ${n}. This line makes the final message much taller than the virtualizer estimate.`,
                ).join("\n\n")
              : `Message ${ordinal}`;
            return {
              id: ordinal,
              session_id: "test-session-xlarge-5500",
              ordinal,
              role: ordinal % 2 === 0 ? "user" : "assistant",
              content,
              timestamp: now,
              has_thinking: false,
              thinking_text: "",
              has_tool_use: false,
              content_length: content.length,
              model: "",
              token_usage: null,
              context_tokens: 0,
              output_tokens: 0,
              has_context_tokens: false,
              has_output_tokens: false,
              tool_calls: [],
              is_system: false,
            };
          },
        );

        await route.fulfill({
          json: {
            messages: [...messages].reverse(),
            count: messages.length,
          },
        });
      },
    );

    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    await page.getByLabel("Follow latest messages").click();

    await expect
      .poll(
        () =>
          sp.scroller.evaluate(
            (el) =>
              el.scrollHeight - el.clientHeight - el.scrollTop,
          ),
        { timeout: 3_000 },
      )
      .toBeLessThanOrEqual(8);
  });

  test("follow latest remains pinned when final content grows after load", async ({
    page,
  }) => {
    await page.route("**/slow-follow-image.svg", async (route) => {
      await new Promise((resolve) => setTimeout(resolve, 600));
      await route.fulfill({
        contentType: "image/svg+xml",
        body: `<svg xmlns="http://www.w3.org/2000/svg" width="800" height="2400">
          <rect width="800" height="2400" fill="#dbeafe"/>
          <text x="40" y="120" font-size="48">slow final content</text>
        </svg>`,
      });
    });

    await page.route(
      "**/api/v1/sessions/test-session-xlarge-5500/messages*",
      async (route) => {
        const now = new Date().toISOString();
        const messages = Array.from(
          { length: 1000 },
          (_, i) => {
            const ordinal = 4500 + i;
            const isLast = ordinal === 5499;
            const content = isLast
              ? "Final response before image.\n\n![slow final content](/slow-follow-image.svg)"
              : `Message ${ordinal}`;
            return {
              id: ordinal,
              session_id: "test-session-xlarge-5500",
              ordinal,
              role: ordinal % 2 === 0 ? "user" : "assistant",
              content,
              timestamp: now,
              has_thinking: false,
              thinking_text: "",
              has_tool_use: false,
              content_length: content.length,
              model: "",
              token_usage: null,
              context_tokens: 0,
              output_tokens: 0,
              has_context_tokens: false,
              has_output_tokens: false,
              tool_calls: [],
              is_system: false,
            };
          },
        );

        await route.fulfill({
          json: {
            messages: [...messages].reverse(),
            count: messages.length,
          },
        });
      },
    );

    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    await page.getByLabel("Follow latest messages").click();
    await page.locator(".message-list-scroll img").waitFor({
      state: "visible",
      timeout: 3_000,
    });

    await expect
      .poll(
        () =>
          sp.scroller.evaluate(
            (el) =>
              el.scrollHeight - el.clientHeight - el.scrollTop,
          ),
        { timeout: 3_000 },
      )
      .toBeLessThanOrEqual(8);
  });

  test("follow latest stays enabled through non-user scroll drift", async ({
    page,
  }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const follow = page.getByLabel("Follow latest messages");
    await follow.click();
    await expect(follow).toHaveAttribute("aria-pressed", "true");

    await page.waitForTimeout(1100);
    await sp.scroller.evaluate((el) => {
      el.scrollTop = 0;
      el.dispatchEvent(new Event("scroll"));
    });

    await expect(follow).toHaveAttribute("aria-pressed", "true");
  });

  test("follow latest button toggles off", async ({ page }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const follow = page.getByLabel("Follow latest messages");
    await expect(follow).toHaveAttribute("aria-pressed", "false");
    await follow.click();
    await expect(follow).toHaveAttribute("aria-pressed", "true");

    await follow.click();
    await expect(follow).toHaveAttribute("aria-pressed", "false");
  });

  test("follow latest toggle-off cancels queued scroll work", async ({
    page,
  }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const follow = page.getByLabel("Follow latest messages");
    await sp.scroller.evaluate((el) => {
      el.scrollTop = 0;
      el.dispatchEvent(new Event("scroll"));
    });
    await expect
      .poll(() => sp.scroller.evaluate((el) => el.scrollTop), {
        timeout: 2_000,
      })
      .toBeLessThanOrEqual(8);

    await page.evaluate(() => {
      type HeldRaf = {
        pending: () => number;
        flush: () => void;
        restore: () => void;
      };
      const win = window as Window & {
        __followRaf?: HeldRaf;
      };
      const originalRequest =
        window.requestAnimationFrame.bind(window);
      const originalCancel =
        window.cancelAnimationFrame.bind(window);
      let nextId = 1;
      const callbacks = new Map<number, FrameRequestCallback>();

      win.__followRaf = {
        pending: () => callbacks.size,
        flush: () => {
          const pending = [...callbacks.values()];
          callbacks.clear();
          for (const cb of pending) {
            cb(performance.now());
          }
        },
        restore: () => {
          window.requestAnimationFrame = originalRequest;
          window.cancelAnimationFrame = originalCancel;
        },
      };
      window.requestAnimationFrame = (
        cb: FrameRequestCallback,
      ) => {
        const id = nextId;
        nextId += 1;
        callbacks.set(id, cb);
        return id;
      };
      window.cancelAnimationFrame = (id: number) => {
        callbacks.delete(id);
      };
    });

    await follow.click();
    await expect
      .poll(
        () =>
          page.evaluate(
            () =>
              (
                window as Window & {
                  __followRaf: { pending: () => number };
                }
              ).__followRaf.pending(),
          ),
        { timeout: 2_000 },
      )
      .toBeGreaterThan(0);

    await follow.click();
    await expect(follow).toHaveAttribute("aria-pressed", "false");

    await page.evaluate(() => {
      (
        window as Window & {
          __followRaf: { flush: () => void; restore: () => void };
        }
      ).__followRaf.flush();
      (
        window as Window & {
          __followRaf: { flush: () => void; restore: () => void };
        }
      ).__followRaf.restore();
    });

    await expect
      .poll(() => sp.scroller.evaluate((el) => el.scrollTop), {
        timeout: 2_000,
      })
      .toBeLessThanOrEqual(8);
  });
});
