import { expect, test, type Route } from "@playwright/test";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";

// Other E2E workers share a server, so restart a separate isolated instance.
test("retries a lost restart request against the real daemon", async ({
  page,
  browserName,
}, testInfo) => {
  test.skip(browserName !== "chromium");
  test.setTimeout(180_000);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "agentsview-restart-e2e-"));
  let server: ChildProcess | undefined;
  try {
    let binary = process.env.E2E_PREBUILT_SERVER;
    if (!binary) {
      binary = path.join(dir, "agentsview");
      const build = spawnSync("go", ["build", "-tags", "fts5", "-o", binary, "./cmd/agentsview"], {
        cwd: path.resolve(".."),
        encoding: "utf8",
        timeout: 120_000,
      });
      expect(build.status, build.stderr).toBe(0);
    }
    const port = await new Promise<number>((resolve, reject) => {
      const listener = net.createServer();
      listener.on("error", reject);
      listener.listen(0, "127.0.0.1", () => {
        const address = listener.address();
        if (!address || typeof address === "string") return reject(new Error("no port"));
        listener.close(() => resolve(address.port));
      });
    });
    const base = `http://127.0.0.1:${port}`;
    server = spawn(
      binary,
      [
        "serve",
        "--host=127.0.0.1",
        `--port=${port}`,
        "--no-sync",
        "--no-browser",
        "--no-update-check",
        "--require-auth=false",
      ],
      {
        env: {
          PATH: process.env.PATH,
          HOME: dir,
          XDG_CONFIG_HOME: path.join(dir, "config"),
          XDG_CACHE_HOME: path.join(dir, "cache"),
          XDG_STATE_HOME: path.join(dir, "state"),
          AGENTSVIEW_DATA_DIR: path.join(dir, "data"),
          AGENTSVIEW_TELEMETRY_ENABLED: "0",
        },
        stdio: "ignore",
      },
    );
    await expect
      .poll(
        async () => {
          try {
            return (await page.request.get(`${base}/api/ping`)).ok();
          } catch {
            return false;
          }
        },
        { timeout: 30_000 },
      )
      .toBe(true);
    const before = await (await page.request.get(`${base}/api/ping`)).json();
    await page.goto(`${base}/settings?panel=tool-result-images`);
    await expect(page.getByRole("heading", { name: "Tool-result images" })).toBeVisible();
    await page.getByRole("radio", { name: "Drop" }).click();
    await expect(page.getByRole("button", { name: "Restart now" })).toBeVisible();
    const abort = async (route: Route) => route.abort("failed");
    await page.route("**/api/v1/daemon/restart", abort);
    await page.getByRole("button", { name: "Restart now" }).click();
    await expect(page.getByRole("button", { name: "Check connection" })).toBeVisible({
      timeout: 35_000,
    });
    await expect(page.getByRole("button", { name: "Restart now" })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("restart-retry.png") });
    await page.unroute("**/api/v1/daemon/restart", abort);
    const reloaded = page.waitForEvent("load", { timeout: 35_000 });
    await page.getByRole("button", { name: "Restart now" }).click();
    await reloaded;
    await expect(page.getByRole("heading", { name: "Tool-result images" })).toBeVisible();
    await expect(page.getByRole("radio", { name: "Drop" })).toHaveAttribute("aria-checked", "true");
    const after = await (await page.request.get(`${base}/api/ping`)).json();
    expect(after.instance_id).not.toBe(before.instance_id);
  } finally {
    if (server && server.exitCode === null && server.signalCode === null) {
      const exited = new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error("server did not stop")), 10_000);
        server!.once("exit", () => {
          clearTimeout(timer);
          resolve();
        });
      });
      server.kill("SIGTERM");
      await exited;
    }
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
