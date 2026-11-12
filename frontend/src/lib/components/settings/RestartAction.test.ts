import { cleanup, render } from "@testing-library/svelte";
import { tick } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { settings } from "../../stores/settings.svelte.js";
import RestartAction from "./RestartAction.svelte";

beforeEach(() => {
  settings.restartSupported = true;
  settings.restartFailed = false;
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
  it("tells a server that can't restart itself how to restart", () => {
    settings.restartSupported = false;

    const { getByRole, queryByRole } = render(RestartAction, {
      notice: "Example notice",
      manual: true,
    });

    expect(queryByRole("button", { name: /Restart/ })).toBeNull();
    expect(getByRole("status").textContent).toContain(
      "Restart AgentsView the way you started it, then reload this page.",
    );
  });

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

  it("shows manual recovery without another restart after the deadline", () => {
    settings.restartFailed = true;
    const { getByRole, getByText, queryByRole } = render(RestartAction, {
      notice: "Example notice",
      manual: true,
    });
    expect(getByRole("status").textContent).toContain("has not reconnected");
    expect(getByText(/restart AgentsView the way you started it/i)).toBeTruthy();
    expect(getByText(/open the address/i)).toBeTruthy();
    expect(queryByRole("button", { name: "Restart now" })).toBeNull();
    expect(getByRole("button", { name: "Check connection" })).toBeTruthy();
  });
});
