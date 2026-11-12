import { cleanup, render } from "@testing-library/svelte";
import { tick } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { settings } from "../../stores/settings.svelte.js";
import RestartAction from "./RestartAction.svelte";

beforeEach(() => {
  settings.restartSupported = true;
  settings.restartFailed = false;
  settings.restartCanRetry = false;
  settings.restartError = null;
  settings.restarting = false;
  settings.saving = false;
  settings.requireAuth = false;
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("RestartAction", () => {
  it.each([true, false])(
    "shows manual guidance for an unsupported server when manual is %s",
    (manual) => {
      settings.restartSupported = false;
      const { getByRole, queryByRole } = render(RestartAction, {
        notice: "Example notice",
        manual,
      });
      expect(queryByRole("button", { name: /Restart/ })).toBeNull();
      if (manual)
        expect(getByRole("status").textContent).toContain(
          "Restart AgentsView the way you started it",
        );
      else expect(getByRole("status").textContent?.trim()).toBe("Example notice");
    },
  );

  it("shows the recovery commands when restart fails", () => {
    settings.restartFailed = true;
    settings.restartError = "restart failed.";
    const { getByRole } = render(RestartAction, { notice: "Example notice", manual: true });
    expect(getByRole("button", { name: "Restart now" })).toBeTruthy();
    const alert = getByRole("alert");
    expect(alert.textContent).toContain("couldn't restart from this page");
    expect(alert.textContent).toContain("Reason: restart failed.");
    expect(alert.textContent).not.toContain("failed..");
    expect(alert.textContent).toContain(
      "Run agentsview serve status and open the address it prints.",
    );
  });

  it("mentions signing in again only when auth is on", async () => {
    const { getByRole } = render(RestartAction, { notice: "Example notice", manual: true });
    expect(getByRole("status").textContent).not.toContain("sign in again");

    settings.requireAuth = true;
    await tick();
    expect(getByRole("status").textContent).toContain("You may need to sign in again.");
  });

  it.each([false, true])("shows recovery with retry availability %s", (canRetry) => {
    settings.restartFailed = true;
    settings.restartCanRetry = canRetry;
    const { getByRole, getByText, queryByRole } = render(RestartAction, {
      notice: "Example notice",
    });
    expect(getByText(/restart AgentsView the way you started it/i)).toBeTruthy();
    expect(getByText(/open the address/i)).toBeTruthy();
    expect(Boolean(queryByRole("button", { name: "Restart now" }))).toBe(canRetry);
    expect(getByRole("button", { name: "Check connection" })).toBeTruthy();
    if (!canRetry) expect(getByRole("status").textContent).toContain("has not reconnected");
  });
});
